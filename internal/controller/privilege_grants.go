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
	"strconv"
	"strings"

	"github.com/ruckc/pgop/internal/postgres"
)

// This file is the shared grant-tracking engine. Every kind of privilege
// grant pgop manages (database grants, schema grants, parameter grants; later
// object grants and default privileges) goes through applyPrivilegeGrants:
//
//   - on every reconcile, each declared grant is compared with what the
//     grantee holds directly on the object (granted by the object's owner,
//     which is the grantor of every GRANT a superuser issues, PostgreSQL's
//     defaults included). Only what is missing is granted, which also repairs
//     privileges revoked outside pgop;
//   - the resource's status ledger (status.managed*) records only what pgop
//     added: privileges the grantee did not hold, and grant options it did
//     not have. A declared privilege the grantee already held (a PostgreSQL
//     default such as PUBLIC's USAGE on the schema public, the owner's own
//     privileges, or a grant made outside pgop) is never recorded, so
//     removing it from the spec never revokes it;
//   - a privilege is only ever revoked when the ledger records it. Revoking a
//     grant option pgop added cascades to what the grantee passed on. A plain
//     revoke that PostgreSQL refuses because the grantee passed the privilege
//     on with a grant option pgop did not give is not forced: it is dropped
//     from the ledger and reported (RevokeSkipped). One failing statement
//     does not hold up the others;
//   - the ledger never grows past its status limit: a change that would make
//     it is refused (TooManyGrants) before anything is applied, so the ledger
//     is never lost to a rejected status update.
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
// declares and finds missing on every reconcile, so a revoke by one is
// undone by the next reconcile of the other.

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

func (t grantTarget) String() string {
	return fmt.Sprintf("%s to %s", t.object(), t.Grantee)
}

// privilegeGrant is a set of privileges on one target. In a desired grant,
// Privileges are the declared privileges and GrantOptions those declared
// WITH GRANT OPTION (all of them or none). In a ledger entry, Privileges are
// the privileges pgop added and GrantOptions the grant options it added.
// Both are canonical (upper case, sorted, no duplicates).
type privilegeGrant struct {
	Target       grantTarget
	Privileges   []string
	GrantOptions []string
}

func (g privilegeGrant) key() string { return g.Target.key() }

func (g privilegeGrant) empty() bool { return len(g.Privileges) == 0 && len(g.GrantOptions) == 0 }

