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
	"strings"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Grantee policy.
//
// pgop grants privileges as a superuser, so the grantees a Database writer
// names in spec.grants and spec.schemas[].grants are checked like membership
// targets: a grantee must be
//
//   - the PUBLIC pseudo-role,
//   - a role managed by a Role of the same Cluster (recorded in that Role's
//     status on this Cluster and carrying its ownership marker), or
//   - a role a Cluster editor listed in spec.rolePolicy.allowedExistingRoles,
//
// and is never a superuser, postgres, a pgop_* role or a predefined pg_*
// role. Everyone who can write Roles and Databases for a Cluster is one trust
// domain; roles created outside pgop (by a DBA, a bootstrap Job, another
// Cluster's restore) are another, which only a Cluster editor can open up.

// granteeVerdict is what pgop does with grants to a grantee.
type granteeVerdict int

const (
	// granteeAllowed: grant.
	granteeAllowed granteeVerdict = iota
	// granteeMissing: the role does not exist (yet); retry later.
	granteeMissing
	// granteePaused: the grantee's Role is being deleted.
	granteePaused
	// granteeRefused: the policy does not allow the grantee.
	granteeRefused
)

// granteeNameProblem explains why the grantee name is never allowed, or
// returns "" when its name alone does not rule it out.
func granteeNameProblem(name string) string {
	switch {
	case postgres.IsPublic(name):
		return ""
	case name == bootstrapRoleName:
		return "is reserved (bootstrap superuser)"
	case strings.HasPrefix(name, reservedRolePrefix):
		return "is reserved for the operator"
	case strings.HasPrefix(name, "pg_"):
		return "is a predefined role"
	case strings.EqualFold(name, "none"):
		return "is a reserved role name"
	}
	return ""
}

// granteeProblem explains why the existing role r may not be a grantee, or
// returns "" when it may.
func granteeProblem(r postgres.ReachableRole, policy *postgresv1alpha1.RolePolicySpec, managed managedRoles) string {
	if p := granteeNameProblem(r.Name); p != "" {
		return p
	}
	if r.Superuser {
		return "is a superuser"
	}
	if managed.manages(r) || policy.AllowsExistingRole(r.Name) {
		return ""
	}
	return "is not managed by a Role of this Cluster and the Cluster's spec.rolePolicy.allowedExistingRoles does not list it"
}

// granteeClient is the subset of *postgres.Client the grantee policy uses.
type granteeClient interface {
	LookupRole(ctx context.Context, name string) (*postgres.ReachableRole, error)
}

var _ granteeClient = (*postgres.Client)(nil)

// granteeResult is the cached decision for one grantee.
type granteeResult struct {
	verdict granteeVerdict
	problem string
}

// granteeChecker decides, with the Cluster's role policy, which grantees pgop
// grants to. Results are cached for one reconcile.
type granteeChecker struct {
	pg     granteeClient
	policy *postgresv1alpha1.RolePolicySpec
	// managed lists the roles the Cluster's Roles manage.
	managed managedRoles
	// deleting lists the PostgreSQL names of the Cluster's Roles that are
	// being deleted: grants to them are paused so the Role controller can
	// revoke them and drop the role.
	deleting map[string]bool
	cache    map[string]granteeResult
}

// check returns the decision for grantee.
func (c *granteeChecker) check(ctx context.Context, grantee string) (granteeResult, error) {
	if res, ok := c.cache[grantee]; ok {
		return res, nil
	}
	res, err := c.decide(ctx, grantee)
	if err != nil {
		return granteeResult{}, err
	}
	if c.cache == nil {
		c.cache = map[string]granteeResult{}
	}
	c.cache[grantee] = res
	return res, nil
}

func (c *granteeChecker) decide(ctx context.Context, grantee string) (granteeResult, error) {
	if postgres.IsPublic(grantee) {
		return granteeResult{verdict: granteeAllowed}, nil
	}
	if p := granteeNameProblem(grantee); p != "" {
		return granteeResult{verdict: granteeRefused, problem: p}, nil
	}
	if c.deleting[grantee] {
		return granteeResult{verdict: granteePaused}, nil
	}
	r, err := c.pg.LookupRole(ctx, grantee)
	if err != nil {
		return granteeResult{}, err
	}
	if r == nil {
		return granteeResult{verdict: granteeMissing}, nil
	}
	if p := granteeProblem(*r, c.policy, c.managed); p != "" {
		return granteeResult{verdict: granteeRefused, problem: p}, nil
	}
	return granteeResult{verdict: granteeAllowed}, nil
}

// grantFilter collects the grants that were held back and why, for one
// reconcile of one or more grant lists.
type grantFilter struct {
	checker *granteeChecker
	missing []string
	paused  []string
	refused []string
}

// filter returns the grants of desired that pgop may apply. field names the
// spec field in messages (for example "grants" or "schemas[app].grants").
// Grants to grantees that are refused, missing or paused are left out; when
// pgop granted them earlier, the engine revokes them (a missing or dropped
// role holds nothing to revoke).
func (f *grantFilter) filter(ctx context.Context, field string, desired []privilegeGrant) ([]privilegeGrant, error) {
	out := make([]privilegeGrant, 0, len(desired))
	for _, g := range desired {
		res, err := f.checker.check(ctx, g.Target.Grantee)
		if err != nil {
			return nil, err
		}
		switch res.verdict {
		case granteeAllowed:
			out = append(out, g)
		case granteeMissing:
			f.missing = append(f.missing, fmt.Sprintf("%s: PostgreSQL role %q does not exist yet", field, g.Target.Grantee))
		case granteePaused:
			f.paused = append(f.paused, fmt.Sprintf("%s: grants to %s are paused: the Role is being deleted", field, g.Target.Grantee))
		case granteeRefused:
			f.refused = append(f.refused, fmt.Sprintf("%s: %s %s", field, g.Target.Grantee, res.problem))
		}
	}
	return out, nil
}

// err reports the held-back grants: refused grantees with reason
// GranteeNotAllowed, missing and paused ones as plain errors. It returns nil
// when nothing was held back.
func (f *grantFilter) err() error {
	var errs []error
	if len(f.refused) > 0 {
		errs = append(errs, &conditionError{reason: ReasonGranteeNotAllowed, err: fmt.Errorf(
			"grantees not allowed (grants to them are not applied, and revoked if pgop granted them): %s",
			strings.Join(f.refused, "; "))})
	}
	for _, m := range [][]string{f.missing, f.paused} {
		if len(m) > 0 {
			errs = append(errs, errors.New(strings.Join(m, "; ")))
		}
	}
	return errors.Join(errs...)
}
