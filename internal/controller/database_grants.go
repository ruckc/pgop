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

// databaseGrantClient is the subset of *postgres.Client used to reconcile
// database grants, PUBLIC's database privileges and settings; it is an
// interface so the logic can be tested without a server.
type databaseGrantClient interface {
	privilegeExecutor
	publicPrivilegeClient
	RoleExists(ctx context.Context, name string) (bool, error)
	SetDatabaseParameter(ctx context.Context, database, parameter, value string) error
	ResetDatabaseParameter(ctx context.Context, database, parameter string) error
	ParameterContext(ctx context.Context, name string) (pgContext string, found bool, err error)
}

var _ databaseGrantClient = (*postgres.Client)(nil)

// databaseTarget is the ledger target of a database grant to grantee.
func databaseTarget(database, grantee string) grantTarget {
	return grantTarget{Kind: postgres.ObjectDatabase, Name: database, Grantee: postgres.CanonicalGrantee(grantee)}
}

// desiredDatabaseGrants converts spec.grants to canonical privilege grants on
// the database pgName.
func desiredDatabaseGrants(grants []postgresv1alpha1.DatabaseGrantSpec, pgName string) ([]privilegeGrant, error) {
	out := make([]privilegeGrant, 0, len(grants))
	seen := make(map[string]bool, len(grants))
	for _, g := range grants {
		t := databaseTarget(pgName, g.Role)
		if seen[t.Grantee] {
			return nil, fmt.Errorf("grants: role %q is listed more than once", t.Grantee)
		}
		seen[t.Grantee] = true
		if t.Grantee == postgres.PublicGrantee && g.WithGrantOption {
			return nil, errors.New("grants[PUBLIC]: the grant option cannot be granted to PUBLIC")
		}
		privs, err := postgres.NormalizeDatabasePrivileges(g.Privileges)
		if err != nil {
			return nil, fmt.Errorf("grants[%s]: %w", g.Role, err)
		}
		out = append(out, privilegeGrant{Target: t, Privileges: privs, WithGrantOption: g.WithGrantOption})
	}
	return out, nil
}

// roleGone returns the engine's gone check for grants whose only external
// dependency is the grantee: a role that was dropped holds no privileges.
func roleGone(pg interface {
	RoleExists(ctx context.Context, name string) (bool, error)
}) func(ctx context.Context, t grantTarget) (bool, error) {
	return func(ctx context.Context, t grantTarget) (bool, error) {
		if postgres.IsPublic(t.Grantee) {
			return false, nil
		}
		exists, err := pg.RoleExists(ctx, t.Grantee)
		return !exists, err
	}
}

// reconcileDatabaseGrants brings the database-level privileges on pgName to
// the state declared by database.Spec.Grants and records the grants pgop
// manages in database.Status.ManagedGrants (the caller persists the status).
// Grants to grantees the checker holds back (refused by the grantee policy,
// missing, or whose Role is being deleted) are not applied, revoked when
// pgop granted them, and reported after the others are applied.
func reconcileDatabaseGrants(ctx context.Context, pg databaseGrantClient, database *postgresv1alpha1.Database, pgName string,
	checker *granteeChecker) error {
	all, err := desiredDatabaseGrants(database.Spec.Grants, pgName)
	if err != nil {
		return err
	}
	managed := make([]privilegeGrant, 0, len(database.Status.ManagedGrants))
	for _, m := range database.Status.ManagedGrants {
		managed = append(managed, privilegeGrant{Target: databaseTarget(pgName, m.Role), Privileges: m.Privileges, WithGrantOption: m.WithGrantOption})
	}
	if len(all) == 0 && len(managed) == 0 {
		return nil
	}
	held := &grantFilter{checker: checker}
	desired, err := held.filter(ctx, "grants", all)
	if err != nil {
		return err
	}

	after, err := applyPrivilegeGrants(ctx, desired, managed, executorOps(pg, roleGone(pg)))
	database.Status.ManagedGrants = nil
	for _, g := range after {
		database.Status.ManagedGrants = append(database.Status.ManagedGrants, postgresv1alpha1.ManagedDatabaseGrant{
			Role: g.Target.Grantee, Privileges: g.Privileges, WithGrantOption: g.WithGrantOption,
		})
	}
	return errors.Join(err, held.err())
}

