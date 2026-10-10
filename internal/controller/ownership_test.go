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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// fakeOwnershipClient simulates the comments and memberships of existing
// roles and databases.
type fakeOwnershipClient struct {
	roles     map[string]string // role -> comment
	closures  map[string][]postgres.ReachableRole
	databases map[string]string // database -> comment
	// raceDatabase makes CreateDatabase find the database created by
	// someone else after DatabaseComment reported it missing.
	raceDatabase bool
	calls        []string
}

func (f *fakeOwnershipClient) RoleComment(_ context.Context, name string) (bool, string, error) {
	c, ok := f.roles[name]
	return ok, c, nil
}

func (f *fakeOwnershipClient) MembershipClosure(_ context.Context, name string) ([]postgres.ReachableRole, error) {
	return f.closures[name], nil
}

func (f *fakeOwnershipClient) DatabaseComment(_ context.Context, name string) (bool, string, error) {
	c, ok := f.databases[name]
	return ok, c, nil
}

func (f *fakeOwnershipClient) CommentOnDatabase(_ context.Context, name, comment string) error {
	f.calls = append(f.calls, fmt.Sprintf("comment %s %s", name, comment))
	f.databases[name] = comment
	return nil
}

func (f *fakeOwnershipClient) CreateDatabase(_ context.Context, name, owner string, createOnly bool) error {
	_, exists := f.databases[name]
	if createOnly && (exists || f.raceDatabase) {
		return fmt.Errorf("database %q: %w", name, postgres.ErrObjectExists)
	}
	f.calls = append(f.calls, fmt.Sprintf("create-or-alter %s owner=%s", name, owner))
	if !exists {
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

// otherKeySigner signs with a key the Cluster does not have (a forger's, or
// a lost one).
var otherKeySigner = newMarkerSigner(&postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}},
	[]byte("another key, 32 bytes long......"))

// createAppDB is the fake client's record of creating (or altering) the
// test database.
const createAppDB = "create-or-alter app owner=owner"

// forgedCurrentKey is a v2 marker naming the Cluster's current key with a
// signature that does not verify.
func forgedCurrentKey(kind string) string {
	return markerV2Prefix + kind + "/" + grantTestRole + ":" + testSigner.keyID() + ".AAAA"
}

// forgedMarkers are comments that are not the Cluster's valid marker for
// the test resource of kind.
func forgedMarkers(kind string) []string {
	name := grantTestRole
	valid := ownerMarker(kind, name)
	return []string{
		legacyMarker(kind, name),          // the unsigned v1 text
		otherKeySigner.marker(kind, name), // signed with another (or a lost) key
		forgedCurrentKey(kind),            // names the current key, wrong HMAC
		valid[:len(valid)-1] + "x",        // a valid marker, altered
	}
}