// desiredGrant returns the desired grant of privileges on t, with the grant
// option for all of them when withGrantOption is set.
func desiredGrant(t grantTarget, privileges []string, withGrantOption bool) privilegeGrant {
	g := privilegeGrant{Target: t, Privileges: privileges}
	if withGrantOption {
		g.GrantOptions = slices.Clone(privileges)
	}
	return g
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
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// privilegeOps reads and changes privileges for the engine.
type privilegeOps struct {
	// held returns the privileges the target's grantee holds directly on its
	// object from the object's owner, and those it holds with the grant
	// option.
	held func(ctx context.Context, t grantTarget) (privileges, grantable []string, err error)
	// grant grants privileges on the target's object to its grantee.
	grant func(ctx context.Context, t grantTarget, privileges []string, withGrantOption bool) error
	// revoke revokes privileges (or their grant option) from the grantee.
	revoke func(ctx context.Context, t grantTarget, privileges []string, mode postgres.RevokeMode) error
	// gone, when set, reports that a target's grantee or object no longer
	// exists: its revokes are skipped (a dropped role or object holds no
	// privileges any more) and the entry leaves the ledger.
	gone func(ctx context.Context, t grantTarget) (bool, error)
}

// privilegeExecutor is the subset of *postgres.Client that reads, grants and
// revokes privileges on any supported object kind.
type privilegeExecutor interface {
	HeldPrivileges(ctx context.Context, obj postgres.PrivilegeObject, grantee string) (privileges, grantable []string, found bool, err error)
	GrantPrivileges(ctx context.Context, obj postgres.PrivilegeObject, grantee string, privileges []string, withGrantOption bool) error
	RevokePrivileges(ctx context.Context, obj postgres.PrivilegeObject, grantee string, privileges []string, mode postgres.RevokeMode) error
}

var _ privilegeExecutor = (*postgres.Client)(nil)

// executorOps returns the engine ops for pg, with gone as the existence
// check for revokes (nil: everything exists).
func executorOps(pg privilegeExecutor, gone func(ctx context.Context, t grantTarget) (bool, error)) privilegeOps {
	return privilegeOps{
		held: func(ctx context.Context, t grantTarget) ([]string, []string, error) {
			privileges, grantable, _, err := pg.HeldPrivileges(ctx, t.object(), t.Grantee)
			return privileges, grantable, err
		},
		grant: func(ctx context.Context, t grantTarget, privileges []string, withGrantOption bool) error {
			return pg.GrantPrivileges(ctx, t.object(), t.Grantee, privileges, withGrantOption)
		},
		revoke: func(ctx context.Context, t grantTarget, privileges []string, mode postgres.RevokeMode) error {
			return pg.RevokePrivileges(ctx, t.object(), t.Grantee, privileges, mode)
		},
		gone: gone,
	}
}

// ledgerLimit is the most entries a ledger may hold; its status field has the
// same maxItems.
type ledgerLimit int

// applyPrivilegeGrants brings the privileges to desired and returns the
// ledger to record in status: the privileges and grant options pgop added
// and has not revoked. It applies as much as it can: an error on one target
// is reported (joined) and the others are still processed, and the returned
// ledger always reflects what was actually done, so nothing pgop added is
// forgotten. Revokes PostgreSQL refuses because of dependent privileges that
// pgop did not make possible are dropped from the ledger and reported with
// reason RevokeSkipped.
func applyPrivilegeGrants(ctx context.Context, desired, managed []privilegeGrant, ops privilegeOps, limit ledgerLimit) ([]privilegeGrant, error) {
	tracked := make(map[string]privilegeGrant, len(managed))
	for _, m := range managed {
		t := tracked[m.key()]
		tracked[m.key()] = privilegeGrant{
			Target:       m.Target,
			Privileges:   union(t.Privileges, m.Privileges),
			GrantOptions: union(t.GrantOptions, m.GrantOptions),
		}
	}
	want := make(map[string]privilegeGrant, len(desired))
	for _, d := range desired {
		want[d.key()] = d
	}
	if n := len(union(slices.Collect(maps.Keys(tracked)), slices.Collect(maps.Keys(want)))); limit > 0 && n > int(limit) {
		return sortedGrants(tracked), &conditionError{reason: ReasonTooManyGrants, err: fmt.Errorf(
			"the grants declared and those pgop still tracks add up to %d entries; pgop tracks at most %d. "+
				"Nothing was granted or revoked: remove grants from the spec", n, limit)}
	}

	var errs []error
	for _, d := range desired {
		added, err := grantMissing(ctx, d, ops)
		if !added.empty() {
			t := tracked[d.key()]
			tracked[d.key()] = privilegeGrant{
				Target:       d.Target,
				Privileges:   union(t.Privileges, added.Privileges),
				GrantOptions: union(t.GrantOptions, added.GrantOptions),
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}

	var skipped []string
	for _, k := range slices.Sorted(maps.Keys(tracked)) {
		m, err := revokeRemoved(ctx, tracked[k], want[k], ops, &skipped)
		if err != nil {
			errs = append(errs, err)
		}
		if m.empty() {
			delete(tracked, k)
		} else {
			tracked[k] = m
		}
	}
	if len(skipped) > 0 {
		errs = append(errs, &conditionError{reason: ReasonRevokeSkipped, err: fmt.Errorf(
			"not revoked (the grantee passed them on with a grant option pgop did not give; revoke with CASCADE "+
				"by hand if wanted), no longer tracked: %s", strings.Join(skipped, "; "))})
	}
	return sortedGrants(tracked), errors.Join(errs...)
}

// grantMissing grants what d declares and the grantee does not hold yet, and
// returns what it added.
func grantMissing(ctx context.Context, d privilegeGrant, ops privilegeOps) (privilegeGrant, error) {
	added := privilegeGrant{Target: d.Target}
	held, grantable, err := ops.held(ctx, d.Target)
	if err != nil {
		return added, err
	}
	missing := subtract(d.Privileges, held)
	missingGrantOptions := subtract(d.GrantOptions, grantable)
	if len(missingGrantOptions) > 0 {
		if err := ops.grant(ctx, d.Target, missingGrantOptions, true); err != nil {
			return added, err
		}
		added.GrantOptions = missingGrantOptions
		added.Privileges = intersect(missing, missingGrantOptions)
	}
	if plain := subtract(missing, missingGrantOptions); len(plain) > 0 {
		if err := ops.grant(ctx, d.Target, plain, false); err != nil {
			return added, err
		}
		added.Privileges = union(added.Privileges, plain)
	}
	return added, nil
}

// revokeRemoved revokes what the ledger entry m records and d (the desired
// grant of the same target, zero when none) no longer declares, and returns
// what remains recorded.
func revokeRemoved(ctx context.Context, m, d privilegeGrant, ops privilegeOps, skipped *[]string) (privilegeGrant, error) {
	removed := subtract(m.Privileges, d.Privileges)
	grantOptionsOnly := subtract(subtract(m.GrantOptions, d.GrantOptions), removed)
	if len(removed) == 0 && len(grantOptionsOnly) == 0 {
		return m, nil
	}
	if ops.gone != nil {
		gone, err := ops.gone(ctx, m.Target)
		if err != nil {
			return m, err
		}
		if gone {
			return privilegeGrant{Target: m.Target}, nil
		}
	}
	var errs []error
	if len(grantOptionsOnly) > 0 {
		// pgop added the grant option, so the grantee may have passed the
		// privileges on: cascade, or the REVOKE fails forever.
		if err := ops.revoke(ctx, m.Target, grantOptionsOnly, postgres.RevokeMode{GrantOptionOnly: true, Cascade: true}); err != nil {
			errs = append(errs, err)
		} else {
			m.GrantOptions = subtract(m.GrantOptions, grantOptionsOnly)
		}
	}
	// Privileges pgop also gave the grant option for are revoked with CASCADE.
	withOption := intersect(removed, m.GrantOptions)
	plain := subtract(removed, withOption)
	if len(withOption) > 0 {
		if err := ops.revoke(ctx, m.Target, withOption, postgres.RevokeMode{Cascade: true}); err != nil {
			errs = append(errs, err)
		} else {
			m.Privileges = subtract(m.Privileges, withOption)
			m.GrantOptions = subtract(m.GrantOptions, withOption)
		}
	}
	if len(plain) > 0 {
		err := ops.revoke(ctx, m.Target, plain, postgres.RevokeMode{})
		switch {
		case postgres.DependentPrivilegesExist(err):
			// The grantee got the grant option elsewhere and passed the
			// privileges on. pgop did not make that possible, so it does not
			// cascade into it.
			*skipped = append(*skipped, fmt.Sprintf("%s on %s", strings.Join(plain, ", "), m.Target))
			m.Privileges = subtract(m.Privileges, plain)
		case err != nil:
			errs = append(errs, err)
		default:
			m.Privileges = subtract(m.Privileges, plain)
		}
	}
	return m, errors.Join(errs...)
}

// sortedGrants returns the non-empty grants in m sorted by key, or nil.
func sortedGrants(m map[string]privilegeGrant) []privilegeGrant {
	var out []privilegeGrant
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !m[k].empty() {
			out = append(out, m[k])
		}
	}
	return out
}
