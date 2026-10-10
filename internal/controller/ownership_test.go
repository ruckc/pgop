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

// fakeOwnershipClient simulates the comments and memberships of existing
// roles and databases.
type fakeOwnershipClient struct {
	roles     map[string]string // role -> comment
	closures  map[string][]postgres.ReachableRole
	databases map[string]string // database -> comment
	calls     []string
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

	Describe("checkRoleOwnership", func() {
		It("creates a missing role with the marker", func() {
			f := &fakeOwnershipClient{roles: map[string]string{}}
			marker, err := rr().checkRoleOwnership(ctx, f, newRole(""), grantTestRole, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(marker).To(Equal(myMarker))
		})

		It("manages a role carrying its marker without re-marking it", func() {
			f := &fakeOwnershipClient{roles: map[string]string{grantTestRole: myMarker}}
			marker, err := rr().checkRoleOwnership(ctx, f, newRole(grantTestRole), grantTestRole, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(marker).To(BeEmpty())
		})

		It("marks a legacy role an earlier pgop recorded in status", func() {
			f := &fakeOwnershipClient{roles: map[string]string{grantTestRole: ""}}
			marker, err := rr().checkRoleOwnership(ctx, f, newRole(grantTestRole), grantTestRole, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(marker).To(Equal(myMarker))
		})

		It("leaves a pre-existing DBA role alone", func() {
			f := &fakeOwnershipClient{roles: map[string]string{grantTestRole: ""}}
			_, err := rr().checkRoleOwnership(ctx, f, newRole(""), grantTestRole, nil)
			Expect(reasonOf(err)).To(Equal(ReasonRoleNotManaged))
			Expect(err.Error()).To(ContainSubstring("COMMENT ON ROLE app IS '" + myMarker + "'"))
		})

		It("leaves a role managed by another Role alone, even if status records it", func() {
			f := &fakeOwnershipClient{roles: map[string]string{grantTestRole: ownerMarker(markerKindRole, "someone-else")}}
			_, err := rr().checkRoleOwnership(ctx, f, newRole(grantTestRole), grantTestRole, nil)
			Expect(reasonOf(err)).To(Equal(ReasonRoleNotManaged))
			Expect(err.Error()).To(ContainSubstring("managed by another pgop resource"))
		})

		It("refuses a superuser role handed over with the marker", func() {
			f := &fakeOwnershipClient{
				roles:    map[string]string{grantTestRole: myMarker},
				closures: map[string][]postgres.ReachableRole{grantTestRole: {{Name: grantTestRole, Comment: myMarker, Superuser: true}}},
			}
			_, err := rr().checkRoleOwnership(ctx, f, newRole(""), grantTestRole, nil)
			Expect(reasonOf(err)).To(Equal(ReasonRolePolicyViolation))
		})
	})

	Describe("ensureDatabase", func() {
		newDB := func(recorded string) *postgresv1alpha1.Database {
			d := &postgresv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: grantTestRole, Namespace: "ns", UID: "uid-me"}}
			d.Status.DatabaseName = recorded
			return d
		}

		It("creates and marks a missing database", func() {
			f := &fakeOwnershipClient{databases: map[string]string{}}
			db := newDB("")
			Expect(ensureDatabase(ctx, f, db, grantTestRole, "owner")).To(Succeed())
			Expect(f.calls).To(Equal([]string{"create-or-alter app owner=owner", "comment app " + myDBMarker}))
			Expect(db.Status.DatabaseName).To(Equal(grantTestRole))
		})

		It("updates the owner of its own database", func() {
			f := &fakeOwnershipClient{databases: map[string]string{grantTestRole: myDBMarker}}
			Expect(ensureDatabase(ctx, f, newDB(grantTestRole), grantTestRole, "owner")).To(Succeed())
			Expect(f.calls).To(Equal([]string{"create-or-alter app owner=owner"}))
		})

		It("marks a legacy database an earlier pgop recorded in status", func() {
			f := &fakeOwnershipClient{databases: map[string]string{grantTestRole: ""}}
			Expect(ensureDatabase(ctx, f, newDB(grantTestRole), grantTestRole, "")).To(Succeed())
			Expect(f.calls).To(ContainElement("comment app " + myDBMarker))
		})

		It("does not take over a pre-existing database or another Database's", func() {
			for _, comment := range []string{"", ownerMarker(markerKindRole, "someone-else"), "reporting db"} {
				f := &fakeOwnershipClient{databases: map[string]string{grantTestRole: comment}}
				db := newDB("")
				err := ensureDatabase(ctx, f, db, grantTestRole, "owner")
				Expect(reasonOf(err)).To(Equal(ReasonDatabaseNotManaged), comment)
				Expect(f.calls).To(BeEmpty(), "no ALTER DATABASE ... OWNER for %q", comment)
				Expect(db.Status.DatabaseName).To(BeEmpty())
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