// schemaGrantClient is the subset of *postgres.Client used to reconcile
// schema grants (on a connection to the database).
type schemaGrantClient interface {
	privilegeExecutor
	RoleExists(ctx context.Context, name string) (bool, error)
	SchemaExists(ctx context.Context, name string) (bool, error)
}

var _ schemaGrantClient = (*postgres.Client)(nil)

// maxSchemaGrants bounds the schema grants pgop tracks per Database (the CRD
// allows 64 schemas with 16 grants each), which keeps
// status.managedSchemaGrants well below the object size limit.
const maxSchemaGrants = 1024

// schemaTarget is the ledger target of a schema grant to grantee.
func schemaTarget(schema, grantee string) grantTarget {
	return grantTarget{Kind: postgres.ObjectSchema, Name: schema, Grantee: postgres.CanonicalGrantee(grantee)}
}

// desiredSchemaGrants converts spec.schemas[].grants to canonical privilege
// grants, grouped by the schema field they come from (in spec order). System
// schemas are skipped (they are refused by reconcileSchemas).
func desiredSchemaGrants(schemas []postgresv1alpha1.SchemaSpec) (fields []string, grants map[string][]privilegeGrant, err error) {
	grants = map[string][]privilegeGrant{}
	seen := map[string]bool{}
	total := 0
	for _, s := range schemas {
		if systemSchemaName(s.Name) || len(s.Grants) == 0 {
			continue
		}
		field := fmt.Sprintf("schemas[%s].grants", s.Name)
		if _, dup := grants[field]; dup {
			return nil, nil, fmt.Errorf("schemas: schema %q is listed more than once", s.Name)
		}
		fields = append(fields, field)
		list := make([]privilegeGrant, 0, len(s.Grants))
		for _, g := range s.Grants {
			t := schemaTarget(s.Name, g.Role)
			if seen[t.key()] {
				return nil, nil, fmt.Errorf("%s: role %q is listed more than once", field, t.Grantee)
			}
			seen[t.key()] = true
			if t.Grantee == postgres.PublicGrantee && g.WithGrantOption {
				return nil, nil, fmt.Errorf("%s[PUBLIC]: the grant option cannot be granted to PUBLIC", field)
			}
			privs, err := postgres.NormalizeSchemaPrivileges(g.Privileges)
			if err != nil {
				return nil, nil, fmt.Errorf("%s[%s]: %w", field, g.Role, err)
			}
			list = append(list, privilegeGrant{Target: t, Privileges: privs, WithGrantOption: g.WithGrantOption})
		}
		grants[field] = list
		total += len(list)
	}
	if total > maxSchemaGrants {
		return nil, nil, &conditionError{reason: ReasonTooManyGrants, err: fmt.Errorf(
			"schemas declare %d grants; pgop tracks at most %d per Database", total, maxSchemaGrants)}
	}
	return fields, grants, nil
}

