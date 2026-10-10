/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// fakeOwnershipClient simulates the comments, members and owners of
// existing roles and databases.
type fakeOwnershipClient struct {
	roles     map[string]string // role -> comment
	closures  map[string][]postgres.ReachableRole
	members   map[string][]postgres.RoleMember
	databases map[string]string // database -> comment
	dbOwners  map[string]string // database -> owner ("super" = a superuser)
	calls     []string
}

func (f *fakeOwnershipClient) RoleComment(_ context.Context, name string) (bool, string, error) {
	c, ok := f.roles[name]
	return ok, c, nil
}

func (f *fakeOwnershipClient) MembershipClosure(_ context.Context, name string) ([]postgres.ReachableRole, error) {
	return f.closures[name], nil
}

func (f *fakeOwnershipClient) RoleMembers(_ context.Context, name string) ([]postgres.RoleMember, error) {
	return f.members[name], nil
}

func (f *fakeOwnershipClient) DatabaseComment(_ context.Context, name string) (bool, string, error) {
	c, ok := f.databases[name]
	return ok, c, nil
}

func (f *fakeOwnershipClient) DatabaseOwner(_ context.Context, name string) (string, bool, error) {
	o := f.dbOwners[name]
	return o, o == "super", nil
}

func (f *fakeOwnershipClient) CommentOnDatabase(_ context.Context, name, comment string) error {
	f.calls = append(f.calls, fmt.Sprintf("comment %s %s", name, comment))
	f.databases[name] = comment
	return nil
}

func (f *fakeOwnershipClient) CreateDatabase(_ context.Context, name, owner string) error {
	f.calls = append(f.calls, fmt.Sprintf("create-or-alter %s owner=%s", name, owner))
	if _, ok := f.databases[name]; !ok {
		f.databases[name] = ""
	}
	return nil
}

func reasonOf(err error) string {
	if ce, ok := errors.AsType[*conditionError](err); ok {
		return ce.reason
	}
	return ""
}

// testTenant is a role owned by a tenant.
const testTenant = "tenant"

// forgedMarkers are comments a tenant could set without the Cluster's key.
func forgedMarkers(kind, name string) []string {
	valid := ownerMarker(kind, name)
	other := newMarkerSigner(&postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}},
		[]byte("another key, 32 bytes long......"))
	return []string{
		legacyMarker(kind, name),                     // the unsigned v1 text
		markerV2Prefix + kind + "/" + name + ":AAAA", // a v2 marker with a wrong HMAC
		other.marker(kind, name),                     // signed with another key
		valid[:len(valid)-1] + "x",                   // a valid marker, altered
		ownerMarker(kind, name+"x"),                  // another resource's marker
	}
}

