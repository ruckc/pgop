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
	"slices"
	"strconv"
	"strings"

	"github.com/ruckc/pgop/internal/postgres"
)

// This file is the shared grant-tracking engine. Every kind of privilege
// grant pgop manages (database grants, schema grants, parameter grants; later
// object grants and default privileges) goes through applyPrivilegeGrants:
//
//   - every desired grant is issued on every reconcile (GRANT is idempotent),
//     which also repairs privileges revoked outside pgop;
//   - a privilege is only ever revoked when the resource's status ledger
//     (status.managed*) records that pgop granted it: privileges granted
//     outside pgop are never revoked, and a lost status means removed entries
//     are not revoked either;
//   - privileges pgop granted WITH GRANT OPTION are revoked with CASCADE, so
//     privileges the grantee passed on go with them.
//
// Ledger keys are exclusive to one resource by construction: a key names the
// object and the grantee, and every object a key can name belongs to exactly
// one resource. Database and schema grants are on the Database's own
// PostgreSQL database (two Databases never manage the same database: the
// younger one reports DuplicateDatabaseName and touches nothing), parameter
// grants name the Role's own role as grantee (DuplicateRoleName likewise).
// So no two resources ever track the same privilege, and one resource's
// revoke cannot take away a privilege another resource declared. Should two
// resources ever overlap anyway (a kind added later whose objects are
// shared), the rule is "grant wins": each resource re-grants what it
// declares on every reconcile, so a revoke by one is undone by the next
// reconcile of the other.

// grantTarget identifies one tracked grant: privileges of one kind of object,
// on one object, to one grantee. It is the ledger key; future kinds (tables,
// functions, default privileges) add the fields they need to it.
type grantTarget struct {
	Kind postgres.ObjectKind
	// Name is the object: a database, schema or parameter name.
	Name string
	// Grantee is a role name, or postgres.PublicGrantee.
	Grantee string
}

// key returns the target's stable, unambiguous ledger key.
func (t grantTarget) key() string {
	return string(t.Kind) + "|" + strconv.Quote(t.Name) + "|" + strconv.Quote(t.Grantee)
}

// object returns the PostgreSQL object the target's privileges are on.
func (t grantTarget) object() postgres.PrivilegeObject {
	return postgres.PrivilegeObject{Kind: t.Kind, Name: t.Name}
}

// privilegeGrant is a set of privileges on one target.
type privilegeGrant struct {
	Target grantTarget
	// Privileges are canonical (upper case, sorted, no duplicates).
	Privileges      []string
	WithGrantOption bool
}

func (g privilegeGrant) key() string { return g.Target.key() }

// privilegePlan is the set of statements that brings the privileges pgop
// manages to the desired state.
type privilegePlan struct {
	// Grant lists every desired grant. GRANT is idempotent, so issuing all
	// of them also restores privileges revoked outside pgop.
	Grant []privilegeGrant
	// RevokeGrantOption lists privileges that stay granted but whose grant
	// option pgop granted and is no longer desired.
	RevokeGrantOption []privilegeGrant
	// Revoke lists privileges pgop granted that are no longer desired.
	// WithGrantOption is set when pgop granted them with the grant option,
	// so the REVOKE must cascade to privileges the grantee passed on.
	Revoke []privilegeGrant
}

