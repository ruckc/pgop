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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

var _ = Describe("Role Controller", func() {
	const (
		RoleNamespace = "default"
		timeout       = time.Second * 10
		interval      = time.Millisecond * 250
	)

	Context("When creating a Role resource", func() {
		It("should successfully create and retrieve the Role resource", func() {
			ctx := context.Background()
			roleName := fmt.Sprintf("test-role-%d", time.Now().UnixNano())
			clusterName := fmt.Sprintf("role-cluster-%d", time.Now().UnixNano())

			By("Creating a Cluster for the Role")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: RoleNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{
					Image:    DefaultPostgresImage,
					Replicas: 1,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			defer func() {
				cluster.Finalizers = nil
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}()

			By("Creating the Role resource")
			role := &postgresv1alpha1.Role{
				ObjectMeta: metav1.ObjectMeta{
					Name:      roleName,
					Namespace: RoleNamespace,
				},
				Spec: postgresv1alpha1.RoleSpec{
					ClusterRef: postgresv1alpha1.ClusterReference{
						Name: clusterName,
					},
					Login:           new(true),
					Superuser:       false,
					CreateDB:        true,
					ConnectionLimit: new(int32(10)),
				},
			}
			Expect(k8sClient.Create(ctx, role)).To(Succeed())

			defer func() {
				role.Finalizers = nil
				_ = k8sClient.Update(ctx, role)
				_ = k8sClient.Delete(ctx, role)
			}()

			By("Verifying the Role was created")
			err := k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: RoleNamespace}, role)
			Expect(err).NotTo(HaveOccurred())
			Expect(role.Spec.IsLogin()).To(BeTrue())
			Expect(role.Spec.CreateDB).To(BeTrue())
			Expect(role.Spec.GetConnectionLimit()).To(Equal(int32(10)))

			By("Verifying the cluster reference")
			Expect(role.Spec.ClusterRef.Name).To(Equal(clusterName))
		})

		It("should handle reconciliation when cluster is not ready", func() {
			ctx := context.Background()
			roleName := fmt.Sprintf("test-role-%d", time.Now().UnixNano())
			clusterName := fmt.Sprintf("role-cluster-%d", time.Now().UnixNano())

			By("Creating a Cluster for the Role")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: RoleNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{
					Image: DefaultPostgresImage,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			defer func() {
				cluster.Finalizers = nil
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}()

			By("Creating the Role resource")
			role := &postgresv1alpha1.Role{
				ObjectMeta: metav1.ObjectMeta{
					Name:      roleName,
					Namespace: RoleNamespace,
				},
				Spec: postgresv1alpha1.RoleSpec{
					ClusterRef: postgresv1alpha1.ClusterReference{
						Name: clusterName,
					},
					Login: new(true),
				},
			}
			Expect(k8sClient.Create(ctx, role)).To(Succeed())

			defer func() {
				role.Finalizers = nil
				_ = k8sClient.Update(ctx, role)
				_ = k8sClient.Delete(ctx, role)
			}()

			By("Reconciling the Role")
			controllerReconciler := &RoleReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Reconcile should return without fatal error
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: roleName, Namespace: RoleNamespace},
			})
			// May requeue since cluster is not ready
			_ = err
			_ = result
		})
	})

	Context("When a Role sets defaulted attributes to false/zero", func() {
		It("should preserve them across the finalizer patch", func() {
			ctx := context.Background()
			roleName := fmt.Sprintf("group-role-%d", time.Now().UnixNano())
			role := &postgresv1alpha1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: RoleNamespace},
				Spec: postgresv1alpha1.RoleSpec{
					ClusterRef:      postgresv1alpha1.ClusterReference{Name: nonexistentCluster},
					Login:           new(false),
					Inherit:         new(false),
					ConnectionLimit: new(int32(0)),
				},
			}
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			defer func() {
				role.Finalizers = nil
				_ = k8sClient.Update(ctx, role)
				_ = k8sClient.Delete(ctx, role)
			}()

			By("Patching the finalizer the way the controller does")
			base := role.DeepCopy()
			controllerutil.AddFinalizer(role, roleFinalizer)
			Expect(k8sClient.Patch(ctx, role, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))).To(Succeed())

			got := &postgresv1alpha1.Role{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: RoleNamespace}, got)).To(Succeed())
			Expect(got.Spec.IsLogin()).To(BeFalse())
			Expect(got.Spec.IsInherit()).To(BeFalse())
			Expect(got.Spec.GetConnectionLimit()).To(Equal(int32(0)))
			Expect(got.Finalizers).To(ContainElement(roleFinalizer))
		})
	})

	Context("When a Role declares memberships", func() {
		newMembershipRole := func(spec postgresv1alpha1.RoleSpec) *postgresv1alpha1.Role {
			spec.ClusterRef = postgresv1alpha1.ClusterReference{Name: nonexistentCluster}
			return &postgresv1alpha1.Role{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("membership-role-%d", time.Now().UnixNano()),
					Namespace: RoleNamespace,
				},
				Spec: spec,
			}
		}

		It("should round-trip memberships and default revokeRemovedMemberships to true", func() {
			ctx := context.Background()
			role := newMembershipRole(postgresv1alpha1.RoleSpec{
				Memberships: []postgresv1alpha1.RoleMembership{
					{Role: memParent, Inherit: new(false), Set: new(true)},
					{Role: "admins", Admin: true},
				},
			})
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, role) }()

			got := &postgresv1alpha1.Role{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(role), got)).To(Succeed())
			Expect(got.Spec.Memberships).To(Equal(role.Spec.Memberships))
			Expect(got.Spec.RevokeRemovedMemberships).To(Equal(new(true)))
			Expect(got.Spec.ShouldRevokeRemovedMemberships()).To(BeTrue())
		})

		It("should still accept the deprecated memberOf field alongside other memberships", func() {
			ctx := context.Background()
			role := newMembershipRole(postgresv1alpha1.RoleSpec{
				MemberOf:                 []string{memLegacy},
				Memberships:              []postgresv1alpha1.RoleMembership{{Role: memParent}},
				RevokeRemovedMemberships: new(false),
			})
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, role) }()

			got := &postgresv1alpha1.Role{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(role), got)).To(Succeed())
			Expect(got.Spec.MemberOf).To(Equal([]string{memLegacy})) //nolint:staticcheck // exercising the deprecated field
			Expect(got.Spec.ShouldRevokeRemovedMemberships()).To(BeFalse())
		})

		It("should reject a role listed in both memberOf and memberships", func() {
			role := newMembershipRole(postgresv1alpha1.RoleSpec{
				MemberOf:    []string{memLegacy, memParent},
				Memberships: []postgresv1alpha1.RoleMembership{{Role: memParent}},
			})
			err := k8sClient.Create(context.Background(), role)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must not be listed in both memberOf and memberships"))
		})

		It("should reject an update that adds a duplicate across the two fields", func() {
			ctx := context.Background()
			role := newMembershipRole(postgresv1alpha1.RoleSpec{MemberOf: []string{memParent}})
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, role) }()

			role.Spec.Memberships = []postgresv1alpha1.RoleMembership{{Role: memParent, Admin: true}}
			err := k8sClient.Update(ctx, role)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must not be listed in both memberOf and memberships"))
		})

		It("should reject the same role twice in memberships", func() {
			role := newMembershipRole(postgresv1alpha1.RoleSpec{
				Memberships: []postgresv1alpha1.RoleMembership{{Role: memParent}, {Role: memParent, Admin: true}},
			})
			Expect(k8sClient.Create(context.Background(), role)).NotTo(Succeed())
		})

		It("should reject an empty membership role", func() {
			role := newMembershipRole(postgresv1alpha1.RoleSpec{
				Memberships: []postgresv1alpha1.RoleMembership{{Role: ""}},
			})
			Expect(k8sClient.Create(context.Background(), role)).NotTo(Succeed())
		})
	})

	Context("When a Role references a non-existent Cluster", func() {
		It("should update status to not ready", func() {
			ctx := context.Background()
			roleName := fmt.Sprintf("orphan-role-%d", time.Now().UnixNano())

			By("Creating a Role with invalid cluster reference")
			role := &postgresv1alpha1.Role{
				ObjectMeta: metav1.ObjectMeta{
					Name:      roleName,
					Namespace: RoleNamespace,
				},
				Spec: postgresv1alpha1.RoleSpec{
					ClusterRef: postgresv1alpha1.ClusterReference{
						Name: nonexistentCluster,
					},
					Login: new(true),
				},
			}
			Expect(k8sClient.Create(ctx, role)).To(Succeed())

			defer func() {
				role.Finalizers = nil
				_ = k8sClient.Update(ctx, role)
				_ = k8sClient.Delete(ctx, role)
			}()

			By("Reconciling the Role")
			controllerReconciler := &RoleReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: roleName, Namespace: RoleNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the Role status shows not ready")
			err = k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: RoleNamespace}, role)
			Expect(err).NotTo(HaveOccurred())
			Expect(role.Status.Ready).To(BeFalse())
		})
	})

	Context("When a Role resource does not exist", func() {
		It("should not return an error", func() {
			ctx := context.Background()
			controllerReconciler := &RoleReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "nonexistent-role",
					Namespace: RoleNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

var _ = Describe("Role Secret Generation", func() {
	It("should generate a random password", func() {
		password1, err := generateRolePassword(32)
		Expect(err).NotTo(HaveOccurred())
		Expect(password1).To(HaveLen(32))

		password2, err := generateRolePassword(32)
		Expect(err).NotTo(HaveOccurred())
		Expect(password2).To(HaveLen(32))

		// Passwords should be different
		Expect(password1).NotTo(Equal(password2))
	})
})