var _ = Describe("Ownership of PostgreSQL roles and databases", func() {
	ctx := context.Background()
	rr := func() *RoleReconciler { return &RoleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()} }
	myMarker := ownerMarker(markerKindRole, grantTestRole)
	myDBMarker := ownerMarker(markerKindDatabase, grantTestRole)

	newRole := func(recorded string) *postgresv1alpha1.Role {
		r := &postgresv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: grantTestRole, Namespace: "ns", UID: "uid-me"}}
		r.Status.RoleName = recorded
		return r
	}
	check := func(f *fakeOwnershipClient, role *postgresv1alpha1.Role) (string, error) {
		return rr().checkRoleOwnership(ctx, f, role, grantTestRole, nil, testSigner)
	}

	Describe("markers", func() {
		It("are signed per Cluster, kind and name", func() {
			Expect(myMarker).To(HavePrefix("pgop:v2:Role/app:"))
			Expect(testSigner.verify(myMarker, markerKindRole, grantTestRole)).To(BeTrue())
			Expect(testSigner.verify(myMarker, markerKindDatabase, grantTestRole)).To(BeFalse())
			otherCluster := newMarkerSigner(&postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c2", Namespace: "ns"}},
				testSigner.key)
			Expect(otherCluster.verify(myMarker, markerKindRole, grantTestRole)).To(BeFalse())
			for _, forged := range forgedMarkers(markerKindRole, grantTestRole) {
				Expect(testSigner.verify(forged, markerKindRole, grantTestRole)).To(BeFalse(), forged)
			}
			Expect(markerSigner{}.verify(myMarker, markerKindRole, grantTestRole)).To(BeFalse(), "no key, no match")
		})
	})

	Describe("checkRoleOwnership", func() {
		It("creates a missing role with the marker", func() {
			marker, err := check(&fakeOwnershipClient{roles: map[string]string{}}, newRole(""))
			Expect(err).NotTo(HaveOccurred())
			Expect(marker).To(Equal(myMarker))
		})

		It("manages a role carrying its marker without re-marking it", func() {
			marker, err := check(&fakeOwnershipClient{roles: map[string]string{grantTestRole: myMarker}}, newRole(grantTestRole))
			Expect(err).NotTo(HaveOccurred())
			Expect(marker).To(BeEmpty())
		})

		It("re-marks a legacy role (no comment or v1 marker) recorded in status", func() {
			for _, comment := range []string{"", legacyMarker(markerKindRole, grantTestRole)} {
				marker, err := check(&fakeOwnershipClient{roles: map[string]string{grantTestRole: comment}}, newRole(grantTestRole))
				Expect(err).NotTo(HaveOccurred())
				Expect(marker).To(Equal(myMarker), comment)
			}
		})

		It("leaves a pre-existing DBA role alone", func() {
			_, err := check(&fakeOwnershipClient{roles: map[string]string{grantTestRole: ""}}, newRole(""))
			Expect(reasonOf(err)).To(Equal(ReasonRoleNotManaged))
			Expect(err.Error()).To(ContainSubstring("COMMENT ON ROLE app IS '" + myMarker + "'"))
		})

		It("refuses forged markers, with or without status", func() {
			for _, forged := range forgedMarkers(markerKindRole, grantTestRole) {
				_, err := check(&fakeOwnershipClient{roles: map[string]string{grantTestRole: forged}}, newRole(""))
				Expect(reasonOf(err)).To(Equal(ReasonRoleNotManaged), forged)
				if forged != legacyMarker(markerKindRole, grantTestRole) {
					_, err = check(&fakeOwnershipClient{roles: map[string]string{grantTestRole: forged}}, newRole(grantTestRole))
					Expect(reasonOf(err)).To(Equal(ReasonRoleNotManaged), forged)
				}
			}
		})

		It("refuses a hand-over while other roles are members of the role (e.g. its creator with ADMIN)", func() {
			f := &fakeOwnershipClient{
				roles:   map[string]string{grantTestRole: myMarker},
				members: map[string][]postgres.RoleMember{grantTestRole: {{Name: testTenant, Admin: true}, {Name: polDBA, Superuser: true}}},
			}
			_, err := check(f, newRole(""))
			Expect(reasonOf(err)).To(Equal(ReasonRolePolicyViolation))
			Expect(err.Error()).To(ContainSubstring("other roles are members of it (tenant)"))

			By("accepting it once only superusers are members")
			f.members[grantTestRole] = []postgres.RoleMember{{Name: polDBA, Superuser: true}}
			_, err = check(f, newRole(""))
			Expect(err).NotTo(HaveOccurred())
		})

		It("refuses a superuser role handed over with the marker", func() {
			f := &fakeOwnershipClient{
				roles:    map[string]string{grantTestRole: myMarker},
				closures: map[string][]postgres.ReachableRole{grantTestRole: {{Name: grantTestRole, Comment: myMarker, Superuser: true}}},
			}
			_, err := check(f, newRole(""))
			Expect(reasonOf(err)).To(Equal(ReasonRolePolicyViolation))
		})
	})

	Describe("ensureDatabase", func() {
		newDB := func(recorded string) *postgresv1alpha1.Database {
			d := &postgresv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: grantTestRole, Namespace: "ns", UID: "uid-me"}}
			d.Status.DatabaseName = recorded
			return d
		}
		ensure := func(f *fakeOwnershipClient, db *postgresv1alpha1.Database, owner string) error {
			return ensureDatabase(ctx, f, db, testSigner, grantTestRole, owner)
		}

		It("creates and marks a missing database", func() {
			f := &fakeOwnershipClient{databases: map[string]string{}}
			db := newDB("")
			Expect(ensure(f, db, "owner")).To(Succeed())
			Expect(f.calls).To(Equal([]string{"create-or-alter app owner=owner", "comment app " + myDBMarker}))
			Expect(db.Status.DatabaseName).To(Equal(grantTestRole))
		})

		It("updates the owner of its own database", func() {
			f := &fakeOwnershipClient{databases: map[string]string{grantTestRole: myDBMarker}}
			Expect(ensure(f, newDB(grantTestRole), "owner")).To(Succeed())
			Expect(f.calls).To(Equal([]string{"create-or-alter app owner=owner"}))
		})

		It("re-marks a legacy database (no comment or v1 marker) recorded in status", func() {
			for _, comment := range []string{"", legacyMarker(markerKindDatabase, grantTestRole)} {
				f := &fakeOwnershipClient{databases: map[string]string{grantTestRole: comment}}
				Expect(ensure(f, newDB(grantTestRole), "")).To(Succeed())
				Expect(f.calls).To(ContainElement("comment app "+myDBMarker), comment)
			}
		})

		It("does not take over a pre-existing database or one with a forged marker", func() {
			comments := append([]string{"", "reporting db"}, forgedMarkers(markerKindDatabase, grantTestRole)...)
			for _, comment := range comments {
				f := &fakeOwnershipClient{databases: map[string]string{grantTestRole: comment}, dbOwners: map[string]string{grantTestRole: testTenant}}
				db := newDB("")
				err := ensure(f, db, "owner")
				Expect(reasonOf(err)).To(Equal(ReasonDatabaseNotManaged), comment)
				Expect(f.calls).To(BeEmpty(), "no ALTER DATABASE ... OWNER for %q", comment)
				Expect(db.Status.DatabaseName).To(BeEmpty())
			}
		})

		It("refuses a validly marked database owned by someone other than a superuser or the declared owner", func() {
			f := &fakeOwnershipClient{databases: map[string]string{grantTestRole: myDBMarker}, dbOwners: map[string]string{grantTestRole: testTenant}}
			err := ensure(f, newDB(""), "owner")
			Expect(reasonOf(err)).To(Equal(ReasonDatabaseNotManaged))
			Expect(err.Error()).To(ContainSubstring("owned by tenant"))
			Expect(f.calls).To(BeEmpty())

			By("accepting the hand-over when a superuser or the declared owner owns it")
			for _, owner := range []string{"super", "owner"} {
				f = &fakeOwnershipClient{databases: map[string]string{grantTestRole: myDBMarker}, dbOwners: map[string]string{grantTestRole: owner}}
				Expect(ensure(f, newDB(""), "owner")).To(Succeed(), owner)
			}
		})
	})
})

