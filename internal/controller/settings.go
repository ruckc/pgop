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
	"strings"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// This file holds the logic shared by Database spec.settings (ALTER DATABASE
// ... SET) and Role spec.settings / spec.databaseSettings (ALTER ROLE ...
// [IN DATABASE ...] SET): name normalization, the policy deciding which
// parameters pgop sets on a writer's behalf, and the tracked set/reset loop.

// parameterContextClient looks up a parameter's pg_settings context.
type parameterContextClient interface {
	ParameterContext(ctx context.Context, name string) (pgContext string, found bool, err error)
}

// normalizeSettings returns settings keyed by normalized (lowercase)
// parameter name. field prefixes error messages (for example "settings").
func normalizeSettings(field string, settings map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(settings))
	for _, k := range slices.Sorted(maps.Keys(settings)) {
		name, err := postgres.NormalizeParameterName(k)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%s: parameter %q is listed more than once (names are case-insensitive)", field, name)
		}
		out[name] = settings[k]
	}
	return out, nil
}

// settingAllowed reports why pgop refuses to set parameter name (normalized)
// as a per-database or per-role default, or "" when it may. pgop runs ALTER
// DATABASE / ALTER ROLE as a superuser, and PostgreSQL trusts what a
// superuser stored, so the writer must not get more than a user could set:
//
//   - parameters on the static denylist are refused;
//   - a parameter the server knows must have context "user" (only
//     superusers may set superuser-context parameters, and postmaster,
//     sighup, internal and backend parameters cannot be set as defaults);
//   - a custom parameter the server does not know (a placeholder, name with
//     a dot) is only set when its namespace is in the Cluster's
//     rolePolicy.allowedSettingPrefixes and not in DeniedSettingPrefixes:
//     its context cannot be checked, and if an extension defining it as
//     superuser-only is loaded later the superuser-stored value applies.
//
// Unknown names without a dot are typos, which PostgreSQL rejects.
func settingAllowed(ctx context.Context, pg parameterContextClient, policy *postgresv1alpha1.RolePolicySpec,
	name string) (string, error) {
	if postgres.DeniedParameter(name) {
		return "is on pgop's denylist", nil
	}
	pgContext, found, err := pg.ParameterContext(ctx, name)
	if err != nil {
		return "", err
	}
	if found {
		if pgContext != pgContextUser {
			return fmt.Sprintf("has context %q (only %q parameters may be set)", pgContext, pgContextUser), nil
		}
		return "", nil
	}
	prefix, _, custom := strings.Cut(name, ".")
	if !custom {
		return "", nil
	}
	if slices.Contains(postgresv1alpha1.DeniedSettingPrefixes, prefix) {
		return fmt.Sprintf("is a custom parameter in the %q namespace, which pgop never sets "+
			"(its extension's settings run code, read server files or are superuser-only)", prefix), nil
	}
	if !policy.AllowsSettingPrefix(prefix) {
		return fmt.Sprintf("is a custom parameter the server does not know; its namespace %q is not listed "+
			"in the Cluster's spec.rolePolicy.allowedSettingPrefixes", prefix), nil
	}
	return "", nil
}

// settingOps applies one setting to its target (a database, a role, or a
// role in a database).
type settingOps struct {
	set   func(ctx context.Context, name, value string) error
	reset func(ctx context.Context, name string) error
}

// applySettings sets every allowed entry of desired (normalized names) and
// resets the managed names that are no longer desired or no longer allowed.
// It returns the names pgop manages afterwards (sorted; on an error, what was
// set so far plus the resets still pending) and the refused settings as
// "<name> <reason>". desired is not modified. policy is the Cluster's role
// policy (see settingAllowed).
func applySettings(ctx context.Context, pg parameterContextClient, policy *postgresv1alpha1.RolePolicySpec,
	desired map[string]string, managed []string, ops settingOps) (after []string, refused []string, err error) {
	allowed := make(map[string]string, len(desired))
	for _, name := range slices.Sorted(maps.Keys(desired)) {
		reason, err := settingAllowed(ctx, pg, policy, name)
		if err != nil {
			return managed, nil, err
		}
		if reason != "" {
			refused = append(refused, name+" "+reason)
			continue
		}
		allowed[name] = desired[name]
	}

	tracked := make(map[string]bool, len(managed)+len(allowed))
	for _, m := range managed {
		tracked[m] = true
	}

	// Settings are re-applied every time, which also repairs drift.
	for _, name := range slices.Sorted(maps.Keys(allowed)) {
		if err := ops.set(ctx, name, allowed[name]); err != nil {
			return sortedKeys(tracked), refused, err
		}
		tracked[name] = true
	}
	for _, name := range managed {
		if _, ok := allowed[name]; ok {
			continue
		}
		if err := ops.reset(ctx, name); err != nil {
			return sortedKeys(tracked), refused, err
		}
		delete(tracked, name)
	}
	return sortedKeys(tracked), refused, nil
}
