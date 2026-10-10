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
	"maps"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// This file converts the grant ledgers between their status form and the
// engine's, and writes statuses without ever dropping a ledger entry.

// statusSaver durably writes the resource's current status, ledgers
// included. The grant reconcilers call it with the intended additions before
// they grant (see applyPrivilegeGrants).
type statusSaver func(ctx context.Context) error

// grantOptionsOf returns the grant options a status ledger entry records.
// Entries written by pgop v0.15 carry withGrantOption instead (all recorded
// privileges were granted with the grant option); they are read the same way.
func grantOptionsOf(privileges, grantOptions []string, legacyWithGrantOption bool) []string {
	if legacyWithGrantOption {
		return union(grantOptions, privileges)
	}
	return grantOptions
}

func databaseLedger(pgName string, managed []postgresv1alpha1.ManagedDatabaseGrant) []privilegeGrant {
	out := make([]privilegeGrant, 0, len(managed))
	for _, m := range managed {
		out = append(out, privilegeGrant{Target: databaseTarget(pgName, m.Role), Privileges: m.Privileges,
			GrantOptions: grantOptionsOf(m.Privileges, m.GrantOptions, m.WithGrantOption)})
	}
	return out
}

func databaseLedgerStatus(ledger []privilegeGrant) []postgresv1alpha1.ManagedDatabaseGrant {
	if len(ledger) == 0 {
		return nil
	}
	out := make([]postgresv1alpha1.ManagedDatabaseGrant, 0, len(ledger))
	for _, g := range ledger {
		out = append(out, postgresv1alpha1.ManagedDatabaseGrant{Role: g.Target.Grantee, Privileges: g.Privileges, GrantOptions: g.GrantOptions})
	}
	return out
}

func schemaLedger(managed []postgresv1alpha1.ManagedSchemaGrant) []privilegeGrant {
	out := make([]privilegeGrant, 0, len(managed))
	for _, m := range managed {
		out = append(out, privilegeGrant{Target: schemaTarget(m.Schema, m.Role), Privileges: m.Privileges,
			GrantOptions: grantOptionsOf(m.Privileges, m.GrantOptions, m.WithGrantOption)})
	}
	return out
}

func schemaLedgerStatus(ledger []privilegeGrant) []postgresv1alpha1.ManagedSchemaGrant {
	if len(ledger) == 0 {
		return nil
	}
	out := make([]postgresv1alpha1.ManagedSchemaGrant, 0, len(ledger))
	for _, g := range ledger {
		out = append(out, postgresv1alpha1.ManagedSchemaGrant{Schema: g.Target.Name, Role: g.Target.Grantee,
			Privileges: g.Privileges, GrantOptions: g.GrantOptions})
	}
	return out
}

func parameterLedger(member string, managed []postgresv1alpha1.ManagedParameterGrant) []privilegeGrant {
	out := make([]privilegeGrant, 0, len(managed))
	for _, m := range managed {
		out = append(out, privilegeGrant{Target: parameterTarget(m.Parameter, member), Privileges: m.Privileges,
			GrantOptions: grantOptionsOf(m.Privileges, m.GrantOptions, m.WithGrantOption)})
	}
	return out
}

func parameterLedgerStatus(ledger []privilegeGrant) []postgresv1alpha1.ManagedParameterGrant {
	if len(ledger) == 0 {
		return nil
	}
	out := make([]postgresv1alpha1.ManagedParameterGrant, 0, len(ledger))
	for _, g := range ledger {
		out = append(out, postgresv1alpha1.ManagedParameterGrant{Parameter: g.Target.Name, Privileges: g.Privileges, GrantOptions: g.GrantOptions})
	}
	return out
}

// mergeLedgers returns the union of two ledgers, entry by entry.
func mergeLedgers(a, b []privilegeGrant) []privilegeGrant {
	m := make(map[string]privilegeGrant, len(a)+len(b))
	for _, g := range append(a, b...) {
		m[g.key()] = mergeGrant(m[g.key()], g)
	}
	return sortedGrants(m)
}