// envtest: name collisions between resources and the namespace boundary.
var _ = Describe("Name collisions and namespaces", func() {
	const ns = "default"
	var (
		ctx    context.Context
		suffix string
	)
	BeforeEach(func() {
		ctx = context.Background()
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})
	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() {
			obj.SetFinalizers(nil)
			_ = k8sClient.Update(ctx, obj)
			_ = k8sClient.Delete(ctx, obj)
		})
	}

	It("refuses the later of two Roles resolving to the same PostgreSQL name on a Cluster", func() {
		pgName := "dup_" + suffix
		first := &postgresv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "dup-a-" + suffix, Namespace: ns},
			Spec: postgresv1alpha1.RoleSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: nonexistentCluster}, RoleName: pgName}}
		create(first)
		time.Sleep(1100 * time.Millisecond) // creation timestamps have second resolution
		second := first.DeepCopy()
		second.ObjectMeta = metav1.ObjectMeta{Name: "dup-b-" + suffix, Namespace: ns}
		create(second)
		otherCluster := first.DeepCopy()
		otherCluster.ObjectMeta = metav1.ObjectMeta{Name: "dup-c-" + suffix, Namespace: ns}
		otherCluster.Spec.ClusterRef.Name = "another-cluster"
		create(otherCluster)

		r := &RoleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Eventually(func(g Gomega) {
			g.Expect(r.checkRoleName(ctx, first, pgName)).To(Succeed())
			err := r.checkRoleName(ctx, second, pgName)
			g.Expect(reasonOf(err)).To(Equal(ReasonDuplicateRoleName))
			g.Expect(err.Error()).To(ContainSubstring(first.Name))
			g.Expect(r.checkRoleName(ctx, otherCluster, pgName)).To(Succeed(), "different Cluster, no collision")
		}).Should(Succeed())
	})

	It("refuses the later of two Databases resolving to the same PostgreSQL name on a Cluster", func() {
		pgName := "dupdb_" + suffix
		first := &postgresv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: "dupdb-a-" + suffix, Namespace: ns},
			Spec: postgresv1alpha1.DatabaseSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: nonexistentCluster}, DatabaseName: pgName}}
		create(first)
		time.Sleep(1100 * time.Millisecond)
		second := first.DeepCopy()
		second.ObjectMeta = metav1.ObjectMeta{Name: "dupdb-b-" + suffix, Namespace: ns}
		create(second)

		r := &DatabaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Eventually(func(g Gomega) {
			g.Expect(r.checkDatabaseName(ctx, first, pgName)).To(Succeed())
			g.Expect(reasonOf(r.checkDatabaseName(ctx, second, pgName))).To(Equal(ReasonDuplicateDatabaseName))
		}).Should(Succeed())

		By("never dropping the database for the later one")
		second.Status.DatabaseName = pgName
		Expect(r.dropPostgresDatabase(ctx, &postgresv1alpha1.Cluster{}, second)).To(Succeed())
	})

	It("keeps a per-Cluster marker key in a pgop-managed Secret that passwordSecretRef cannot read", func() {
		cluster := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "mk-" + suffix, Namespace: ns},
			Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage},
		}
		create(cluster)
		_, err := loadMarkerSigner(ctx, k8sClient, cluster)
		Expect(err).To(MatchError(ContainSubstring("marker key Secret")))

		Expect(ensureMarkerKeySecret(ctx, k8sClient, k8sClient.Scheme(), cluster)).To(Succeed())
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: markerKeySecretName(cluster), Namespace: ns}, secret)).To(Succeed())
		Expect(secret.Data[secretKeyMarkerKey]).To(HaveLen(markerKeySize))
		Expect(secret.Labels).To(HaveKeyWithValue(LabelAppManagedBy, LabelValuePgop))
		Expect(metav1.IsControlledBy(secret, cluster)).To(BeTrue())
		key := secret.Data[secretKeyMarkerKey]

		By("never changing an existing key")
		Expect(ensureMarkerKeySecret(ctx, k8sClient, k8sClient.Scheme(), cluster)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret)).To(Succeed())
		Expect(secret.Data[secretKeyMarkerKey]).To(Equal(key))

		signer, err := loadMarkerSigner(ctx, k8sClient, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(signer.verify(signer.marker(markerKindRole, "x"), markerKindRole, "x")).To(BeTrue())
		Expect(signer.marker(markerKindRole, "x")).NotTo(Equal(testSigner.marker(markerKindRole, "x")))

		By("refusing it as a passwordSecretRef")
		role := &postgresv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "mk-role", Namespace: ns},
			Spec: postgresv1alpha1.RoleSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: cluster.Name},
				PasswordSecretRef: &postgresv1alpha1.SecretKeySelector{Name: secret.Name, Key: secretKeyMarkerKey}}}
		_, err = (&RoleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}).readPasswordSecretRef(ctx, role)
		Expect(reasonOf(err)).To(Equal(ReasonRolePolicyViolation))
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })
	})

	It("cannot reach a Cluster in another namespace", func() {
		otherNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-" + suffix}}
		Expect(k8sClient.Create(ctx, otherNS)).To(Succeed())
		cluster := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "xns-" + suffix, Namespace: ns},
			Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage},
		}
		create(cluster)
		role := &postgresv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "xns-role", Namespace: otherNS.Name},
			Spec: postgresv1alpha1.RoleSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: cluster.Name}}}
		create(role)
		db := &postgresv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: "xns-db", Namespace: otherNS.Name},
			Spec: postgresv1alpha1.DatabaseSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: cluster.Name}}}
		create(db)

		rr := &RoleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := rr.getCluster(ctx, role)
		Expect(err).To(MatchError(ContainSubstring("not found")))
		_, err = rr.Reconcile(ctx, reconcileRequest(role))
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(role), role)).To(Succeed())
		cond := meta.FindStatusCondition(role.Status.Conditions, ConditionTypeAvailable)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Message).To(ContainSubstring("not found"))
		Expect(role.Finalizers).To(BeEmpty(), "nothing was done on the other namespace's Cluster")

		dr := &DatabaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err = dr.getCluster(ctx, db)
		Expect(err).To(MatchError(ContainSubstring("not found")))

		By("and the other namespace's Roles are not mapped to the Cluster")
		Expect(rr.rolesForCluster(ctx, cluster)).To(BeEmpty())
		Expect(dr.databasesForCluster(ctx, cluster)).To(BeEmpty())
	})
})
