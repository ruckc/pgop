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
	"strings"
)

// privilegeGrant is a set of privileges on one object, identified by Key:
// the grantee role for database grants (the database is fixed), the
// parameter for parameter grants (the grantee is fixed).
type privilegeGrant struct {
	Key string
	// Privileges are canonical (upper case, sorted, no duplicates).
	Privileges      []string
	WithGrantOption bool
}

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
		want[d.Key] = d
	}
	for _, m := range managed {
		d, ok := want[m.Key]
		if !ok {
			plan.Revoke = append(plan.Revoke, m)
			continue
		}
		if removed := subtract(m.Privileges, d.Privileges); len(removed) > 0 {
			plan.Revoke = append(plan.Revoke, privilegeGrant{Key: m.Key, Privileges: removed})
		}
		if m.WithGrantOption && !d.WithGrantOption {
			if kept := intersect(m.Privileges, d.Privileges); len(kept) > 0 {
				plan.RevokeGrantOption = append(plan.RevokeGrantOption, privilegeGrant{Key: m.Key, Privileges: kept})
			}
		}
	}
	return plan
}

// privilegeOps issues the GRANT and REVOKE statements for one object type.
type privilegeOps struct {
	grant  func(ctx context.Context, g privilegeGrant) error
	revoke func(ctx context.Context, g privilegeGrant, grantOptionOnly bool) error
}

// applyPrivilegeGrants brings the managed grants to desired and returns the
// grants pgop manages afterwards, to be recorded in status. On an error the
// returned list still records the grants made so far and keeps pending
// revokes, so nothing pgop granted is forgotten.
func applyPrivilegeGrants(ctx context.Context, desired, managed []privilegeGrant, ops privilegeOps) ([]privilegeGrant, error) {
	plan := diffPrivilegeGrants(desired, managed)

	tracked := make(map[string]privilegeGrant, len(managed))
	for _, m := range managed {
		tracked[m.Key] = m
	}
	snapshot := func() []privilegeGrant { return sortedGrants(tracked) }

	for _, g := range plan.Grant {
		if err := ops.grant(ctx, g); err != nil {
			return snapshot(), err
		}
		t := tracked[g.Key]
		tracked[g.Key] = privilegeGrant{
			Key:             g.Key,
			Privileges:      union(t.Privileges, g.Privileges),
			WithGrantOption: t.WithGrantOption || g.WithGrantOption,
		}
	}
	for _, g := range plan.RevokeGrantOption {
		if err := ops.revoke(ctx, g, true); err != nil {
			return snapshot(), err
		}
		t := tracked[g.Key]
		t.WithGrantOption = false
		tracked[g.Key] = t
	}
	for _, g := range plan.Revoke {
		if err := ops.revoke(ctx, g, false); err != nil {
			return snapshot(), err
		}
		t := tracked[g.Key]
		t.Privileges = subtract(t.Privileges, g.Privileges)
		if len(t.Privileges) == 0 {
			delete(tracked, g.Key)
		} else {
			tracked[g.Key] = t
		}
	}

	// Everything succeeded: pgop now manages exactly the desired grants.
	tracked = make(map[string]privilegeGrant, len(desired))
	for _, d := range desired {
		tracked[d.Key] = d
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
	slices.SortFunc(out, func(a, b privilegeGrant) int { return strings.Compare(a.Key, b.Key) })
	return out
}