// mergeDatabaseLedgers adds the ledger entries of theirs (a newer copy of the
// status) to ours, so a status write never drops what another write recorded.
// The settings ledger is merged too (a RESET of a setting that is not set
// changes nothing).
func mergeDatabaseLedgers(ours, theirs *postgresv1alpha1.DatabaseStatus) {
	ours.ManagedGrants = databaseLedgerStatus(mergeLedgers(databaseLedger("", ours.ManagedGrants), databaseLedger("", theirs.ManagedGrants)))
	ours.ManagedSchemaGrants = schemaLedgerStatus(mergeLedgers(schemaLedger(ours.ManagedSchemaGrants), schemaLedger(theirs.ManagedSchemaGrants)))
	ours.ManagedSettings = unionStrings(ours.ManagedSettings, theirs.ManagedSettings)
	revoked := map[postgresv1alpha1.PublicPrivilege]bool{}
	for _, p := range append(ours.RevokedPublicPrivileges, theirs.RevokedPublicPrivileges...) {
		revoked[p] = true
	}
	ours.RevokedPublicPrivileges = nil
	for _, it := range publicPrivilegeItems {
		if revoked[it.field] {
			ours.RevokedPublicPrivileges = append(ours.RevokedPublicPrivileges, it.field)
		}
	}
}

// mergeRoleLedgers is mergeDatabaseLedgers for a Role (parameter grants,
// memberships and settings).
func mergeRoleLedgers(ours, theirs *postgresv1alpha1.RoleStatus) {
	ours.ManagedParameterGrants = parameterLedgerStatus(mergeLedgers(parameterLedger("", ours.ManagedParameterGrants),
		parameterLedger("", theirs.ManagedParameterGrants)))
	ours.ManagedMemberships = unionStrings(ours.ManagedMemberships, theirs.ManagedMemberships)
	ours.ManagedSettings = unionStrings(ours.ManagedSettings, theirs.ManagedSettings)
	perDB := map[string][]string{}
	for _, d := range append(slices.Clone(ours.ManagedDatabaseSettings), theirs.ManagedDatabaseSettings...) {
		perDB[d.Database] = unionStrings(perDB[d.Database], d.Settings)
	}
	ours.ManagedDatabaseSettings = nil
	for _, db := range slices.Sorted(maps.Keys(perDB)) {
		ours.ManagedDatabaseSettings = append(ours.ManagedDatabaseSettings,
			postgresv1alpha1.ManagedRoleDatabaseSettings{Database: db, Settings: perDB[db]})
	}
}

// unionStrings returns the sorted union of two string sets, nil when empty.
func unionStrings(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range append(slices.Clone(a), b...) {
		set[s] = true
	}
	return sortedKeys(set)
}

// updateStatusKeepingLedgers writes obj's status. On a conflict (the cached
// object was stale, or the object changed meanwhile) it re-reads the object,
// merges the newer copy's grant ledgers into obj's (merge) and retries, so an
// entry pgop recorded is never lost to a status write.
func updateStatusKeepingLedgers[T client.Object](ctx context.Context, c client.Client, reader client.Reader, obj T,
	fresh func() T, merge func(ours, theirs T)) error {
	if reader == nil {
		reader = c
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := c.Status().Update(ctx, obj)
		if !apierrors.IsConflict(err) {
			return err
		}
		latest := fresh()
		if getErr := reader.Get(ctx, client.ObjectKeyFromObject(obj), latest); getErr != nil {
			return getErr
		}
		merge(obj, latest)
		obj.SetResourceVersion(latest.GetResourceVersion())
		return err
	})
}

// saveStatus writes the Database's status without dropping ledger entries.
func (r *DatabaseReconciler) saveStatus(ctx context.Context, database *postgresv1alpha1.Database) error {
	return updateStatusKeepingLedgers(ctx, r.Client, r.APIReader, database,
		func() *postgresv1alpha1.Database { return &postgresv1alpha1.Database{} },
		func(ours, theirs *postgresv1alpha1.Database) { mergeDatabaseLedgers(&ours.Status, &theirs.Status) })
}

// saveStatus writes the Role's status without dropping ledger entries.
func (r *RoleReconciler) saveStatus(ctx context.Context, role *postgresv1alpha1.Role) error {
	return updateStatusKeepingLedgers(ctx, r.Client, r.APIReader, role,
		func() *postgresv1alpha1.Role { return &postgresv1alpha1.Role{} },
		func(ours, theirs *postgresv1alpha1.Role) { mergeRoleLedgers(&ours.Status, &theirs.Status) })
}