// reconcileSchemaGrants brings the schema privileges to the state declared
// by spec.schemas[].grants and records the grants pgop manages in
// database.Status.ManagedSchemaGrants (the caller persists the status). It
// runs on a connection to the database, after the schemas were created.
// Privileges pgop granted are revoked once they leave the spec, also when the
// whole schema entry is removed (the schema is not dropped); a schema or
// grantee that no longer exists holds nothing to revoke.
func reconcileSchemaGrants(ctx context.Context, pg schemaGrantClient, database *postgresv1alpha1.Database, checker *granteeChecker) error {
	fields, all, err := desiredSchemaGrants(database.Spec.Schemas)
	if err != nil {
		return err
	}
	managed := make([]privilegeGrant, 0, len(database.Status.ManagedSchemaGrants))
	for _, m := range database.Status.ManagedSchemaGrants {
		managed = append(managed, privilegeGrant{Target: schemaTarget(m.Schema, m.Role), Privileges: m.Privileges, WithGrantOption: m.WithGrantOption})
	}
	if len(fields) == 0 && len(managed) == 0 {
		return nil
	}
	held := &grantFilter{checker: checker}
	var desired []privilegeGrant
	for _, field := range fields {
		allowed, err := held.filter(ctx, field, all[field])
		if err != nil {
			return err
		}
		desired = append(desired, allowed...)
	}

	roleIsGone := roleGone(pg)
	gone := func(ctx context.Context, t grantTarget) (bool, error) {
		exists, err := pg.SchemaExists(ctx, t.Name)
		if err != nil || !exists {
			return !exists, err
		}
		return roleIsGone(ctx, t)
	}
	after, err := applyPrivilegeGrants(ctx, desired, managed, executorOps(pg, gone))
	database.Status.ManagedSchemaGrants = nil
	for _, g := range after {
		database.Status.ManagedSchemaGrants = append(database.Status.ManagedSchemaGrants, postgresv1alpha1.ManagedSchemaGrant{
			Schema: g.Target.Name, Role: g.Target.Grantee, Privileges: g.Privileges, WithGrantOption: g.WithGrantOption,
		})
	}
	return errors.Join(err, held.err())
}

// publicPrivilegeClient reads the privileges PUBLIC holds on an object.
type publicPrivilegeClient interface {
	PublicPrivileges(ctx context.Context, obj postgres.PrivilegeObject) (privileges []string, found bool, err error)
}

// publicSchemaName is the schema spec.publicPrivileges.publicSchema* act on.
const publicSchemaName = "public"

// publicPrivilegeItem is one default PUBLIC privilege spec.publicPrivileges
// can revoke.
type publicPrivilegeItem struct {
	field     postgresv1alpha1.PublicPrivilege
	kind      postgres.ObjectKind
	privilege string
	requested func(*postgresv1alpha1.PublicPrivilegesSpec) *bool
}

var publicPrivilegeItems = []publicPrivilegeItem{
	{postgresv1alpha1.PublicPrivilegeConnect, postgres.ObjectDatabase, postgres.PrivilegeConnect,
		func(p *postgresv1alpha1.PublicPrivilegesSpec) *bool { return p.Connect }},
	{postgresv1alpha1.PublicPrivilegeTemporary, postgres.ObjectDatabase, postgres.PrivilegeTemporary,
		func(p *postgresv1alpha1.PublicPrivilegesSpec) *bool { return p.Temporary }},
	{postgresv1alpha1.PublicPrivilegePublicSchemaUsage, postgres.ObjectSchema, postgres.PrivilegeUsage,
		func(p *postgresv1alpha1.PublicPrivilegesSpec) *bool { return p.PublicSchemaUsage }},
	{postgresv1alpha1.PublicPrivilegePublicSchemaCreate, postgres.ObjectSchema, postgres.PrivilegeCreate,
		func(p *postgresv1alpha1.PublicPrivilegesSpec) *bool { return p.PublicSchemaCreate }},
}

// revokeRequested reports whether spec.publicPrivileges sets the item's
// field to false.
func (it publicPrivilegeItem) revokeRequested(spec *postgresv1alpha1.PublicPrivilegesSpec) bool {
	if spec == nil {
		return false
	}
	v := it.requested(spec)
	return v != nil && !*v
}