var _ = Describe("Ownership of PostgreSQL roles and databases", func() {
	ctx := context.Background()
	rr := func() *RoleReconciler { return &RoleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()} }
	myMarker := ownerMarker(markerKindRole, grantTestRole)
	myDBMarker := ownerMarker(markerKindDatabase, grantTestRole)
	adoptRole := &postgresv1alpha1.RolePolicySpec{AdoptableRoles: []string{grantTestRole}}
	adoptDB := &postgresv1alpha1.RolePolicySpec{AdoptableDatabases: []string{grantTestRole}}
	testCluster := func(policy *postgresv1alpha1.RolePolicySpec) *postgresv1alpha1.Cluster {
		return &postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "cluster-uid"},
			Spec: postgresv1alpha1.ClusterSpec{RolePolicy: policy}}
	}

	newRole := func(recorded string) *postgresv1alpha1.Role {
		r := &postgresv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: grantTestRole, Namespace: "ns", UID: "uid-me"}}
		r.Status.RoleName = recorded
		return r
	}
	// check runs checkRoleOwnership; the role counts as recorded when its
	// status names the role (roleRecorded is tested separately).
	check := func(f *fakeOwnershipClient, role *postgresv1alpha1.Role, policy *postgresv1alpha1.RolePolicySpec) (string, error) {
		marker, _, err := rr().checkRoleOwnership(ctx, f, role, testCluster(policy), grantTestRole, policy, testSigner,
			role.Status.RoleName == grantTestRole)
		return marker, err
	}

	Describe("markers", func() {
		It("are signed per Cluster, kind and name", func() {
			Expect(myMarker).To(HavePrefix("pgop:v2:Role/app:"))
			Expect(testSigner.verify(myMarker, markerKindRole, grantTestRole)).To(BeTrue())
			Expect(testSigner.verify(myMarker, markerKindDatabase, grantTestRole)).To(BeFalse())
			otherCluster := newMarkerSigner(&postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c2", Namespace: "ns"}},
				testSigner.key)
			Expect(otherCluster.verify(myMarker, markerKindRole, grantTestRole)).To(BeFalse())
			for _, forged := range forgedMarkers(markerKindRole) {
				Expect(testSigner.verify(forged, markerKindRole, grantTestRole)).To(BeFalse(), forged)
			}
			Expect(markerSigner{}.verify(myMarker, markerKindRole, grantTestRole)).To(BeFalse(), "no key, no match")
		})
	})

	Describe("checkRoleOwnership", func() {
		It("creates a missing role with the marker, create-only", func() {
			marker, createOnly, err := rr().checkRoleOwnership(ctx, &fakeOwnershipClient{roles: map[string]string{}}, newRole(""),
				testCluster(nil), grantTestRole, nil, testSigner, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(marker).To(Equal(myMarker))
			Expect(createOnly).To(BeTrue(), "a concurrent CREATE by someone else must fail, not turn into an ALTER")

			_, createOnly, err = rr().checkRoleOwnership(ctx, &fakeOwnershipClient{roles: map[string]string{grantTestRole: ""}},
				newRole(grantTestRole), testCluster(nil), grantTestRole, nil, testSigner, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(createOnly).To(BeFalse())
		})

		It("manages a role recorded in status that carries its marker", func() {
			marker, err := check(&fakeOwnershipClient{roles: map[string]string{grantTestRole: myMarker}}, newRole(grantTestRole), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(marker).To(BeEmpty())
		})

		It("re-marks a recorded role whose marker is missing, unsigned or from a lost key", func() {
			for _, comment := range []string{"", legacyMarker(markerKindRole, grantTestRole), otherKeySigner.marker(markerKindRole, grantTestRole)} {
				marker, err := check(&fakeOwnershipClient{roles: map[string]string{grantTestRole: comment}}, newRole(grantTestRole), nil)
				Expect(err).NotTo(HaveOccurred(), comment)
				Expect(marker).To(Equal(myMarker), comment)
			}
		})

		It("never counts a marker naming the current key with a bad signature, even when recorded", func() {
			for _, comment := range []string{forgedCurrentKey(markerKindRole), myMarker[:len(myMarker)-1] + "x"} {
				_, err := check(&fakeOwnershipClient{roles: map[string]string{grantTestRole: comment}}, newRole(grantTestRole), nil)
				Expect(reasonOf(err)).To(Equal(ReasonRoleNotManaged), comment)
			}
		})

		It("leaves a recorded role with an unrelated comment alone", func() {
			for _, comment := range []string{"owned by the DBA team", ownerMarker(markerKindRole, "someone-else")} {
				_, err := check(&fakeOwnershipClient{roles: map[string]string{grantTestRole: comment}}, newRole(grantTestRole), nil)
				Expect(reasonOf(err)).To(Equal(ReasonRoleNotManaged), comment)
				Expect(err.Error()).To(ContainSubstring("adoptableRoles"))
			}
		})

		It("never takes over an unrecorded role on the strength of its comment", func() {
			// Includes the valid marker: a tenant can copy it onto a role it
			// administers (for example created through a throw-away role it
			// dropped afterwards, leaving no members).
			comments := append([]string{"", myMarker, "owned by the DBA team"}, forgedMarkers(markerKindRole)...)
			for _, comment := range comments {
				_, err := check(&fakeOwnershipClient{roles: map[string]string{grantTestRole: comment}}, newRole(""), nil)
				Expect(reasonOf(err)).To(Equal(ReasonRoleNotManaged), comment)
				Expect(err.Error()).To(SatisfyAll(ContainSubstring("adoptableRoles"), Not(ContainSubstring("COMMENT ON"))))
			}
		})

		It("takes over an unrecorded role a Cluster editor allowlisted (e.g. after re-creating the Role without status)", func() {
			f := &fakeOwnershipClient{roles: map[string]string{grantTestRole: myMarker},
				closures: map[string][]postgres.ReachableRole{grantTestRole: {{Name: grantTestRole}}}}
			marker, err := check(f, newRole(""), adoptRole)
			Expect(err).NotTo(HaveOccurred())
			Expect(marker).To(Equal(myMarker))
		})

		It("refuses an allowlisted superuser or a role reaching forbidden roles", func() {
			for _, closure := range [][]postgres.ReachableRole{
				{{Name: grantTestRole, Superuser: true}},
				{{Name: grantTestRole}, {Name: polDBA, Via: polDBA, Superuser: true}},
				{{Name: grantTestRole}, {Name: polExecProgram, Via: polExecProgram}},
			} {
				f := &fakeOwnershipClient{roles: map[string]string{grantTestRole: ""},
					closures: map[string][]postgres.ReachableRole{grantTestRole: closure}}
				_, err := check(f, newRole(""), adoptRole)
				Expect(reasonOf(err)).To(Equal(ReasonRolePolicyViolation), "%v", closure)
			}
		})
	})

	Describe("ensureDatabase", func() {
		newDB := func(recorded string) *postgresv1alpha1.Database {
			d := &postgresv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: grantTestRole, Namespace: "ns", UID: "uid-me"}}
			d.Status.DatabaseName = recorded
			return d
		}
		ensure := func(f *fakeOwnershipClient, db *postgresv1alpha1.Database, policy *postgresv1alpha1.RolePolicySpec, owner string) error {
			return ensureDatabase(ctx, f, db, testCluster(policy), testSigner, grantTestRole, owner,
				db.Status.DatabaseName == grantTestRole)
		}

		It("creates and marks a missing database", func() {
			f := &fakeOwnershipClient{databases: map[string]string{}}
			db := newDB("")
			Expect(ensure(f, db, nil, "owner")).To(Succeed())
			Expect(f.calls).To(Equal([]string{createAppDB, "comment app " + myDBMarker}))
			Expect(db.Status.DatabaseName).To(Equal(grantTestRole))
		})

		It("updates the owner of its own database", func() {
			f := &fakeOwnershipClient{databases: map[string]string{grantTestRole: myDBMarker}}
			Expect(ensure(f, newDB(grantTestRole), nil, "owner")).To(Succeed())
			Expect(f.calls).To(Equal([]string{createAppDB}))
		})

		It("re-marks a recorded database whose marker is missing, unsigned or from a lost key", func() {
			for _, comment := range []string{"", legacyMarker(markerKindDatabase, grantTestRole), otherKeySigner.marker(markerKindDatabase, grantTestRole)} {
				f := &fakeOwnershipClient{databases: map[string]string{grantTestRole: comment}}
				Expect(ensure(f, newDB(grantTestRole), nil, "")).To(Succeed())
				Expect(f.calls).To(ContainElement("comment app "+myDBMarker), comment)
			}
		})

		It("never takes over an unrecorded database on the strength of its comment", func() {
			comments := append([]string{"", myDBMarker, "reporting db"}, forgedMarkers(markerKindDatabase)...)
			for _, comment := range comments {
				f := &fakeOwnershipClient{databases: map[string]string{grantTestRole: comment}}
				db := newDB("")
				err := ensure(f, db, nil, "owner")
				Expect(reasonOf(err)).To(Equal(ReasonDatabaseNotManaged), comment)
				Expect(err.Error()).To(ContainSubstring("adoptableDatabases"))
				Expect(f.calls).To(BeEmpty(), "no ALTER DATABASE ... OWNER for %q", comment)
				Expect(db.Status.DatabaseName).To(BeEmpty())
			}
		})

		It("does not take over a database someone created between the check and the create", func() {
			f := &fakeOwnershipClient{databases: map[string]string{}, raceDatabase: true}
			db := newDB("")
			err := ensure(f, db, nil, "owner")
			Expect(reasonOf(err)).To(Equal(ReasonDatabaseNotManaged))
			Expect(errors.Is(err, postgres.ErrObjectExists)).To(BeTrue())
			Expect(f.calls).To(BeEmpty(), "no ALTER DATABASE ... OWNER")
			Expect(db.Status.DatabaseName).To(BeEmpty())
		})

		It("records the Cluster UID with the database name", func() {
			db := newDB("")
			Expect(ensure(&fakeOwnershipClient{databases: map[string]string{}}, db, nil, "owner")).To(Succeed())
			Expect(db.Status.ClusterUID).To(Equal("cluster-uid"))
		})

		It("takes over an unrecorded database a Cluster editor allowlisted", func() {
			f := &fakeOwnershipClient{databases: map[string]string{grantTestRole: "reporting db"}}
			db := newDB("")
			Expect(ensure(f, db, adoptDB, "owner")).To(Succeed())
			Expect(f.calls).To(Equal([]string{createAppDB, "comment app " + myDBMarker}))
			Expect(db.Status.DatabaseName).To(Equal(grantTestRole))
		})
	})
})

