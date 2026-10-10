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

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// This file converts the grant ledgers between their status form and the
// engine's, and writes the statuses that hold them.

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

func extensionLedger(managed []postgresv1alpha1.ManagedExtensionGrant) []privilegeGrant {
	out := make([]privilegeGrant, 0, len(managed))
	for _, m := range managed {
		ek, ok := extensionKindOf(m.Kind)
		if !ok {
			continue
		}
		out = append(out, privilegeGrant{Target: extensionTarget(ek.engine, m.Extension, m.Schema, m.Role), Privileges: m.Privileges})
	}
	return out
}

func extensionLedgerStatus(ledger []privilegeGrant) []postgresv1alpha1.ManagedExtensionGrant {
	if len(ledger) == 0 {
		return nil
	}
	out := make([]postgresv1alpha1.ManagedExtensionGrant, 0, len(ledger))
	for _, g := range ledger {
		ek, ok := extensionKindOf(g.Target.Kind)
		if !ok {
			continue
		}
		out = append(out, postgresv1alpha1.ManagedExtensionGrant{Extension: g.Target.Name, Role: g.Target.Grantee,
			Kind: ek.api, Schema: g.Target.Schema, Privileges: g.Privileges})
	}
	return out
}

// saveStatus writes the Database's status. A conflict (a stale cached copy,
// or a concurrent change) is returned, not retried: the status computed from
// a stale copy must not overwrite a newer one (it could drop the record of
// what the Database created). The grant ledgers do not depend on a retry:
// what pgop is about to add is recorded before it is granted (see
// applyPrivilegeGrants), so a failed write only means nothing is granted
// until the next reconcile.
func (r *DatabaseReconciler) saveStatus(ctx context.Context, database *postgresv1alpha1.Database) error {
	return r.Status().Update(ctx, database)
}

// saveStatus writes the Role's status (see DatabaseReconciler.saveStatus).
func (r *RoleReconciler) saveStatus(ctx context.Context, role *postgresv1alpha1.Role) error {
	return r.Status().Update(ctx, role)
}