// publicGrantConflict reports whether the Database also grants the item's
// privilege to PUBLIC (in spec.grants, or in schemas[public].grants).
func publicGrantConflict(database *postgresv1alpha1.Database, it publicPrivilegeItem) bool {
	grantsIt := func(privileges []string, normalize func([]string) ([]string, error)) bool {
		privs, err := normalize(privileges)
		return err == nil && slices.Contains(privs, it.privilege)
	}
	if it.kind == postgres.ObjectDatabase {
		return slices.ContainsFunc(database.Spec.Grants, func(g postgresv1alpha1.DatabaseGrantSpec) bool {
			return postgres.IsPublic(g.Role) && grantsIt(g.Privileges, postgres.NormalizeDatabasePrivileges)
		})
	}
	for _, s := range database.Spec.Schemas {
		if s.Name == publicSchemaName && slices.ContainsFunc(s.Grants, func(g postgresv1alpha1.GrantSpec) bool {
			return postgres.IsPublic(g.Role) && grantsIt(g.Privileges, postgres.NormalizeSchemaPrivileges)
		}) {
			return true
		}
	}
	return false
}

// publicPrivilegeExecutor is what reconcilePublicPrivileges needs.
type publicPrivilegeExecutor interface {
	privilegeExecutor
	publicPrivilegeClient
}

// reconcilePublicPrivileges applies spec.publicPrivileges for the items of
// kind (the database items run on the admin connection, the schema items on
// a connection to the database) and records what pgop revoked in
// database.Status.RevokedPublicPrivileges.
//
// A privilege is revoked from PUBLIC, and recorded, only while PUBLIC holds
// it, so the record lists exactly what pgop took away; once the spec no
// longer asks for the revoke, pgop grants exactly that back to PUBLIC. The
// check runs on every reconcile, so a privilege granted back to PUBLIC
// outside pgop is revoked again. A privilege the spec also grants to PUBLIC
// is left alone and reported (reason PublicPrivilegeConflict).
func reconcilePublicPrivileges(ctx context.Context, pg publicPrivilegeExecutor, database *postgresv1alpha1.Database,
	pgName string, kind postgres.ObjectKind) error {
	recorded := map[postgresv1alpha1.PublicPrivilege]bool{}
	for _, p := range database.Status.RevokedPublicPrivileges {
		recorded[p] = true
	}
	record := func() {
		database.Status.RevokedPublicPrivileges = nil
		for _, it := range publicPrivilegeItems {
			if recorded[it.field] {
				database.Status.RevokedPublicPrivileges = append(database.Status.RevokedPublicPrivileges, it.field)
			}
		}
	}
	defer record()

	type held struct {
		privileges []string
		found      bool
	}
	cache := map[postgres.PrivilegeObject]held{}
	publicHolds := func(obj postgres.PrivilegeObject) (held, error) {
		if h, ok := cache[obj]; ok {
			return h, nil
		}
		privs, found, err := pg.PublicPrivileges(ctx, obj)
		if err != nil {
			return held{}, err
		}
		cache[obj] = held{privs, found}
		return cache[obj], nil
	}

	var conflicts []string
	for _, it := range publicPrivilegeItems {
		if it.kind != kind {
			continue
		}
		obj := postgres.PrivilegeObject{Kind: it.kind, Name: pgName}
		if it.kind == postgres.ObjectSchema {
			obj.Name = publicSchemaName
		}
		wantRevoked := it.revokeRequested(database.Spec.PublicPrivileges)
		if wantRevoked && publicGrantConflict(database, it) {
			conflicts = append(conflicts, string(it.field))
			continue
		}
		if !wantRevoked && !recorded[it.field] {
			continue
		}
		h, err := publicHolds(obj)
		if err != nil {
			return err
		}
		if !h.found {
			// The object is gone (the schema public was dropped): there is
			// nothing to revoke, and nothing to grant back.
			if !wantRevoked {
				delete(recorded, it.field)
			}
			continue
		}
		holds := slices.Contains(h.privileges, it.privilege)
		switch {
		case wantRevoked && holds:
			if err := pg.RevokePrivileges(ctx, obj, postgres.PublicGrantee, []string{it.privilege}, postgres.RevokeMode{}); err != nil {
				return err
			}
			recorded[it.field] = true
		case !wantRevoked:
			if !holds {
				if err := pg.GrantPrivileges(ctx, obj, postgres.PublicGrantee, []string{it.privilege}, false); err != nil {
					return err
				}
			}
			delete(recorded, it.field)
		}
	}
	if len(conflicts) > 0 {
		return &conditionError{reason: ReasonPublicPrivilegeConflict, err: fmt.Errorf(
			"publicPrivileges %s: the same privilege is granted to PUBLIC in grants or schemas[public].grants; "+
				"PUBLIC's privilege is left as it is", strings.Join(conflicts, ", "))}
	}
	return nil
}

