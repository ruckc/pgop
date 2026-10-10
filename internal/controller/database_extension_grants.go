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
	"slices"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Extension grants (spec.extensions[].grants).
//
// An extension's objects are owned by the superuser that installed it, so
// their owner cannot grant on them for the application: pgop does, through
// the shared grant engine. A grant names the extension, a grantee and
// privileges per kind of object; pgop lists the extension's member objects
// (pg_depend deptype 'e') on every reconcile, so objects an update adds are
// picked up. The ledger (status.managedExtensionGrants) records per
// extension, grantee and kind the privileges pgop added on at least one
// object; removing them revokes them from every object of that kind of the
// extension.
//
// Privileges are only granted on objects that do not act with their owner's
// (superuser) privileges and that do not depend on not being executable by
// others:
//
//   - EXECUTE only on functions and procedures written in SQL or PL/pgSQL
//     that are not SECURITY DEFINER: a SECURITY DEFINER function owned by a
//     superuser runs as that superuser, and C functions (dblink_connect_u,
//     file readers, pageinspect, ...) rely on REVOKE EXECUTE FROM PUBLIC as
//     their only access check. An invoker SQL or PL/pgSQL function runs with
//     the caller's privileges, so it gives nothing the caller could not do
//     itself. When a function pgop granted EXECUTE on stops qualifying (an
//     update made it SECURITY DEFINER or rewrote it in C), pgop revokes that
//     EXECUTE;
//   - table privileges only on plain and partitioned tables: views and
//     materialized views read their tables with their owner's privileges,
//     and a foreign table may read server files (file_fdw);
//   - no TRIGGER privilege (see ExtensionGrantSpec.Tables).
//
// Skipped objects are counted in status.extensions[].skippedObjects.
//
// On an extension the server does not trust at its installed version (only
// the Cluster's allowedExtensions allows it), only read-only privileges are
// granted (extensionKind.readOnly): CREATE on its schema or write privileges
// on its tables and sequences would let a role plant objects where, or
// change the state that, the extension's superuser-run code reads, which is
// what the stricter install rule for such extensions forbids.

// Ledger limit: the maxItems of status.managedExtensionGrants.
const extensionGrantLedgerLimit ledgerLimit = 1024

// allPrivilegesKeyword is the long form of ALL.
const allPrivilegesKeyword = "ALL PRIVILEGES"

// The engine object kinds of extension grants.
const (
	extKindSchema    postgres.ObjectKind = "EXTENSION SCHEMA"
	extKindTables    postgres.ObjectKind = "EXTENSION TABLES"
	extKindSequences postgres.ObjectKind = "EXTENSION SEQUENCES"
	extKindFunctions postgres.ObjectKind = "EXTENSION FUNCTIONS"
)

// extensionKind ties an API object kind to its engine kind, member kind and
// privileges.
type extensionKind struct {
	api    postgresv1alpha1.ExtensionObjectKind
	engine postgres.ObjectKind
	member postgres.MemberKind // "" for the schema
	// privileges are the privileges accepted (besides ALL); all is what ALL
	// expands to.
	privileges []string
	all        []string
	// readOnly are the privileges granted on an extension the server does
	// not trust (one only the Cluster's allowedExtensions allows): none of
	// them lets the grantee write where the extension's superuser-run code
	// (background workers, SECURITY DEFINER functions, a DBA calling its
	// functions) resolves names or reads its state.
	readOnly []string
	spec     func(postgresv1alpha1.ExtensionGrantSpec) []string
}

