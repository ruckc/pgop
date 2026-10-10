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

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// parameterGrantClient is the subset of *postgres.Client used to reconcile
// parameter grants; it is an interface so the logic can be tested without a
// server.
type parameterGrantClient interface {
	ServerVersionNum(ctx context.Context) (int, error)
	privilegeExecutor
	RoleExists(ctx context.Context, name string) (bool, error)
	ParameterContext(ctx context.Context, name string) (pgContext string, found bool, err error)
}

// grantableParameterContexts are the pg_settings contexts of parameters pgop
// grants SET on. SET on a superuser-context parameter is what parameterGrants
// is for; SET on a user-context one is harmless. Other contexts are refused:
// sighup, postmaster and internal parameters cannot be SET in a session at
// all (the grant would only be a trap for a later ALTER SYSTEM privilege),
// and backend / superuser-backend parameters (log_connections, ...) would be
// settable by the role at connection start.
var grantableParameterContexts = []string{pgContextUser, pgContextSuperuser}

// pg_settings contexts pgop checks for.
const (
	pgContextUser      = "user"
	pgContextSuperuser = "superuser"
)

// filterParameterContexts drops from desired the parameters whose context
// pgop does not grant SET on, and returns their names. Custom placeholders
// unknown to the server are kept, as for Database.spec.settings.
func filterParameterContexts(ctx context.Context, pg parameterGrantClient, desired []privilegeGrant) (allowed []privilegeGrant, refused []string, err error) {
	allowed = desired[:0:0]
	for _, g := range desired {
		pgContext, found, err := pg.ParameterContext(ctx, g.Target.Name)
		if err != nil {
			return nil, nil, err
		}
		if found && !slices.Contains(grantableParameterContexts, pgContext) {
			refused = append(refused, fmt.Sprintf("%s (context %q)", g.Target.Name, pgContext))
			continue
		}
		allowed = append(allowed, g)
	}
	return allowed, refused, nil
}

var _ parameterGrantClient = (*postgres.Client)(nil)

// parameterTarget is the ledger target of a parameter grant to member.
func parameterTarget(parameter, member string) grantTarget {
	return grantTarget{Kind: postgres.ObjectParameter, Name: parameter, Grantee: member}
}

// desiredParameterGrants converts spec.parameterGrants to canonical privilege
// grants to member, one per lowercased parameter name. Parameters on pgop's
// denylist (see postgres.DeniedParameter) are left out and returned in
// denied.
func desiredParameterGrants(grants []postgresv1alpha1.ParameterGrantSpec, member string) (out []privilegeGrant, denied []string, err error) {
	out = make([]privilegeGrant, 0, len(grants))
	seen := make(map[string]bool, len(grants))
	for _, g := range grants {
		name, err := postgres.NormalizeParameterName(g.Parameter)
		if err != nil {
			return nil, nil, fmt.Errorf("parameterGrants: %w", err)
		}
		if seen[name] {
			return nil, nil, fmt.Errorf("parameterGrants: parameter %q is listed more than once (names are case-insensitive)", name)
		}
		seen[name] = true
		if postgres.DeniedParameter(name) {
			denied = append(denied, name)
			continue
		}
		privs, err := postgres.NormalizeParameterPrivileges(g.Privileges)
		if err != nil {
			return nil, nil, fmt.Errorf("parameterGrants[%s]: %w", name, err)
		}
		out = append(out, privilegeGrant{Target: parameterTarget(name, member), Privileges: privs, WithGrantOption: g.WithGrantOption})
	}
	return out, denied, nil
}

func managedParameterGrants(role *postgresv1alpha1.Role, member string) []privilegeGrant {
	out := make([]privilegeGrant, 0, len(role.Status.ManagedParameterGrants))
	for _, m := range role.Status.ManagedParameterGrants {
		out = append(out, privilegeGrant{Target: parameterTarget(m.Parameter, member), Privileges: m.Privileges, WithGrantOption: m.WithGrantOption})
	}
	return out
}

func recordParameterGrants(role *postgresv1alpha1.Role, grants []privilegeGrant) {
	role.Status.ManagedParameterGrants = nil
	for _, g := range grants {
		role.Status.ManagedParameterGrants = append(role.Status.ManagedParameterGrants, postgresv1alpha1.ManagedParameterGrant{
			Parameter: g.Target.Name, Privileges: g.Privileges, WithGrantOption: g.WithGrantOption,
		})
	}
}

// reconcileParameterGrants brings member's privileges on configuration
// parameters to the state declared by role.Spec.ParameterGrants and records
// the grants pgop manages in role.Status.ManagedParameterGrants (the caller
// persists the status). Parameter privileges need PostgreSQL 15; on an older
// server a non-empty parameterGrants is reported with reason
// UnsupportedServerVersion. Denylisted parameters are not granted (a managed
// grant on one is revoked) and are reported with reason ParameterNotAllowed.
func reconcileParameterGrants(ctx context.Context, pg parameterGrantClient, role *postgresv1alpha1.Role, member string) error {
	desired, denied, err := desiredParameterGrants(role.Spec.ParameterGrants, member)
	if err != nil {
		return err
	}
	var deniedErr error
	if len(denied) > 0 {
		deniedErr = &conditionError{reason: ReasonParameterNotAllowed, err: fmt.Errorf(
			"parameterGrants not allowed (they switch identity, load code or bypass safeguards): %s",
			strings.Join(denied, ", "))}
	}
	managed := managedParameterGrants(role, member)
	if len(desired) == 0 && len(managed) == 0 {
		return deniedErr
	}

	version, err := pg.ServerVersionNum(ctx)
	if err != nil {
		return err
	}
	if version < postgres.MinParameterPrivilegesVersion {
		if len(desired) > 0 {
			return &conditionError{reason: ReasonUnsupportedServerVersion, err: fmt.Errorf(
				"parameterGrants require PostgreSQL 15 or later (server_version_num %d); remove them or upgrade the cluster", version)}
		}
		// No parameter privileges can exist on this server.
		role.Status.ManagedParameterGrants = nil
		return deniedErr
	}

	desired, refused, err := filterParameterContexts(ctx, pg, desired)
	if err != nil {
		return err
	}
	if len(refused) > 0 {
		deniedErr = errors.Join(deniedErr, &conditionError{reason: ReasonParameterNotAllowed, err: fmt.Errorf(
			"parameterGrants not allowed (only user and superuser parameters can be granted): %s",
			strings.Join(refused, ", "))})
	}

	after, err := applyPrivilegeGrants(ctx, desired, managed, executorOps(pg, nil))
	recordParameterGrants(role, after)
	if err != nil {
		return err
	}
	return deniedErr
}

// revokeManagedParameterGrants revokes every parameter privilege pgop granted
// to member, before the role is dropped: DROP ROLE fails while the role holds
// privileges on parameters.
func revokeManagedParameterGrants(ctx context.Context, pg parameterGrantClient, role *postgresv1alpha1.Role, member string) error {
	managed := managedParameterGrants(role, member)
	if len(managed) == 0 {
		return nil
	}
	exists, err := pg.RoleExists(ctx, member)
	if err != nil || !exists {
		return err
	}
	after, err := applyPrivilegeGrants(ctx, nil, managed, executorOps(pg, nil))
	recordParameterGrants(role, after)
	return err
}
