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

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// Privileges held by a role block DROP ROLE ("role cannot be dropped because
// some objects depend on it"). The helpers below remove the privileges that
// pgop itself manages on a role's behalf (database and schema ACL entries)
// right before the role is dropped. They revoke with CASCADE: the role is
// going away, so privileges it passed on to others go with it, as they would
// with DROP OWNED.

// listStrings runs query with arg and returns the first column of each row.
func (c *Client) listStrings(ctx context.Context, query string, arg any) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeAllDatabasePrivileges revokes every database-level privilege role
// holds, on every database of the cluster.
func (c *Client) RevokeAllDatabasePrivileges(ctx context.Context, role string) error {
	dbs, err := c.listStrings(ctx, `SELECT DISTINCT d.datname FROM pg_database d, aclexplode(d.datacl) a
WHERE a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $1)`, role)
	if err != nil {
		return fmt.Errorf("failed to list database privileges of %q: %w", role, err)
	}
	for _, db := range dbs {
		query := buildRevokeQuery([]string{PrivilegeAll}, "DATABASE "+quoteIdent(db), role, RevokeMode{Cascade: true})
		if _, err := c.db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("failed to revoke privileges on database %q from %q: %w", db, role, err)
		}
	}
	return nil
}

// DatabasesWithSchemaPrivileges returns the connectable databases in which
// role holds privileges on schemas (recorded in pg_shdepend).
func (c *Client) DatabasesWithSchemaPrivileges(ctx context.Context, role string) ([]string, error) {
	dbs, err := c.listStrings(ctx, `SELECT DISTINCT d.datname FROM pg_shdepend s
JOIN pg_database d ON d.oid = s.dbid
WHERE s.deptype = 'a' AND s.classid = 'pg_namespace'::regclass
  AND s.refclassid = 'pg_authid'::regclass
  AND s.refobjid = (SELECT oid FROM pg_roles WHERE rolname = $1)
  AND d.datallowconn`, role)
	if err != nil {
		return nil, fmt.Errorf("failed to list schema privileges of %q: %w", role, err)
	}
	return dbs, nil
}

// RevokeAllSchemaPrivileges revokes every schema privilege role holds in the
// database this client is connected to.
func (c *Client) RevokeAllSchemaPrivileges(ctx context.Context, role string) error {
	schemas, err := c.listStrings(ctx, `SELECT DISTINCT n.nspname FROM pg_namespace n, aclexplode(n.nspacl) a
WHERE a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $1)`, role)
	if err != nil {
		return fmt.Errorf("failed to list schema privileges of %q: %w", role, err)
	}
	for _, s := range schemas {
		query := buildRevokeQuery([]string{PrivilegeAll}, "SCHEMA "+quoteIdent(s), role, RevokeMode{Cascade: true})
		if _, err := c.db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("failed to revoke privileges on schema %q from %q: %w", s, role, err)
		}
	}
	return nil
}

// DependentObjectsError reports that DROP ROLE failed because objects or
// privileges still depend on the role. Detail is PostgreSQL's list of them.
type DependentObjectsError struct {
	Role   string
	Detail string
	Err    error
}

func (e *DependentObjectsError) Error() string {
	return fmt.Sprintf("role %q cannot be dropped because objects or privileges depend on it: %s", e.Role, e.Detail)
}

func (e *DependentObjectsError) Unwrap() error { return e.Err }

// sqlStateDependentObjects is SQLSTATE dependent_objects_still_exist.
const sqlStateDependentObjects = "2BP01"

// maxDetailLength bounds the PostgreSQL detail kept in a DependentObjectsError
// (it ends up in a status condition).
const maxDetailLength = 1024

// dependentObjectsError converts a DROP ROLE failure with SQLSTATE 2BP01
// (dependent_objects_still_exist) to a *DependentObjectsError.
func dependentObjectsError(role string, err error) error {
	pqErr, ok := errors.AsType[*pq.Error](err)
	if !ok || pqErr.Code != sqlStateDependentObjects {
		return nil
	}
	detail := pqErr.Detail
	if len(detail) > maxDetailLength {
		detail = detail[:maxDetailLength] + "..."
	}
	return &DependentObjectsError{Role: role, Detail: detail, Err: err}
}