var extensionKinds = []extensionKind{
	{api: postgresv1alpha1.ExtensionObjectSchema, engine: extKindSchema,
		privileges: []string{postgres.PrivilegeCreate, postgres.PrivilegeUsage},
		all:        []string{postgres.PrivilegeCreate, postgres.PrivilegeUsage},
		readOnly:   []string{postgres.PrivilegeUsage},
		spec:       func(g postgresv1alpha1.ExtensionGrantSpec) []string { return g.Schema }},
	{api: postgresv1alpha1.ExtensionObjectTables, engine: extKindTables, member: postgres.MemberTables,
		privileges: []string{postgres.PrivilegeSelect, postgres.PrivilegeInsert, postgres.PrivilegeUpdate, postgres.PrivilegeDelete,
			postgres.PrivilegeTruncate, postgres.PrivilegeReferences, postgres.PrivilegeMaintain},
		all: []string{postgres.PrivilegeSelect, postgres.PrivilegeInsert, postgres.PrivilegeUpdate, postgres.PrivilegeDelete,
			postgres.PrivilegeTruncate, postgres.PrivilegeReferences},
		readOnly: []string{postgres.PrivilegeSelect},
		spec:     func(g postgresv1alpha1.ExtensionGrantSpec) []string { return g.Tables }},
	{api: postgresv1alpha1.ExtensionObjectSequences, engine: extKindSequences, member: postgres.MemberSequences,
		privileges: []string{postgres.PrivilegeUsage, postgres.PrivilegeSelect, postgres.PrivilegeUpdate},
		all:        []string{postgres.PrivilegeUsage, postgres.PrivilegeSelect, postgres.PrivilegeUpdate},
		readOnly:   []string{postgres.PrivilegeSelect},
		spec:       func(g postgresv1alpha1.ExtensionGrantSpec) []string { return g.Sequences }},
	{api: postgresv1alpha1.ExtensionObjectFunctions, engine: extKindFunctions, member: postgres.MemberRoutines,
		privileges: []string{postgres.PrivilegeExecute},
		all:        []string{postgres.PrivilegeExecute},
		readOnly:   []string{postgres.PrivilegeExecute},
		spec:       func(g postgresv1alpha1.ExtensionGrantSpec) []string { return g.Functions }},
}

// extensionKindOf returns the extension kind with the engine or API kind k.
func extensionKindOf[K postgres.ObjectKind | postgresv1alpha1.ExtensionObjectKind](k K) (extensionKind, bool) {
	for _, ek := range extensionKinds {
		if string(ek.engine) == string(k) || string(ek.api) == string(k) {
			return ek, true
		}
	}
	return extensionKind{}, false
}