// Names used by the recorded-name tests.
const (
	recTestRole = "dbauser"
	recTestUIDA = "uid-a"
)

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

	It("makes clusterRef immutable on Roles and Databases", func() {
		role := &postgresv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "imm-role-" + suffix, Namespace: ns},
			Spec: postgresv1alpha1.RoleSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: "cluster-a"}}}
		create(role)
		role.Spec.ClusterRef.Name = "cluster-b"
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, role))).To(BeTrue())

		db := &postgresv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: "imm-db-" + suffix, Namespace: ns},
			Spec: postgresv1alpha1.DatabaseSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: "cluster-a"}}}
		create(db)
		db.Spec.ClusterRef.Name = "cluster-b"
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, db))).To(BeTrue())
	})

	It("treats a name as recorded only on the Cluster (UID) it was recorded on", func() {
		clusterA := &postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "rec-a-" + suffix, Namespace: ns, UID: recTestUIDA}}
		clusterB := &postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "rec-b-" + suffix, Namespace: ns, UID: "uid-b"}}
		recreatedA := clusterA.DeepCopy()
		recreatedA.UID = "uid-a-2"
		rr := &RoleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		dr := &DatabaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		role := &postgresv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "rec-role-" + suffix, Namespace: ns},
			Spec: postgresv1alpha1.RoleSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: clusterA.Name}}}
		create(role)
		role.Status.RoleName = recTestRole
		role.Status.ClusterUID = recTestUIDA
		for _, tc := range []struct {
			cluster *postgresv1alpha1.Cluster
			name    string
			want    bool
		}{
			{clusterA, recTestRole, true},
			{clusterB, recTestRole, false},   // retargeted to another Cluster
			{recreatedA, recTestRole, false}, // the Cluster was deleted and re-created
			{clusterA, "other_name", false},  // a different name
		} {
			got, err := rr.roleRecorded(ctx, role, tc.cluster, tc.name)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(tc.want), "%s/%s", tc.cluster.UID, tc.name)
		}

		By("a status written before clusterUID existed counts only with the Role's Secret for that Cluster")
		role.Status.ClusterUID = ""
		got, err := rr.roleRecorded(ctx, role, clusterA, recTestRole)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeFalse(), "no credentials Secret")
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: credentialsSecretName(role, clusterA), Namespace: ns}}
		Expect(controllerutil.SetControllerReference(role, secret, k8sClient.Scheme())).To(Succeed())
		create(secret)
		Eventually(func(g Gomega) {
			got, err := rr.roleRecorded(ctx, role, clusterA, recTestRole)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(BeTrue())
		}).Should(Succeed())
		got, err = rr.roleRecorded(ctx, role, clusterB, recTestRole)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeFalse(), "the Secret of cluster A does not prove ownership on cluster B")

		By("the same rules for Databases")
		db := &postgresv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: "rec-db-" + suffix, Namespace: ns},
			Spec: postgresv1alpha1.DatabaseSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: clusterA.Name}, Owner: "owner-x"}}
		create(db)
		db.Status.DatabaseName = "appdb"
		db.Status.ClusterUID = recTestUIDA
		for _, tc := range []struct {
			cluster *postgresv1alpha1.Cluster
			want    bool
		}{{clusterA, true}, {clusterB, false}, {recreatedA, false}} {
			got, err := dr.databaseRecorded(ctx, db, tc.cluster, "appdb")
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(tc.want), string(tc.cluster.UID))
		}
		db.Status.ClusterUID = ""
		dbSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: db.Name + "-owner-x-credentials", Namespace: ns},
			Data: map[string][]byte{SecretKeyHost: []byte(clusterHost(clusterA))}}
		Expect(controllerutil.SetControllerReference(db, dbSecret, k8sClient.Scheme())).To(Succeed())
		create(dbSecret)
		Eventually(func(g Gomega) {
			got, err := dr.databaseRecorded(ctx, db, clusterA, "appdb")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(BeTrue())
		}).Should(Succeed())
		got, err = dr.databaseRecorded(ctx, db, clusterB, "appdb")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeFalse(), "the Secret points at cluster A's host")

		By("never dropping a database recorded on another Cluster")
		db.Status.ClusterUID = "uid-b"
		Expect(dr.dropPostgresDatabase(ctx, clusterA, db)).To(Succeed(), "returns without connecting")
	})

	It("counts a role as managed only when its Role recorded it on this Cluster", func() {
		cluster := &postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "mg-" + suffix, Namespace: ns, UID: "uid-mg"}}
		newRole := func(name, pgName string) *postgresv1alpha1.Role {
			r := &postgresv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: postgresv1alpha1.RoleSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: cluster.Name}, RoleName: pgName}}
			create(r)
			return r
		}
		recordedRole := newRole("mg-rec-"+suffix, "mg_rec")
		recordedRole.Status.RoleName, recordedRole.Status.ClusterUID = "mg_rec", "uid-mg"
		Expect(k8sClient.Status().Update(ctx, recordedRole)).To(Succeed())
		specOnly := newRole("mg-spec-"+suffix, "mg_squatted") // names a role someone else built
		otherCluster := newRole("mg-other-"+suffix, "mg_other")
		otherCluster.Status.RoleName, otherCluster.Status.ClusterUID = "mg_other", "uid-old"
		Expect(k8sClient.Status().Update(ctx, otherCluster)).To(Succeed())

		rr := &RoleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Eventually(func(g Gomega) {
			managed, err := rr.managedRoles(ctx, specOnly, cluster, testSigner)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(managed).To(HaveKey("mg_rec"))
			g.Expect(managed).NotTo(HaveKey("mg_squatted"))
			g.Expect(managed).NotTo(HaveKey("mg_other"))
			g.Expect(managed.manages(postgres.ReachableRole{Name: "mg_squatted",
				Comment: testSigner.marker(markerKindRole, specOnly.Name)})).To(BeFalse())
		}).Should(Succeed())
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
