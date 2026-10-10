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
	"slices"
	"strings"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// membershipClient is the subset of *postgres.Client used to reconcile role
// memberships; it is an interface so the logic can be tested without a server.
type membershipClient interface {
	ServerVersionNum(ctx context.Context) (int, error)
	ListMemberships(ctx context.Context, member string, serverVersion int) (map[string]postgres.MembershipState, error)
	GrantRole(ctx context.Context, role, member string, opts postgres.MembershipOptions) error
	RevokeAdminOption(ctx context.Context, role, member string) error
	RevokeRole(ctx context.Context, role, member string) error
	MembershipClosure(ctx context.Context, name string) ([]postgres.ReachableRole, error)
}

var _ membershipClient = (*postgres.Client)(nil)

// membershipGrant is a GRANT to issue for a desired membership.
type membershipGrant struct {
	Role string
	Opts postgres.MembershipOptions
}

// membershipPlan is the set of statements that brings the current memberships
// to the desired state.
type membershipPlan struct {
	// Grant lists memberships that are missing or whose options differ.
	Grant []membershipGrant
	// RevokeAdmin lists desired memberships that have the ADMIN option
	// although admin is false.
	RevokeAdmin []string
	// Revoke lists memberships pgop granted that are no longer desired.
	Revoke []string
}

// optionDiffers reports whether a requested option (nil = don't care) differs
// from the current one.
func optionDiffers(want, have *bool) bool {
	return want != nil && (have == nil || *have != *want)
}

// diffMemberships computes the statements needed to reach desired, given the
// memberships pgop manages (status.managedMemberships) and the current grants.
// Only managed memberships that are no longer desired and still exist are
// revoked, and only when revokeRemoved is true; grants pgop never managed are
// never revoked.
func diffMemberships(desired []postgresv1alpha1.RoleMembership, managed []string,
	current map[string]postgres.MembershipState, revokeRemoved bool) membershipPlan {
	var plan membershipPlan
	want := make(map[string]bool, len(desired))
	for _, d := range desired {
		want[d.Role] = true
		cur, exists := current[d.Role]
		if !exists || (d.Admin && !cur.Admin) || optionDiffers(d.Inherit, cur.Inherit) || optionDiffers(d.Set, cur.Set) {
			plan.Grant = append(plan.Grant, membershipGrant{
				Role: d.Role,
				Opts: postgres.MembershipOptions{Admin: d.Admin, Inherit: d.Inherit, Set: d.Set},
			})
		}
		if exists && cur.Admin && !d.Admin {
			plan.RevokeAdmin = append(plan.RevokeAdmin, d.Role)
		}
	}
	if revokeRemoved {
		for _, m := range managed {
			if _, exists := current[m]; exists && !want[m] {
				plan.Revoke = append(plan.Revoke, m)
			}
		}
		slices.Sort(plan.Revoke)
		plan.Revoke = slices.Compact(plan.Revoke)
	}
	return plan
}

// membershipChecker decides, with the Cluster's role policy, whether
// membership in a role is allowed. Results are cached for one reconcile.
type membershipChecker struct {
	pg     membershipClient
	policy *postgresv1alpha1.RolePolicySpec
	// managed lists the roles the Cluster's Roles manage.
	managed managedRoles
	cache   map[string]string
}

// problem explains why membership in target is not allowed, or returns ""
// when it is.
func (c *membershipChecker) problem(ctx context.Context, target string) (string, error) {
	if p, ok := c.cache[target]; ok {
		return p, nil
	}
	p := membershipNameProblem(target, c.policy)
	if p != "" {
		p = target + " " + p
	} else {
		closure, err := c.pg.MembershipClosure(ctx, target)
		if err != nil {
			return "", err
		}
		p = membershipProblem(target, closure, c.policy, c.managed)
	}
	if c.cache == nil {
		c.cache = map[string]string{}
	}
	c.cache[target] = p
	return p, nil
}

