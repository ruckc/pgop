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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// reservedPrefixName is a name using the prefix reserved for the operator.
const reservedPrefixName = "pgop_x"

// CRD validation of the role privilege policy (issue #24, PR 1).
var _ = Describe("Role policy CRD validation", func() {
	const ns = "default"

	var (
		ctx    context.Context
		suffix string
	)

	BeforeEach(func() {
		ctx = context.Background()
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	create := func(obj client.Object) error {
		err := k8sClient.Create(ctx, obj)
		if err == nil {
			DeferCleanup(func() {
				obj.SetFinalizers(nil)
				_ = k8sClient.Update(ctx, obj)
				_ = k8sClient.Delete(ctx, obj)
			})
		}
		return err
	}
	expectInvalid := func(err error) {
		ExpectWithOffset(1, apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
	}

	newRole := func(name string, spec postgresv1alpha1.RoleSpec) *postgresv1alpha1.Role {
		spec.ClusterRef = postgresv1alpha1.ClusterReference{Name: nonexistentCluster}
		return &postgresv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
	}
	newDatabase := func(name string, spec postgresv1alpha1.DatabaseSpec) *postgresv1alpha1.Database {
		spec.ClusterRef = postgresv1alpha1.ClusterReference{Name: nonexistentCluster}
		return &postgresv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
	}

	Context("Role", func() {
		for _, target := range []string{bootstrapRoleName, "pgop_operator", "pgop_replicator", reservedPrefixName,
			polExecProgram, "pg_read_server_files", "pg_write_server_files"} {
			It(fmt.Sprintf("rejects membership in %q", target), func() {
				expectInvalid(create(newRole("m-"+suffix, postgresv1alpha1.RoleSpec{
					Memberships: []postgresv1alpha1.RoleMembership{{Role: target}},
				})))
				expectInvalid(create(newRole("mo-"+suffix, postgresv1alpha1.RoleSpec{
					MemberOf: []string{grantTestRole, target}, //nolint:staticcheck // deprecated field under test
				})))
			})
		}

		It("accepts memberships whose policy is decided at reconcile time", func() {
			Expect(create(newRole("ok-"+suffix, postgresv1alpha1.RoleSpec{
				Memberships: []postgresv1alpha1.RoleMembership{{Role: polMonitor}, {Role: "app_ro"}},
				CreateRole:  true,
				BypassRLS:   true,
			}))).To(Succeed())
		})

		It("rejects a Role named postgres without spec.roleName", func() {
			expectInvalid(create(newRole(bootstrapRoleName, postgresv1alpha1.RoleSpec{})))
			Expect(create(newRole(bootstrapRoleName, postgresv1alpha1.RoleSpec{RoleName: "app_x"}))).To(Succeed())
		})

		It("no longer has a superuser field", func() {
			const field = pgContextSuperuser // the removed spec.superuser field
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(postgresv1alpha1.GroupVersion.WithKind("Role"))
			u.SetName("su-" + suffix)
			u.SetNamespace(ns)
			Expect(unstructured.SetNestedField(u.Object, nonexistentCluster, "spec", "clusterRef", "name")).To(Succeed())
			Expect(unstructured.SetNestedField(u.Object, true, "spec", field)).To(Succeed())
			Expect(create(u)).To(Succeed())
			_, found, err := unstructured.NestedFieldNoCopy(u.Object, "spec", field)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse(), "spec.superuser must be pruned")
		})
	})

	Context("Cluster spec.rolePolicy", func() {
		newCluster := func(policy *postgresv1alpha1.RolePolicySpec) *postgresv1alpha1.Cluster {
			return &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "policy-" + suffix, Namespace: ns},
				Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage, RolePolicy: policy},
			}
		}

		It("accepts known attributes, predefined roles and extension names", func() {
			Expect(create(newCluster(&postgresv1alpha1.RolePolicySpec{
				AllowedAttributes: []postgresv1alpha1.RoleAttribute{
					postgresv1alpha1.RoleAttributeCreateRole, postgresv1alpha1.RoleAttributeReplication, postgresv1alpha1.RoleAttributeBypassRLS,
				},
				AllowedPredefinedRoles: postgresv1alpha1.GrantablePredefinedRoles,
				AllowedExtensions:      []string{polFileFDW, polUUID},
			}))).To(Succeed())
		})

		It("rejects superuser as an attribute", func() {
			expectInvalid(create(newCluster(&postgresv1alpha1.RolePolicySpec{
				AllowedAttributes: []postgresv1alpha1.RoleAttribute{"superuser"},
			})))
		})

		for _, name := range []string{polExecProgram, "pg_read_server_files", "pg_write_server_files", "pg_database_owner", bootstrapRoleName} {
			It(fmt.Sprintf("rejects %q as an allowed predefined role", name), func() {
				expectInvalid(create(newCluster(&postgresv1alpha1.RolePolicySpec{AllowedPredefinedRoles: []string{name}})))
			})
		}

		It("rejects an invalid extension name", func() {
			expectInvalid(create(newCluster(&postgresv1alpha1.RolePolicySpec{AllowedExtensions: []string{`x"; DROP`}})))
		})

		It("validates allowedExistingRoles like role names", func() {
			for _, name := range []string{bootstrapRoleName, "pg_monitor_x", DefaultOperatorUsername, "Analytics", `a"b`} {
				expectInvalid(create(newCluster(&postgresv1alpha1.RolePolicySpec{AllowedExistingRoles: []string{name}})))
			}
			Expect(create(newCluster(&postgresv1alpha1.RolePolicySpec{AllowedExistingRoles: []string{"analytics_ro"}}))).To(Succeed())
		})

		It("validates adoptableRoles and adoptableDatabases", func() {
			for _, name := range []string{bootstrapRoleName, "pg_x", reservedPrefixName, "Upper", `a"b`} {
				expectInvalid(create(newCluster(&postgresv1alpha1.RolePolicySpec{AdoptableRoles: []string{name}})))
			}
			for _, name := range []string{bootstrapRoleName, template1Name, "template_app", "pg_x", reservedPrefixName, "Upper"} {
				expectInvalid(create(newCluster(&postgresv1alpha1.RolePolicySpec{AdoptableDatabases: []string{name}})))
			}
			Expect(create(newCluster(&postgresv1alpha1.RolePolicySpec{
				AdoptableRoles: []string{"legacy_app"}, AdoptableDatabases: []string{"legacy_db"},
			}))).To(Succeed())
		})
	})

	Context("Database", func() {
		for _, name := range []string{bootstrapRoleName, "template0", "template1"} {
			It(fmt.Sprintf("rejects a Database named %q without spec.databaseName", name), func() {
				expectInvalid(create(newDatabase(name, postgresv1alpha1.DatabaseSpec{})))
			})
		}

		for _, name := range []string{"pg_catalog", "pg_toast", "information_schema"} {
			It(fmt.Sprintf("rejects the system schema %q", name), func() {
				expectInvalid(create(newDatabase("s-"+suffix, postgresv1alpha1.DatabaseSpec{
					Schemas: []postgresv1alpha1.SchemaSpec{{Name: name}},
				})))
			})
		}

		It("rejects an invalid extension name", func() {
			expectInvalid(create(newDatabase("e-"+suffix, postgresv1alpha1.DatabaseSpec{
				Extensions: []postgresv1alpha1.ExtensionSpec{{Name: `x" CASCADE`}},
			})))
		})

		It("accepts regular schemas and extensions", func() {
			Expect(create(newDatabase("ok-"+suffix, postgresv1alpha1.DatabaseSpec{
				Schemas:    []postgresv1alpha1.SchemaSpec{{Name: "app_s"}, {Name: "public"}},
				Extensions: []postgresv1alpha1.ExtensionSpec{{Name: polUUID}, {Name: polTrgm, Schema: "app_s", Version: "1.6"}},
			}))).To(Succeed())
		})
	})
})