// subtract returns the elements of a that are not in b.
func subtract(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// intersect returns the elements of a that are also in b.
func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// union returns the sorted union of a and b without duplicates.
func union(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// diffPrivilegeGrants computes the statements needed to reach desired, given
// the grants pgop manages (recorded in status). Only managed privileges are
// ever revoked; privileges granted outside pgop are left alone.
func diffPrivilegeGrants(desired, managed []privilegeGrant) privilegePlan {
	plan := privilegePlan{Grant: desired}
	want := make(map[string]privilegeGrant, len(desired))
	for _, d := range desired {
		want[d.key()] = d
	}
	for _, m := range managed {
		d, ok := want[m.key()]
		if !ok {
			plan.Revoke = append(plan.Revoke, m)
			continue
		}
		if removed := subtract(m.Privileges, d.Privileges); len(removed) > 0 {
			plan.Revoke = append(plan.Revoke, privilegeGrant{Target: m.Target, Privileges: removed, WithGrantOption: m.WithGrantOption})
		}
		if m.WithGrantOption && !d.WithGrantOption {
			if kept := intersect(m.Privileges, d.Privileges); len(kept) > 0 {
				plan.RevokeGrantOption = append(plan.RevokeGrantOption, privilegeGrant{Target: m.Target, Privileges: kept})
			}
		}
	}
	return plan
}

// privilegeOps issues the GRANT and REVOKE statements for the engine.
type privilegeOps struct {
	grant  func(ctx context.Context, g privilegeGrant) error
	revoke func(ctx context.Context, g privilegeGrant, mode postgres.RevokeMode) error
}

// privilegeExecutor is the subset of *postgres.Client that issues GRANT and
// REVOKE statements on any supported object kind.
type privilegeExecutor interface {
	GrantPrivileges(ctx context.Context, obj postgres.PrivilegeObject, grantee string, privileges []string, withGrantOption bool) error
	RevokePrivileges(ctx context.Context, obj postgres.PrivilegeObject, grantee string, privileges []string, mode postgres.RevokeMode) error
}

var _ privilegeExecutor = (*postgres.Client)(nil)

// executorOps returns the ops that grant and revoke on each target's own
// object and grantee. gone, when set, reports that a target's grantee or
// object no longer exists: its revoke is then skipped (a dropped role or
// object holds no privileges any more) and the entry leaves the ledger.
func executorOps(pg privilegeExecutor, gone func(ctx context.Context, t grantTarget) (bool, error)) privilegeOps {
	return privilegeOps{
		grant: func(ctx context.Context, g privilegeGrant) error {
			return pg.GrantPrivileges(ctx, g.Target.object(), g.Target.Grantee, g.Privileges, g.WithGrantOption)
		},
		revoke: func(ctx context.Context, g privilegeGrant, mode postgres.RevokeMode) error {
			if gone != nil {
				if missing, err := gone(ctx, g.Target); err != nil || missing {
					return err
				}
			}
			return pg.RevokePrivileges(ctx, g.Target.object(), g.Target.Grantee, g.Privileges, mode)
		},
	}
}

// applyPrivilegeGrants brings the managed grants to desired and returns the
// grants pgop manages afterwards, to be recorded in status. On an error the
// returned list still records the grants made so far and keeps pending
// revokes, so nothing pgop granted is forgotten.
func applyPrivilegeGrants(ctx context.Context, desired, managed []privilegeGrant, ops privilegeOps) ([]privilegeGrant, error) {
	plan := diffPrivilegeGrants(desired, managed)

	tracked := make(map[string]privilegeGrant, len(managed))
	for _, m := range managed {
		tracked[m.key()] = m
	}
	snapshot := func() []privilegeGrant { return sortedGrants(tracked) }

	for _, g := range plan.Grant {
		if err := ops.grant(ctx, g); err != nil {
			return snapshot(), err
		}
		t := tracked[g.key()]
		tracked[g.key()] = privilegeGrant{
			Target:          g.Target,
			Privileges:      union(t.Privileges, g.Privileges),
			WithGrantOption: t.WithGrantOption || g.WithGrantOption,
		}
	}
	for _, g := range plan.RevokeGrantOption {
		// The grant option was granted by pgop, so the grantee may have
		// passed the privileges on: cascade, or the REVOKE fails forever.
		if err := ops.revoke(ctx, g, postgres.RevokeMode{GrantOptionOnly: true, Cascade: true}); err != nil {
			return snapshot(), err
		}
		t := tracked[g.key()]
		t.WithGrantOption = false
		tracked[g.key()] = t
	}
	for _, g := range plan.Revoke {
		if err := ops.revoke(ctx, g, postgres.RevokeMode{Cascade: g.WithGrantOption}); err != nil {
			return snapshot(), err
		}
		t := tracked[g.key()]
		t.Privileges = subtract(t.Privileges, g.Privileges)
		if len(t.Privileges) == 0 {
			delete(tracked, g.key())
		} else {
			tracked[g.key()] = t
		}
	}

	// Everything succeeded: pgop now manages exactly the desired grants.
	tracked = make(map[string]privilegeGrant, len(desired))
	for _, d := range desired {
		tracked[d.key()] = d
	}
	return snapshot(), nil
}

// sortedGrants returns the grants in m sorted by key, or nil when m is empty.
func sortedGrants(m map[string]privilegeGrant) []privilegeGrant {
	if len(m) == 0 {
		return nil
	}
	out := make([]privilegeGrant, 0, len(m))
	for _, g := range m {
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b privilegeGrant) int { return strings.Compare(a.key(), b.key()) })
	return out
}