// reconcileMemberships brings member's role memberships to the state declared
// by role.Spec and records the managed memberships in
// role.Status.ManagedMemberships (the caller persists the status).
//
// Memberships the policy does not allow (see membershipProblem) are not
// granted and are reported with reason MembershipNotAllowed; the others are
// still applied. A forbidden membership that pgop granted earlier (tracked in
// status.managedMemberships) is revoked, whatever revokeRemovedMemberships
// says.
func reconcileMemberships(ctx context.Context, pg membershipClient, role *postgresv1alpha1.Role, member string,
	policy *postgresv1alpha1.RolePolicySpec, clusterRoles managedRoles) error {
	requested := role.Spec.DesiredMemberships()
	managed := role.Status.ManagedMemberships
	if len(requested) == 0 && len(managed) == 0 {
		return nil
	}

	checker := &membershipChecker{pg: pg, policy: policy, managed: clusterRoles}
	desired := make([]postgresv1alpha1.RoleMembership, 0, len(requested))
	var refused []string
	for _, d := range requested {
		p, err := checker.problem(ctx, d.Role)
		if err != nil {
			return err
		}
		if p != "" {
			refused = append(refused, p)
			continue
		}
		desired = append(desired, d)
	}
	var forbiddenManaged []string
	for _, m := range managed {
		p, err := checker.problem(ctx, m)
		if err != nil {
			return err
		}
		if p != "" {
			forbiddenManaged = append(forbiddenManaged, m)
		}
	}

	version, err := pg.ServerVersionNum(ctx)
	if err != nil {
		return err
	}
	if version < postgres.MinMembershipOptionsVersion {
		for _, d := range desired {
			if d.Inherit != nil || d.Set != nil {
				return fmt.Errorf("memberships[%s]: the inherit and set options require PostgreSQL 16 or later "+
					"(server_version_num %d); remove them or upgrade the cluster", d.Role, version)
			}
		}
	}

	current, err := pg.ListMemberships(ctx, member, version)
	if err != nil {
		return err
	}

	plan := diffMemberships(desired, managed, current, role.Spec.ShouldRevokeRemovedMemberships())
	var revokedForbidden []string
	for _, m := range forbiddenManaged {
		if _, exists := current[m]; exists {
			revokedForbidden = append(revokedForbidden, m)
		}
	}
	plan.Revoke = append(plan.Revoke, revokedForbidden...)
	slices.Sort(plan.Revoke)
	plan.Revoke = slices.Compact(plan.Revoke)

	// Track what pgop manages as statements succeed, so a partial failure
	// still records grants that were made and keeps revokes that are pending.
	tracked := map[string]bool{}
	for _, m := range managed {
		tracked[m] = true
	}
	for _, d := range desired {
		if _, exists := current[d.Role]; exists {
			tracked[d.Role] = true
		}
	}
	record := func() {
		role.Status.ManagedMemberships = sortedKeys(tracked)
	}

	for _, g := range plan.Grant {
		if err := pg.GrantRole(ctx, g.Role, member, g.Opts); err != nil {
			record()
			return err
		}
		tracked[g.Role] = true
	}
	for _, r := range plan.RevokeAdmin {
		if err := pg.RevokeAdminOption(ctx, r, member); err != nil {
			record()
			return err
		}
	}
	for _, r := range plan.Revoke {
		if err := pg.RevokeRole(ctx, r, member); err != nil {
			record()
			return err
		}
		delete(tracked, r)
	}

	// Everything succeeded: pgop now manages exactly the desired (allowed)
	// memberships. With revokeRemovedMemberships=false, removed ones are
	// dropped from tracking without being revoked.
	tracked = make(map[string]bool, len(desired))
	for _, d := range desired {
		tracked[d.Role] = true
	}
	record()

	if len(refused) == 0 {
		return nil
	}
	msg := "memberships not allowed: " + strings.Join(refused, "; ")
	if len(revokedForbidden) > 0 {
		msg += "; revoked the membership(s) pgop had granted in " + strings.Join(revokedForbidden, ", ")
	}
	return &conditionError{reason: ReasonMembershipNotAllowed, err: errors.New(msg)}
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(m))
}