// desiredDatabaseSettings returns spec.settings keyed by normalized
// (lowercase) parameter name.
func desiredDatabaseSettings(settings map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(settings))
	for _, k := range slices.Sorted(maps.Keys(settings)) {
		name, err := postgres.NormalizeParameterName(k)
		if err != nil {
			return nil, fmt.Errorf("settings: %w", err)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("settings: parameter %q is listed more than once (names are case-insensitive)", name)
		}
		out[name] = settings[k]
	}
	return out, nil
}

// settingAllowed reports why pgop refuses to set parameter name per database,
// or "" when it may. Parameters on the static denylist are refused, and so is
// every parameter the server knows with a context other than "user" (only
// superusers may set superuser-context parameters, and postmaster, sighup,
// internal and backend parameters cannot be set per database at all): pgop
// runs ALTER DATABASE as a superuser, so without this check a Database author
// could set superuser-only parameters for every session. Unknown parameters
// are custom placeholders (or typos, which PostgreSQL then rejects).
func settingAllowed(ctx context.Context, pg databaseGrantClient, name string) (string, error) {
	if postgres.DeniedParameter(name) {
		return "is on pgop's denylist", nil
	}
	pgContext, found, err := pg.ParameterContext(ctx, name)
	if err != nil {
		return "", err
	}
	if found && pgContext != pgContextUser {
		return fmt.Sprintf("has context %q (only %q parameters may be set per database)", pgContext, pgContextUser), nil
	}
	return "", nil
}

// reconcileDatabaseSettings applies spec.settings with ALTER DATABASE ... SET,
// resets the settings pgop applied that were removed from the spec (or are no
// longer allowed), and records the settings pgop manages in
// database.Status.ManagedSettings. Settings that are not allowed are skipped
// and reported with reason SettingNotAllowed after the others are applied.
func reconcileDatabaseSettings(ctx context.Context, pg databaseGrantClient, database *postgresv1alpha1.Database, pgName string) error {
	desired, err := desiredDatabaseSettings(database.Spec.Settings)
	if err != nil {
		return err
	}
	managed := database.Status.ManagedSettings
	if len(desired) == 0 && len(managed) == 0 {
		return nil
	}

	var refused []string
	for _, name := range slices.Sorted(maps.Keys(desired)) {
		reason, err := settingAllowed(ctx, pg, name)
		if err != nil {
			return err
		}
		if reason != "" {
			refused = append(refused, name+" "+reason)
			delete(desired, name)
		}
	}

	tracked := make(map[string]bool, len(managed)+len(desired))
	for _, m := range managed {
		tracked[m] = true
	}
	record := func() { database.Status.ManagedSettings = sortedKeys(tracked) }

	// Settings are re-applied every time, which also repairs drift.
	for _, name := range slices.Sorted(maps.Keys(desired)) {
		if err := pg.SetDatabaseParameter(ctx, pgName, name, desired[name]); err != nil {
			record()
			return err
		}
		tracked[name] = true
	}
	for _, name := range managed {
		if _, ok := desired[name]; ok {
			continue
		}
		if err := pg.ResetDatabaseParameter(ctx, pgName, name); err != nil {
			record()
			return err
		}
		delete(tracked, name)
	}
	record()
	if len(refused) > 0 {
		return &conditionError{reason: ReasonSettingNotAllowed,
			err: fmt.Errorf("settings not allowed: %s", strings.Join(refused, "; "))}
	}
	return nil
}
