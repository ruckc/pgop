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
	"maps"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// PostgreSQL role names used by the membership tests.
const (
	memParent = "parent"
	memLegacy = "legacy"
	memGone   = "gone"
	memOld    = "old"
)

// testManaged lists the roles the test Cluster's Roles manage; the fake
// client's default closure gives every role its Role's marker.
var testManaged = func() managedRoles {
	m := managedRoles{}
	for _, name := range []string{"a", "b", grantTestOther, memParent, memLegacy, memGone, memOld,
		"ops", "dba", "tenant_b", "wraps_dba", "app_ro", "repl", "app_mon"} {
		m[name] = ownerMarker(markerKindRole, name)
	}
	return m
}()

// fakeMembershipClient records statements and simulates pg_auth_members.
type fakeMembershipClient struct {
	version int
	current map[string]postgres.MembershipState
	// closures maps a role to its MembershipClosure; a role not listed is
	// a plain role without memberships.
	closures map[string][]postgres.ReachableRole
	failOn   string // statement prefix + role, e.g. "grant:b"
	calls    []string
}

func (f *fakeMembershipClient) MembershipClosure(_ context.Context, name string) ([]postgres.ReachableRole, error) {
	if c, ok := f.closures[name]; ok {
		return c, nil
	}
	return []postgres.ReachableRole{{Name: name, Comment: ownerMarker(markerKindRole, name)}}, nil
}