// normalize validates privileges for the kind and returns them upper case,
// with ALL expanded, sorted and without duplicates.
func (ek extensionKind) normalize(privileges []string) ([]string, error) {
	if len(privileges) == 0 {
		return nil, fmt.Errorf("no %s privileges given", ek.api)
	}
	var out []string
	for _, p := range privileges {
		u := strings.ToUpper(strings.Join(strings.Fields(p), " "))
		switch {
		case u == postgres.PrivilegeAll || u == allPrivilegesKeyword:
			out = append(out, ek.all...)
		case slices.Contains(ek.privileges, u):
			out = append(out, u)
		default:
			return nil, fmt.Errorf("invalid %s privilege %q (allowed: %s, ALL)", ek.api, p, strings.Join(ek.privileges, ", "))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// extensionTarget is the ledger target of an extension grant.
func extensionTarget(kind postgres.ObjectKind, extension, schema, grantee string) grantTarget {
	return grantTarget{Kind: kind, Name: extension, Schema: schema, Grantee: postgres.CanonicalGrantee(grantee)}
}

// desiredExtensionGrants converts spec.extensions[].grants to privilege
// grants, grouped by spec field (in spec order). Only extensions in
// states.eligible (installed and allowed) are included. Schema grants on a
// schema that is not the extension's own (public, a system schema, one
// listed in spec.schemas), and privileges other than the kind's readOnly ones
// on an extension the server does not trust (states.untrusted), are left
// out and reported in notAllowed; MAINTAIN on a server older than
// PostgreSQL 17 in unsupported.
func desiredExtensionGrants(database *postgresv1alpha1.Database, states extensionStates, serverVersion int) (
	fields []string, grants map[string][]privilegeGrant, notAllowed, unsupported []string, err error) {
	specSchemas := map[string]bool{}
	for _, s := range database.Spec.Schemas {
		specSchemas[s.Name] = true
	}
	grants = map[string][]privilegeGrant{}
	for _, ext := range database.Spec.Extensions {
		inst := states.eligible[ext.Name]
		if inst == nil || len(ext.Grants) == 0 {
			continue
		}
		field := fmt.Sprintf("extensions[%s].grants", ext.Name)
		if _, dup := grants[field]; dup {
			continue // reported by reconcileExtensions
		}
		fields = append(fields, field)
		seen := map[string]bool{}
		var list []privilegeGrant
		for _, g := range ext.Grants {
			grantee := postgres.CanonicalGrantee(g.Role)
			if seen[grantee] {
				return nil, nil, nil, nil, fmt.Errorf("%s: role %q is listed more than once", field, grantee)
			}
			seen[grantee] = true
			for _, ek := range extensionKinds {
				requested := ek.spec(g)
				if len(requested) == 0 {
					continue
				}
				privs, err := ek.normalize(requested)
				if err != nil {
					return nil, nil, nil, nil, fmt.Errorf("%s[%s]: %w", field, g.Role, err)
				}
				schema := ""
				if ek.engine == extKindSchema {
					schema = inst.Schema
					if schema == publicSchemaName || systemSchemaName(schema) || specSchemas[schema] {
						notAllowed = append(notAllowed, fmt.Sprintf("%s[%s].schema: the extension is installed in %s, "+
							"not in a schema of its own (grant on it in spec.schemas[].grants)", field, g.Role, schema))
						continue
					}
				}
				if states.untrusted[ext.Name] {
					if denied := subtract(privs, ek.readOnly); len(denied) > 0 {
						notAllowed = append(notAllowed, fmt.Sprintf("%s[%s].%s: %s, because the server does not trust the "+
							"extension (only the Cluster's allowedExtensions allows it): only %s", field, g.Role, ek.api,
							strings.Join(denied, ", "), strings.Join(ek.readOnly, ", ")))
						if privs = intersect(privs, ek.readOnly); len(privs) == 0 {
							continue
						}
					}
				}
				if slices.Contains(privs, postgres.PrivilegeMaintain) && serverVersion < postgres.MinMaintainPrivilegeVersion {
					unsupported = append(unsupported, fmt.Sprintf("%s[%s].tables: MAINTAIN", field, g.Role))
					privs = slices.DeleteFunc(privs, func(p string) bool { return p == postgres.PrivilegeMaintain })
					if len(privs) == 0 {
						continue
					}
				}
				list = append(list, desiredGrant(extensionTarget(ek.engine, ext.Name, schema, grantee), privs, false))
			}
		}
		grants[field] = list
	}
	return fields, grants, notAllowed, unsupported, nil
}

// extensionGrantClient is the subset of *postgres.Client used to reconcile
// extension grants (on a connection to the database).
type extensionGrantClient interface {
	privilegeExecutor
	RoleExists(ctx context.Context, name string) (bool, error)
	SchemaExists(ctx context.Context, name string) (bool, error)
	InstalledExtension(ctx context.Context, name string) (*postgres.InstalledExtension, error)
	ExtensionMembers(ctx context.Context, extension string, kind postgres.MemberKind, grantee string) ([]postgres.ExtensionMember, error)
	GrantOnMembers(ctx context.Context, kind postgres.MemberKind, identities, privileges []string, grantee string) error
	RevokeOnMembers(ctx context.Context, kind postgres.MemberKind, identities, privileges []string, grantee string) error
}

var _ extensionGrantClient = (*postgres.Client)(nil)

// extensionGrantOps returns the engine ops for extension grants. cover maps
// "schema|grantee" to the schema privileges desired grants of any extension
// declare for the grantee: a revoke on an extension's schema leaves those
// alone (two extensions can share a schema).
func extensionGrantOps(pg extensionGrantClient, cover map[string][]string) privilegeOps {
	schemaObj := func(t grantTarget) postgres.PrivilegeObject {
		return postgres.PrivilegeObject{Kind: postgres.ObjectSchema, Name: t.Schema}
	}
	members := func(ctx context.Context, t grantTarget) (extensionKind, []postgres.ExtensionMember, error) {
		ek, ok := extensionKindOf(t.Kind)
		if !ok {
			return ek, nil, fmt.Errorf("unsupported extension grant kind %q", t.Kind)
		}
		m, err := pg.ExtensionMembers(ctx, t.Name, ek.member, t.Grantee)
		return ek, m, err
	}
	roleIsGone := roleGone(pg)
	return privilegeOps{
		held: func(ctx context.Context, t grantTarget) ([]string, []string, error) {
			if t.Kind == extKindSchema {
				privs, grantable, _, err := pg.HeldPrivileges(ctx, schemaObj(t), t.Grantee)
				return privs, grantable, err
			}
			ek, ms, err := members(ctx, t)
			if err != nil {
				return nil, nil, err
			}
			// What the grantee holds on every eligible object; with none,
			// nothing is missing.
			held := slices.Clone(ek.privileges)
			for _, m := range ms {
				if m.Eligible {
					held = intersect(held, m.Held)
				}
			}
			return held, nil, nil
		},
		grant: func(ctx context.Context, t grantTarget, privileges []string, _ bool) error {
			if t.Kind == extKindSchema {
				return pg.GrantPrivileges(ctx, schemaObj(t), t.Grantee, privileges, false)
			}
			ek, ms, err := members(ctx, t)
			if err != nil {
				return err
			}
			var errs []error
			for _, p := range privileges {
				var ids []string
				for _, m := range ms {
					if m.Eligible && !slices.Contains(m.Held, p) {
						ids = append(ids, m.Identity)
					}
				}
				if len(ids) > 0 {
					errs = append(errs, pg.GrantOnMembers(ctx, ek.member, ids, []string{p}, t.Grantee))
				}
			}
			return errors.Join(errs...)
		},
		revoke: func(ctx context.Context, t grantTarget, privileges []string, _ postgres.RevokeMode) error {
			if t.Kind == extKindSchema {
				privileges = subtract(privileges, cover[t.Schema+"|"+t.Grantee])
				if len(privileges) == 0 {
					return nil
				}
				return pg.RevokePrivileges(ctx, schemaObj(t), t.Grantee, privileges, postgres.RevokeMode{})
			}
			ek, ms, err := members(ctx, t)
			if err != nil {
				return err
			}
			var errs []error
			for _, p := range privileges {
				var ids []string
				for _, m := range ms {
					if slices.Contains(m.Held, p) {
						ids = append(ids, m.Identity)
					}
				}
				if len(ids) > 0 {
					errs = append(errs, pg.RevokeOnMembers(ctx, ek.member, ids, []string{p}, t.Grantee))
				}
			}
			return errors.Join(errs...)
		},
		gone: func(ctx context.Context, t grantTarget) (bool, error) {
			if gone, err := roleIsGone(ctx, t); err != nil || gone {
				return gone, err
			}
			if t.Kind == extKindSchema {
				if t.Schema == "" {
					return true, nil
				}
				exists, err := pg.SchemaExists(ctx, t.Schema)
				return !exists, err
			}
			inst, err := pg.InstalledExtension(ctx, t.Name)
			return inst == nil, err
		},
	}
}

// reconcileExtensionGrants brings the privileges on the extensions' objects
// to the state declared by spec.extensions[].grants and records what pgop
// added in database.Status.ManagedExtensionGrants (the caller persists the
// status). states.eligible are the extensions that are installed and allowed
// (see reconcileExtensions); grants on other extensions are not applied, and
// those pgop made are revoked, except on extensions whose state could not be
// read (states.unknown), whose tracked grants are left as they are. It also
// revokes EXECUTE pgop granted on functions that no longer qualify, and
// counts the skipped objects in status.extensions[].skippedObjects.
func reconcileExtensionGrants(ctx context.Context, pg extensionGrantClient, database *postgresv1alpha1.Database,
	states extensionStates, serverVersion int, checker *granteeChecker, save statusSaver) error {
	if states.allUnknown {
		return nil
	}
	fields, all, notAllowed, unsupported, err := desiredExtensionGrants(database, states, serverVersion)
	if err != nil {
		return err
	}
	var kept []privilegeGrant
	managed := slices.DeleteFunc(extensionLedger(database.Status.ManagedExtensionGrants), func(g privilegeGrant) bool {
		if states.unknown[g.Target.Name] {
			kept = append(kept, g)
			return true
		}
		return false
	})
	if len(fields) == 0 && len(managed) == 0 {
		return nil
	}
	withKept := func(ledger []privilegeGrant) []postgresv1alpha1.ManagedExtensionGrant {
		m := map[string]privilegeGrant{}
		for _, g := range slices.Concat(ledger, kept) {
			m[g.key()] = g
		}
		return extensionLedgerStatus(sortedGrants(m))
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
	cover := map[string][]string{}
	for _, d := range desired {
		if d.Target.Kind == extKindSchema {
			k := d.Target.Schema + "|" + d.Target.Grantee
			cover[k] = union(cover[k], d.Privileges)
		}
	}
	persist := func(ctx context.Context, ledger []privilegeGrant) error {
		database.Status.ManagedExtensionGrants = withKept(ledger)
		if save == nil {
			return nil
		}
		return save(ctx)
	}
	after, err := applyPrivilegeGrants(ctx, desired, managed, extensionGrantOps(pg, cover),
		max(extensionGrantLedgerLimit-ledgerLimit(len(kept)), 1), persist)
	database.Status.ManagedExtensionGrants = withKept(after)
	errs := []error{err, held.err(), revokeFromIneligibleObjects(ctx, pg, database, desired, after)}
	if len(notAllowed) > 0 {
		errs = append(errs, &conditionError{reason: ReasonExtensionGrantNotAllowed, err: fmt.Errorf(
			"extension grants not applied (and revoked if pgop granted them): %s", strings.Join(notAllowed, "; "))})
	}
	if len(unsupported) > 0 {
		errs = append(errs, &conditionError{reason: ReasonUnsupportedServerVersion, err: fmt.Errorf(
			"the MAINTAIN privilege needs PostgreSQL 17 or later: %s", strings.Join(unsupported, "; "))})
	}
	return errors.Join(errs...)
}

// revokeFromIneligibleObjects revokes the privileges pgop's ledger records
// from objects of the desired member kinds that no longer qualify (a
// function an update made SECURITY DEFINER or rewrote in C), and records per
// extension how many objects the grants skip.
func revokeFromIneligibleObjects(ctx context.Context, pg extensionGrantClient, database *postgresv1alpha1.Database,
	desired, ledger []privilegeGrant) error {
	recorded := map[string][]string{}
	for _, g := range ledger {
		recorded[g.key()] = g.Privileges
	}
	skipped := map[string]map[string]bool{} // extension -> skipped object identities
	var errs []error
	for _, d := range desired {
		ek, ok := extensionKindOf(d.Target.Kind)
		if !ok || ek.member == "" {
			continue
		}
		ms, err := pg.ExtensionMembers(ctx, d.Target.Name, ek.member, d.Target.Grantee)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, p := range recorded[d.key()] {
			var ids []string
			for _, m := range ms {
				if !m.Eligible && slices.Contains(m.Held, p) {
					ids = append(ids, m.Identity)
				}
			}
			if len(ids) > 0 {
				errs = append(errs, pg.RevokeOnMembers(ctx, ek.member, ids, []string{p}, d.Target.Grantee))
			}
		}
		for _, m := range ms {
			if !m.Eligible {
				if skipped[d.Target.Name] == nil {
					skipped[d.Target.Name] = map[string]bool{}
				}
				skipped[d.Target.Name][string(ek.member)+" "+m.Identity] = true
			}
		}
	}
	for i := range database.Status.Extensions {
		st := &database.Status.Extensions[i]
		st.SkippedObjects = int32(len(skipped[st.Name]))
	}
	return errors.Join(errs...)
}

// reconcileExtensionAccess runs reconcileExtensionGrants on a connection to
// the database.
func reconcileExtensionAccess(ctx context.Context, pg *postgres.Client, database *postgresv1alpha1.Database,
	states extensionStates, checker *granteeChecker, save statusSaver) error {
	if len(database.Status.ManagedExtensionGrants) == 0 && !slices.ContainsFunc(database.Spec.Extensions,
		func(e postgresv1alpha1.ExtensionSpec) bool { return len(e.Grants) > 0 }) {
		for i := range database.Status.Extensions {
			database.Status.Extensions[i].SkippedObjects = 0
		}
		return nil
	}
	version, err := pg.ServerVersionNum(ctx)
	if err != nil {
		return err
	}
	err = reconcileExtensionGrants(ctx, pg, database, states, version, checker, save)
	if err != nil {
		logf.FromContext(ctx).Error(err, "Failed to reconcile extension grants")
	}
	return err
}

// extensionsGrantingTo reports whether spec.extensions[].grants name the
// PostgreSQL role pgName.
func extensionsGrantingTo(database *postgresv1alpha1.Database, pgName string) bool {
	return slices.ContainsFunc(database.Spec.Extensions, func(e postgresv1alpha1.ExtensionSpec) bool {
		return slices.ContainsFunc(e.Grants, func(g postgresv1alpha1.ExtensionGrantSpec) bool { return g.Role == pgName })
	})
}
