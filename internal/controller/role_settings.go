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

// roleSettingsClient is the subset of *postgres.Client used to reconcile role
// settings; it is an interface so the logic can be tested without a server.
type roleSettingsClient interface {
	parameterContextClient
	DatabaseExists(ctx context.Context, name string) (bool, error)
	SetRoleParameter(ctx context.Context, role, database, parameter, value string) error
	ResetRoleParameter(ctx context.Context, role, database, parameter string) error
}

var _ roleSettingsClient = (*postgres.Client)(nil)

// desiredRoleDatabaseSettings returns spec.databaseSettings as normalized
// settings per database.
func desiredRoleDatabaseSettings(entries []postgresv1alpha1.RoleDatabaseSettings) (map[string]map[string]string, error) {
	out := make(map[string]map[string]string, len(entries))
	for _, e := range entries {
		if e.Database == "" {
			return nil, errors.New("databaseSettings: database must not be empty")
		}
		if _, dup := out[e.Database]; dup {
			return nil, fmt.Errorf("databaseSettings: database %q is listed more than once", e.Database)
		}
		s, err := normalizeSettings(fmt.Sprintf("databaseSettings[%s].settings", e.Database), e.Settings)
		if err != nil {
			return nil, err
		}
		out[e.Database] = s
	}
	return out, nil
}

// clearStaleRoleSettings forgets the settings ledger when it was not written
// for pgName on the Cluster with UID clusterUID (the Cluster was re-created,
// or the status belongs to another role): pgop only resets settings it set
// on this very role.
func clearStaleRoleSettings(role *postgresv1alpha1.Role, pgName, clusterUID string) {
	if role.Status.RoleName == pgName && role.Status.ClusterUID == clusterUID {
		return
	}
	role.Status.ManagedSettings = nil
	role.Status.ManagedDatabaseSettings = nil
}

// reconcileRoleSettings applies spec.settings (ALTER ROLE ... SET) and
// spec.databaseSettings (ALTER ROLE ... IN DATABASE ... SET) to the
// PostgreSQL role pgName, resets the settings pgop applied that were removed
// from the spec (or are no longer allowed), and records what pgop manages in
// role.Status.ManagedSettings / ManagedDatabaseSettings (the caller persists
// the status). The caller must only call it for a role this Role owns.
//
// Settings that are not allowed (see settingAllowed: the same policy as
// Database spec.settings) are skipped and reported with reason
// SettingNotAllowed after the others are applied. databaseSettings for a
// database that does not exist are returned in pending instead of failing
// the reconcile: a Database owned by this Role waits for the Role to be
// Ready, so failing would deadlock.
func reconcileRoleSettings(ctx context.Context, pg roleSettingsClient, role *postgresv1alpha1.Role, pgName string) (pending []string, err error) {
	// Defense in depth: the CRD, checkRoleName and the ownership checks
	// already keep reserved roles (postgres, pgop_*, pg_*) out, whose
	// sessions include the operator's own.
	if reason := reservedRoleName(pgName); reason != "" {
		return nil, &conditionError{reason: ReasonReservedName, err: errors.New(reason)}
	}
	desired, err := normalizeSettings("settings", role.Spec.Settings)
	if err != nil {
		return nil, err
	}
	desiredDB, err := desiredRoleDatabaseSettings(role.Spec.DatabaseSettings)
	if err != nil {
		return nil, err
	}

	var errs []error
	var refused []string

	if len(desired) > 0 || len(role.Status.ManagedSettings) > 0 {
		after, r, err := applySettings(ctx, pg, desired, role.Status.ManagedSettings, roleSettingOps(pg, pgName, ""))
		role.Status.ManagedSettings = after
		errs = append(errs, err)
		for _, s := range r {
			refused = append(refused, "settings: "+s)
		}
	}

	managedDB := make(map[string][]string, len(role.Status.ManagedDatabaseSettings))
	for _, m := range role.Status.ManagedDatabaseSettings {
		managedDB[m.Database] = m.Settings
	}
	databases := slices.Sorted(maps.Keys(desiredDB))
	for db := range managedDB {
		if _, ok := desiredDB[db]; !ok {
			databases = append(databases, db)
		}
	}
	slices.Sort(databases)

	afterDB := make(map[string][]string, len(databases))
	for _, db := range databases {
		want, managed := desiredDB[db], managedDB[db]
		if len(want) == 0 && len(managed) == 0 {
			continue
		}
		exists, err := pg.DatabaseExists(ctx, db)
		if err != nil {
			errs = append(errs, err)
			afterDB[db] = managed
			continue
		}
		if !exists {
			// DROP DATABASE removes the role's settings in it, so there is
			// nothing left to reset; desired ones wait for the database.
			if len(want) > 0 {
				pending = append(pending, db)
			}
			continue
		}
		after, r, err := applySettings(ctx, pg, want, managed, roleSettingOps(pg, pgName, db))
		afterDB[db] = after
		errs = append(errs, err)
		for _, s := range r {
			refused = append(refused, fmt.Sprintf("databaseSettings[%s]: %s", db, s))
		}
	}
	role.Status.ManagedDatabaseSettings = nil
	for _, db := range slices.Sorted(maps.Keys(afterDB)) {
		if len(afterDB[db]) > 0 {
			role.Status.ManagedDatabaseSettings = append(role.Status.ManagedDatabaseSettings,
				postgresv1alpha1.ManagedRoleDatabaseSettings{Database: db, Settings: afterDB[db]})
		}
	}

	if len(refused) > 0 {
		errs = append(errs, &conditionError{reason: ReasonSettingNotAllowed,
			err: fmt.Errorf("settings not allowed: %s", strings.Join(refused, "; "))})
	}
	return pending, errors.Join(errs...)
}

func roleSettingOps(pg roleSettingsClient, role, database string) settingOps {
	return settingOps{
		set: func(ctx context.Context, name, value string) error {
			return pg.SetRoleParameter(ctx, role, database, name, value)
		},
		reset: func(ctx context.Context, name string) error {
			return pg.ResetRoleParameter(ctx, role, database, name)
		},
	}
}