func (f *fakeMembershipClient) fail(op, role string) error {
	if f.failOn == op+":"+role {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeMembershipClient) ServerVersionNum(context.Context) (int, error) { return f.version, nil }

func (f *fakeMembershipClient) ListMemberships(context.Context, string, int) (map[string]postgres.MembershipState, error) {
	out := map[string]postgres.MembershipState{}
	maps.Copy(out, f.current)
	return out, nil
}

func (f *fakeMembershipClient) GrantRole(_ context.Context, role, member string, opts postgres.MembershipOptions) error {
	f.calls = append(f.calls, fmt.Sprintf("grant %s to %s", role, member))
	return f.fail("grant", role)
}

func (f *fakeMembershipClient) RevokeAdminOption(_ context.Context, role, member string) error {
	f.calls = append(f.calls, fmt.Sprintf("revoke admin %s from %s", role, member))
	return f.fail("revokeadmin", role)
}

func (f *fakeMembershipClient) RevokeRole(_ context.Context, role, member string) error {
	f.calls = append(f.calls, fmt.Sprintf("revoke %s from %s", role, member))
	return f.fail("revoke", role)
}

var _ = Describe("Role memberships", func() {
	type rm = postgresv1alpha1.RoleMembership
	type st = postgres.MembershipState

	Describe("RoleSpec.DesiredMemberships", func() {
		It("merges memberships and the deprecated memberOf", func() {
			spec := postgresv1alpha1.RoleSpec{
				MemberOf:    []string{memLegacy, memParent}, //nolint:staticcheck // deprecated field under test
				Memberships: []rm{{Role: memParent, Admin: true}, {Role: "other", Inherit: new(false)}},
			}
			Expect(spec.DesiredMemberships()).To(Equal([]rm{
				{Role: memParent, Admin: true},
				{Role: "other", Inherit: new(false)},
				{Role: memLegacy},
			}))
		})
	})

	Describe("diffMemberships", func() {
		It("grants a missing membership", func() {
			plan := diffMemberships([]rm{{Role: "a", Set: new(false)}}, nil, nil, true)
			Expect(plan.Grant).To(Equal([]membershipGrant{{Role: "a", Opts: postgres.MembershipOptions{Set: new(false)}}}))
			Expect(plan.RevokeAdmin).To(BeEmpty())
			Expect(plan.Revoke).To(BeEmpty())
		})

		It("does nothing when the grant already matches", func() {
			plan := diffMemberships(
				[]rm{{Role: "a", Inherit: new(true)}, {Role: "b"}},
				[]string{"a", "b"},
				map[string]st{"a": {Inherit: new(true), Set: new(true)}, "b": {Inherit: new(false), Set: new(false)}},
				true)
			Expect(plan).To(Equal(membershipPlan{}))
		})

		It("re-grants when an option changes", func() {
			plan := diffMemberships(
				[]rm{{Role: "a", Inherit: new(false), Set: new(true)}},
				[]string{"a"},
				map[string]st{"a": {Inherit: new(true), Set: new(true)}},
				true)
			Expect(plan.Grant).To(HaveLen(1))
			Expect(plan.Grant[0].Opts.Inherit).To(Equal(new(false)))
		})

		It("grants ADMIN when requested and revokes it when no longer wanted", func() {
			plan := diffMemberships([]rm{{Role: "a", Admin: true}}, []string{"a"}, map[string]st{"a": {}}, true)
			Expect(plan.Grant).To(Equal([]membershipGrant{{Role: "a", Opts: postgres.MembershipOptions{Admin: true}}}))

			plan = diffMemberships([]rm{{Role: "a"}}, []string{"a"}, map[string]st{"a": {Admin: true}}, true)
			Expect(plan.Grant).To(BeEmpty())
			Expect(plan.RevokeAdmin).To(Equal([]string{"a"}))
		})

		It("revokes a managed membership removed from the spec", func() {
			plan := diffMemberships([]rm{{Role: "a"}}, []string{"a", memGone},
				map[string]st{"a": {}, memGone: {}}, true)
			Expect(plan.Revoke).To(Equal([]string{memGone}))
		})

		It("keeps a manual (unmanaged) membership", func() {
			plan := diffMemberships([]rm{{Role: "a"}}, []string{"a"},
				map[string]st{"a": {}, "manual": {}}, true)
			Expect(plan.Revoke).To(BeEmpty())
		})

		It("skips revoking a managed membership that no longer exists", func() {
			plan := diffMemberships(nil, []string{"dropped"}, map[string]st{}, true)
			Expect(plan.Revoke).To(BeEmpty())
		})

		It("does not revoke when revokeRemovedMemberships is false", func() {
			plan := diffMemberships(nil, []string{memGone}, map[string]st{memGone: {}}, false)
			Expect(plan.Revoke).To(BeEmpty())
		})

		It("treats memberOf entries like memberships without options", func() {
			spec := postgresv1alpha1.RoleSpec{MemberOf: []string{memLegacy}, Memberships: []rm{{Role: "new", Admin: true}}} //nolint:staticcheck // deprecated field under test
			plan := diffMemberships(spec.DesiredMemberships(), []string{memLegacy, memOld},
				map[string]st{memLegacy: {Inherit: new(true), Set: new(true)}, memOld: {}}, true)
			Expect(plan.Grant).To(Equal([]membershipGrant{{Role: "new", Opts: postgres.MembershipOptions{Admin: true}}}))
			Expect(plan.Revoke).To(Equal([]string{memOld}))
		})
	})

	Describe("reconcileMemberships", func() {
		ctx := context.Background()
		newRole := func(spec postgresv1alpha1.RoleSpec, managed ...string) *postgresv1alpha1.Role {
			r := &postgresv1alpha1.Role{Spec: spec}
			r.Status.ManagedMemberships = managed
			return r
		}

		It("is a no-op without memberships", func() {
			f := &fakeMembershipClient{version: 180000}
			role := newRole(postgresv1alpha1.RoleSpec{})
			Expect(reconcileMemberships(ctx, f, role, "app", nil, testManaged)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(role.Status.ManagedMemberships).To(BeNil())
		})

		It("grants, revokes and records the desired set", func() {
			f := &fakeMembershipClient{version: 180000, current: map[string]st{memGone: {}, "manual": {}}}
			role := newRole(postgresv1alpha1.RoleSpec{Memberships: []rm{{Role: "a"}}}, memGone)
			Expect(reconcileMemberships(ctx, f, role, "app", nil, testManaged)).To(Succeed())
			Expect(f.calls).To(Equal([]string{polGrantA, "revoke gone from app"}))
			Expect(role.Status.ManagedMemberships).To(Equal([]string{"a"}))
		})

		It("stops tracking removed memberships without revoking when opted out", func() {
			f := &fakeMembershipClient{version: 180000, current: map[string]st{memGone: {}}}
			role := newRole(postgresv1alpha1.RoleSpec{RevokeRemovedMemberships: new(false)}, memGone)
			Expect(reconcileMemberships(ctx, f, role, "app", nil, testManaged)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(role.Status.ManagedMemberships).To(BeNil())
		})

		It("keeps pending revokes and records successful grants on failure", func() {
			f := &fakeMembershipClient{version: 180000, current: map[string]st{memGone: {}}, failOn: "revoke:gone"}
			role := newRole(postgresv1alpha1.RoleSpec{Memberships: []rm{{Role: "a"}}}, memGone)
			Expect(reconcileMemberships(ctx, f, role, "app", nil, testManaged)).NotTo(Succeed())
			Expect(role.Status.ManagedMemberships).To(Equal([]string{"a", memGone}))
		})

		It("does not track a grant that failed", func() {
			f := &fakeMembershipClient{version: 180000, failOn: "grant:b"}
			role := newRole(postgresv1alpha1.RoleSpec{Memberships: []rm{{Role: "a"}, {Role: "b"}}})
			Expect(reconcileMemberships(ctx, f, role, "app", nil, testManaged)).NotTo(Succeed())
			Expect(role.Status.ManagedMemberships).To(Equal([]string{"a"}))
		})

		It("fails clearly when inherit/set are used before PostgreSQL 16", func() {
			f := &fakeMembershipClient{version: 150004}
			role := newRole(postgresv1alpha1.RoleSpec{Memberships: []rm{{Role: "a", Inherit: new(false)}}})
			err := reconcileMemberships(ctx, f, role, "app", nil, testManaged)
			Expect(err).To(MatchError(ContainSubstring("require PostgreSQL 16 or later")))
			Expect(f.calls).To(BeEmpty())
		})

		It("refuses forbidden memberships, applies the others and revokes forbidden managed ones", func() {
			f := &fakeMembershipClient{
				version: 180000,
				current: map[string]st{polMonitor: {}, polOps: {}},
				closures: map[string][]postgres.ReachableRole{
					polOps:         {{Name: polOps, Comment: ownerMarker(markerKindRole, polOps)}, {Name: polDBA, Via: polDBA, Superuser: true}},
					grantTestAdmin: {{Name: grantTestAdmin, Superuser: true}},
				},
			}
			role := newRole(postgresv1alpha1.RoleSpec{
				MemberOf:    []string{"a", polMonitor, polOps, grantTestAdmin}, //nolint:staticcheck // deprecated field under test
				Memberships: []rm{{Role: polExecProgram}, {Role: "pgop_operator"}},
			}, polMonitor, polOps)
			err := reconcileMemberships(ctx, f, role, "app", nil, testManaged)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue())
			Expect(ce.reason).To(Equal(ReasonMembershipNotAllowed))
			Expect(err.Error()).To(SatisfyAll(
				ContainSubstring("pg_execute_server_program gives access to the server's files"),
				ContainSubstring("pgop_operator is reserved"),
				ContainSubstring("pg_monitor is a predefined role"),
				ContainSubstring("ops is a member of dba, which is a superuser"),
				ContainSubstring("admin is a superuser"),
				ContainSubstring("revoked the membership(s) pgop had granted in pg_monitor, ops"),
			))
			Expect(f.calls).To(Equal([]string{polGrantA, "revoke ops from app", "revoke pg_monitor from app"}))
			Expect(role.Status.ManagedMemberships).To(Equal([]string{"a"}))
		})

		It("revokes a forbidden managed membership even when revokeRemovedMemberships is false", func() {
			f := &fakeMembershipClient{version: 180000, current: map[string]st{polReadAllData: {}}}
			role := newRole(postgresv1alpha1.RoleSpec{RevokeRemovedMemberships: new(false)}, polReadAllData)
			Expect(reconcileMemberships(ctx, f, role, "app", nil, testManaged)).To(Succeed())
			Expect(f.calls).To(Equal([]string{"revoke pg_read_all_data from app"}))
			Expect(role.Status.ManagedMemberships).To(BeNil())
		})

		It("grants predefined roles the Cluster policy allows", func() {
			f := &fakeMembershipClient{version: 180000}
			policy := &postgresv1alpha1.RolePolicySpec{AllowedPredefinedRoles: []string{polMonitor}}
			role := newRole(postgresv1alpha1.RoleSpec{Memberships: []rm{{Role: polMonitor}}})
			Expect(reconcileMemberships(ctx, f, role, "app", policy, testManaged)).To(Succeed())
			Expect(f.calls).To(Equal([]string{"grant pg_monitor to app"}))
			Expect(role.Status.ManagedMemberships).To(Equal([]string{polMonitor}))
		})

		It("does not adopt an existing forbidden membership granted outside pgop", func() {
			f := &fakeMembershipClient{version: 180000, current: map[string]st{"pg_write_all_data": {}}}
			role := newRole(postgresv1alpha1.RoleSpec{Memberships: []rm{{Role: "pg_write_all_data"}}})
			err := reconcileMemberships(ctx, f, role, "app", nil, testManaged)
			Expect(err).To(MatchError(ContainSubstring("pg_write_all_data is a predefined role")))
			Expect(f.calls).To(BeEmpty())
			Expect(role.Status.ManagedMemberships).To(BeNil())
		})

		It("refuses roles no Role of the Cluster manages unless the policy lists them, and revokes them", func() {
			f := &fakeMembershipClient{
				version: 180000,
				current: map[string]st{polAnalytics: {}},
				closures: map[string][]postgres.ReachableRole{
					polAnalytics:    {{Name: polAnalytics}},
					polOtherCluster: {{Name: polOtherCluster, Comment: ownerMarker(markerKindRole, "role-of-another-cluster")}},
					polWrapsDBA:     {{Name: polWrapsDBA, Comment: ownerMarker(markerKindRole, polWrapsDBA)}, {Name: polAnalytics, Via: polAnalytics}},
				},
			}
			role := newRole(postgresv1alpha1.RoleSpec{Memberships: []rm{
				{Role: polAnalytics}, {Role: polOtherCluster}, {Role: polWrapsDBA}, {Role: "tenant_b"},
			}}, polAnalytics)
			err := reconcileMemberships(ctx, f, role, "app", nil, testManaged)
			Expect(err).To(MatchError(SatisfyAll(
				ContainSubstring("analytics_ro is not managed by a Role of this Cluster"),
				ContainSubstring("other_cluster is not managed by a Role of this Cluster"),
				ContainSubstring("wraps_dba is a member of analytics_ro, which is not managed"),
			)))
			Expect(f.calls).To(Equal([]string{"grant tenant_b to app", "revoke analytics_ro from app"}))

			By("allowing it through allowedExistingRoles")
			f.calls = nil
			policy := &postgresv1alpha1.RolePolicySpec{AllowedExistingRoles: []string{polAnalytics}}
			role.Spec.Memberships = []rm{{Role: polAnalytics}, {Role: polWrapsDBA}}
			delete(f.current, polAnalytics)
			Expect(reconcileMemberships(ctx, f, role, "app", policy, testManaged)).To(Succeed())
			Expect(f.calls).To(Equal([]string{"grant analytics_ro to app", "grant wraps_dba to app"}))
		})

		It("allows plain and admin memberships before PostgreSQL 16", func() {
			f := &fakeMembershipClient{version: 150004}
			role := newRole(postgresv1alpha1.RoleSpec{MemberOf: []string{"a"}, Memberships: []rm{{Role: "b", Admin: true}}}) //nolint:staticcheck // deprecated field under test
			Expect(reconcileMemberships(ctx, f, role, "app", nil, testManaged)).To(Succeed())
			Expect(f.calls).To(Equal([]string{"grant b to app", polGrantA}))
		})
	})
})
