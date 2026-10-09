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
	"fmt"
	"maps"
	"slices"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// databaseGrantClient is the subset of *postgres.Client used to reconcile
// database grants and settings; it is an interface so the logic can be tested
// without a server.
type databaseGrantClient interface {
	RoleExists(ctx context.Context, name string) (bool, error)
	GrantDatabasePrivileges(ctx context.Context, database, role string, privileges []string, withGrantOption bool) error
	RevokeDatabasePrivileges(ctx context.Context, database, role string, privileges []string, grantOptionOnly bool) error
	SetDatabaseParameter(ctx context.Context, database, parameter, value string) error
	ResetDatabaseParameter(ctx context.Context, database, parameter string) error
}

var _ databaseGrantClient = (*postgres.Client)(nil)

// desiredDatabaseGrants converts spec.grants to canonical privilege grants.
func desiredDatabaseGrants(grants []postgresv1alpha1.DatabaseGrantSpec) ([]privilegeGrant, error) {
	out := make([]privilegeGrant, 0, len(grants))
	seen := make(map[string]bool, len(grants))
	for _, g := range grants {
		if seen[g.Role] {
			return nil, fmt.Errorf("grants: role %q is listed more than once", g.Role)
		}
		seen[g.Role] = true
		privs, err := postgres.NormalizeDatabasePrivileges(g.Privileges)
		if err != nil {
			return nil, fmt.Errorf("grants[%s]: %w", g.Role, err)
		}
		out = append(out, privilegeGrant{Key: g.Role, Privileges: privs, WithGrantOption: g.WithGrantOption})
	}
	return out, nil
}

// reconcileDatabaseGrants brings the database-level privileges on pgName to
// the state declared by database.Spec.Grants and records the grants pgop
// manages in database.Status.ManagedGrants (the caller persists the status).
func reconcileDatabaseGrants(ctx context.Context, pg databaseGrantClient, database *postgresv1alpha1.Database, pgName string) error {
	desired, err := desiredDatabaseGrants(database.Spec.Grants)
	if err != nil {
		return err
	}
	managed := make([]privilegeGrant, 0, len(database.Status.ManagedGrants))
	for _, m := range database.Status.ManagedGrants {
		managed = append(managed, privilegeGrant{Key: m.Role, Privileges: m.Privileges, WithGrantOption: m.WithGrantOption})
	}
	if len(desired) == 0 && len(managed) == 0 {
		return nil
	}

	ops := privilegeOps{
		grant: func(ctx context.Context, g privilegeGrant) error {
			exists, err := pg.RoleExists(ctx, g.Key)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("grants[%s]: PostgreSQL role %q does not exist yet", g.Key, g.Key)
			}
			return pg.GrantDatabasePrivileges(ctx, pgName, g.Key, g.Privileges, g.WithGrantOption)
		},
		revoke: func(ctx context.Context, g privilegeGrant, grantOptionOnly bool) error {
			// A role that was dropped holds no privileges any more.
			exists, err := pg.RoleExists(ctx, g.Key)
			if err != nil || !exists {
				return err
			}
			return pg.RevokeDatabasePrivileges(ctx, pgName, g.Key, g.Privileges, grantOptionOnly)
		},
	}

	after, err := applyPrivilegeGrants(ctx, desired, managed, ops)
	database.Status.ManagedGrants = nil
	for _, g := range after {
		database.Status.ManagedGrants = append(database.Status.ManagedGrants, postgresv1alpha1.ManagedDatabaseGrant{
			Role: g.Key, Privileges: g.Privileges, WithGrantOption: g.WithGrantOption,
		})
	}
	return err
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

// reconcileDatabaseSettings applies spec.settings with ALTER DATABASE ... SET,
// resets the settings pgop applied that were removed from the spec, and
// records the settings pgop manages in database.Status.ManagedSettings.
func reconcileDatabaseSettings(ctx context.Context, pg databaseGrantClient, database *postgresv1alpha1.Database, pgName string) error {
	desired, err := desiredDatabaseSettings(database.Spec.Settings)
	if err != nil {
		return err
	}
	managed := database.Status.ManagedSettings
	if len(desired) == 0 && len(managed) == 0 {
		return nil
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
	return nil
}
