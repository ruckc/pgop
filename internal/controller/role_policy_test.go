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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

type fakeExtensionClient map[string]bool

func (f fakeExtensionClient) ExtensionTrusted(_ context.Context, name, _ string) (bool, bool, error) {
	trusted, ok := f[name]
	return trusted, ok, nil
}

// Role names used by the policy tests.
const (
	polOps          = "ops"
	polDBA          = "dba"
	polExecProgram  = "pg_execute_server_program"
	polReadAllData  = "pg_read_all_data"
	polReadSettings = "pg_read_all_settings"
	polMonitor      = "pg_monitor"
	polFileFDW      = "file_fdw"
	polTrgm         = "pg_trgm"
	polUUID         = "uuid-ossp"
	polGrantA       = "grant a to app"
)

var _ = Describe("Role policy", func() {
	type rr = postgres.ReachableRole

	allowAll := &postgresv1alpha1.RolePolicySpec{
		AllowedAttributes: []postgresv1alpha1.RoleAttribute{
			postgresv1alpha1.RoleAttributeCreateRole, postgresv1alpha1.RoleAttributeReplication, postgresv1alpha1.RoleAttributeBypassRLS,
		},
		AllowedPredefinedRoles: postgresv1alpha1.GrantablePredefinedRoles,
	}

	DescribeTable("reservedRoleName",
		func(name string, reserved bool) {
			if reserved {
				Expect(reservedRoleName(name)).NotTo(BeEmpty())
			} else {
				Expect(reservedRoleName(name)).To(BeEmpty())
			}
		},
		Entry(bootstrapRoleName, bootstrapRoleName, true),
		Entry("operator", "pgop_operator", true),
		Entry("replicator", "pgop_replicator", true),
		Entry("any pgop_ name", "pgop_anything", true),
		Entry("predefined prefix", polMonitor, true),
		Entry("plain", grantTestRole, false),
		Entry("similar but not reserved", "pgopx", false),
		Entry("postgres prefix", "postgres_app", false),
	)

	DescribeTable("reservedDatabaseName",
		func(name string, reserved bool) {
			if reserved {
				Expect(reservedDatabaseName(name)).NotTo(BeEmpty())
			} else {
				Expect(reservedDatabaseName(name)).To(BeEmpty())
			}
		},
		Entry("maintenance db", bootstrapRoleName, true),
		Entry("template0", "template0", true),
		Entry("template1", "template1", true),
		Entry("app", grantTestRole, false),
	)

	DescribeTable("systemSchemaName",
		func(name string, system bool) { Expect(systemSchemaName(name)).To(Equal(system)) },
		Entry("pg_catalog", "pg_catalog", true),
		Entry("pg_toast", "pg_toast", true),
		Entry("information_schema", "information_schema", true),
		Entry("public", "public", false),
		Entry("app", grantTestRole, false),
	)

	DescribeTable("membershipNameProblem without a policy",
		func(name, want string) {
			Expect(membershipNameProblem(name, nil)).To(ContainSubstring(want))
		},
		Entry(bootstrapRoleName, bootstrapRoleName, "reserved"),
		Entry("pgop_operator", "pgop_operator", "reserved for the operator"),
		Entry("pgop_replicator", "pgop_replicator", "reserved for the operator"),
		Entry(polExecProgram, polExecProgram, "files or programs"),
		Entry("pg_read_server_files", "pg_read_server_files", "files or programs"),
		Entry("pg_write_server_files", "pg_write_server_files", "files or programs"),
		Entry(polMonitor, polMonitor, "allowedPredefinedRoles"),
		Entry(polReadAllData, polReadAllData, "allowedPredefinedRoles"),
		Entry("pg_database_owner", "pg_database_owner", "allowedPredefinedRoles"),
		Entry("plain role", "app_ro", ""),
	)

	It("allows listed predefined roles but never the server-file roles", func() {
		for _, name := range postgresv1alpha1.GrantablePredefinedRoles {
			Expect(membershipNameProblem(name, allowAll)).To(BeEmpty(), name)
		}
		for _, name := range postgresv1alpha1.ForbiddenPredefinedRoles {
			policy := &postgresv1alpha1.RolePolicySpec{AllowedPredefinedRoles: []string{name}}
			Expect(membershipNameProblem(name, policy)).NotTo(BeEmpty(), name)
		}
		// An unknown pg_ name is not allowed even if listed (the CRD enum
		// rejects it; the Go check does not rely on that).
		Expect(membershipNameProblem("pg_future", &postgresv1alpha1.RolePolicySpec{AllowedPredefinedRoles: []string{"pg_future"}})).
			NotTo(BeEmpty())
	})

	Describe("membershipProblem", func() {
		It("allows a plain role", func() {
			Expect(membershipProblem("app_ro", []rr{{Name: "app_ro"}}, nil)).To(BeEmpty())
		})
		It("allows a role that does not exist yet (GRANT then fails)", func() {
			Expect(membershipProblem("later", nil, nil)).To(BeEmpty())
		})
		It("refuses a superuser", func() {
			Expect(membershipProblem(polDBA, []rr{{Name: polDBA, Superuser: true}}, allowAll)).To(Equal("dba is a superuser"))
		})
		It("refuses a role with an attribute the policy does not allow", func() {
			closure := []rr{{Name: "repl", Replication: true, BypassRLS: true}}
			Expect(membershipProblem("repl", closure, nil)).To(ContainSubstring("has REPLICATION, BYPASSRLS"))
			Expect(membershipProblem("repl", closure, allowAll)).To(BeEmpty())
		})
		It("allows the predefined roles an allowed predefined role contains", func() {
			monitor := &postgresv1alpha1.RolePolicySpec{AllowedPredefinedRoles: []string{polMonitor}}
			closure := []rr{{Name: polMonitor}, {Name: polReadSettings, Via: polReadSettings}}
			Expect(membershipProblem(polMonitor, closure, monitor)).To(BeEmpty())
			Expect(adoptionProblem("app_mon", []rr{{Name: "app_mon"}, {Name: polMonitor, Via: polMonitor},
				{Name: polReadSettings, Via: polMonitor}}, monitor)).To(BeEmpty())
			By("but not a server-file role, even inside an allowed predefined role")
			withExec := []rr{{Name: polMonitor}, {Name: polReadSettings, Via: polReadSettings}, {Name: polExecProgram, Via: polExecProgram}}
			Expect(membershipProblem(polMonitor, withExec, monitor)).To(ContainSubstring(polExecProgram))
			By("and not the contained roles when the outer role is not allowed")
			Expect(membershipProblem("ops", []rr{{Name: "ops"}, {Name: polMonitor, Via: polMonitor},
				{Name: polReadSettings, Via: polMonitor}}, nil)).To(ContainSubstring(polMonitor))
		})

		It("refuses a role that reaches a superuser or a forbidden role indirectly", func() {
			Expect(membershipProblem(polOps, []rr{{Name: polOps}, {Name: bootstrapRoleName, Via: "admins", Superuser: true}}, allowAll)).
				To(Equal("ops is a member of postgres, which is reserved (bootstrap superuser)"))
			Expect(membershipProblem(polOps, []rr{{Name: polOps}, {Name: polExecProgram, Via: polExecProgram}}, allowAll)).
				To(ContainSubstring("ops is a member of pg_execute_server_program"))
			Expect(membershipProblem(polOps, []rr{{Name: polOps}, {Name: polReadAllData, Via: polReadAllData}}, nil)).
				To(ContainSubstring("allowedPredefinedRoles"))
		})
	})

	Describe("adoptionProblem", func() {
		It("allows a new or plain role", func() {
			Expect(adoptionProblem(grantTestRole, nil, nil)).To(BeEmpty())
			Expect(adoptionProblem(grantTestRole, []rr{{Name: grantTestRole, CreateRole: true}}, nil)).To(BeEmpty())
		})
		It("refuses an existing superuser", func() {
			Expect(adoptionProblem(polDBA, []rr{{Name: polDBA, Superuser: true}}, allowAll)).To(ContainSubstring("it is a superuser"))
		})
		It("refuses a role that is already a member of a forbidden role", func() {
			Expect(adoptionProblem(polOps, []rr{{Name: polOps}, {Name: grantTestAdmin, Via: grantTestAdmin, Superuser: true}}, allowAll)).
				To(ContainSubstring("it is a member of admin, which is a superuser"))
		})
	})

	Describe("desiredRoleOptions", func() {
		spec := postgresv1alpha1.RoleSpec{CreateDB: true, CreateRole: true, BypassRLS: true}

		It("is never a superuser and keeps non-privileged attributes", func() {
			opts, err := desiredRoleOptions(&postgresv1alpha1.RoleSpec{CreateDB: true}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(opts).To(Equal(postgres.RoleOptions{Login: true, Inherit: true, CreateDB: true, ConnectionLimit: -1}))
		})

		It("applies none of the privileged attributes when one is not allowed", func() {
			policy := &postgresv1alpha1.RolePolicySpec{AllowedAttributes: []postgresv1alpha1.RoleAttribute{postgresv1alpha1.RoleAttributeCreateRole}}
			opts, err := desiredRoleOptions(&spec, policy)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue())
			Expect(ce.reason).To(Equal(ReasonRolePolicyViolation))
			Expect(err.Error()).To(ContainSubstring("does not allow bypassRLS"))
			Expect(err.Error()).NotTo(ContainSubstring("allow createRole"))
			Expect(opts.CreateRole || opts.Replication || opts.BypassRLS).To(BeFalse())
			Expect(opts.CreateDB).To(BeTrue())
		})

		It("applies the privileged attributes the policy allows", func() {
			opts, err := desiredRoleOptions(&spec, allowAll)
			Expect(err).NotTo(HaveOccurred())
			Expect(opts.CreateRole).To(BeTrue())
			Expect(opts.BypassRLS).To(BeTrue())
			Expect(opts.Replication).To(BeFalse())
		})
	})

	Describe("extensionAllowed", func() {
		ctx := context.Background()
		pg := fakeExtensionClient{polTrgm: true, polFileFDW: false}

		DescribeTable("decisions",
			func(name string, policy *postgresv1alpha1.RolePolicySpec, want bool) {
				got, err := extensionAllowed(ctx, pg, policy, postgresv1alpha1.ExtensionSpec{Name: name})
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(want))
			},
			Entry("trusted", polTrgm, nil, true),
			Entry("untrusted", polFileFDW, nil, false),
			Entry("untrusted but allowed", polFileFDW, &postgresv1alpha1.RolePolicySpec{AllowedExtensions: []string{polFileFDW}}, true),
			Entry("not available", "nope", nil, false),
		)
	})
})
