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

// settingAllowed reports why pgop refuses to set parameter name as a
// per-database or per-role default, or "" when it may. Parameters on the
// static denylist are refused, and so is every parameter the server knows
// with a context other than "user" (only superusers may set superuser-context
// parameters, and postmaster, sighup, internal and backend parameters cannot
// be set as defaults at all): pgop runs ALTER DATABASE / ALTER ROLE as a
// superuser, so without this check a writer could set superuser-only
// parameters for sessions. Unknown parameters are custom placeholders (or
// typos, which PostgreSQL then rejects).
func settingAllowed(ctx context.Context, pg parameterContextClient, name string) (string, error) {
	if postgres.DeniedParameter(name) {
		return "is on pgop's denylist", nil
	}
	pgContext, found, err := pg.ParameterContext(ctx, name)
	if err != nil {
		return "", err
	}
	if found && pgContext != pgContextUser {
		return fmt.Sprintf("has context %q (only %q parameters may be set)", pgContext, pgContextUser), nil
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
// "<name> <reason>". desired is not modified.
func applySettings(ctx context.Context, pg parameterContextClient, desired map[string]string, managed []string,
	ops settingOps) (after []string, refused []string, err error) {
	allowed := make(map[string]string, len(desired))
	for _, name := range slices.Sorted(maps.Keys(desired)) {
		reason, err := settingAllowed(ctx, pg, name)
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
