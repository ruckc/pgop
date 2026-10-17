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
	"database/sql"
	"errors"
	"fmt"
)

// Role deletion with Role spec.deletion.reassignOwnedTo: the objects a role
// owns are handed to another role before the role is dropped.

// ReassignTargetInfo describes the role objects are to be reassigned to, and
// whether the role being deleted is a member of it.
type ReassignTargetInfo struct {
	Exists bool
	Super  bool
	// OldHasUsage reports pg_has_role(old, target, 'USAGE'): the old role
	// inherits the target's privileges.
	OldHasUsage bool
}

// RoleReassignTarget reads what the reassignment guards need to know about
// target and the role old being deleted.
func (c *Client) RoleReassignTarget(ctx context.Context, old, target string) (ReassignTargetInfo, error) {
	var info ReassignTargetInfo
	err := c.db.QueryRowContext(ctx, `SELECT r.rolsuper, pg_catalog.pg_has_role($1::name, r.oid, 'USAGE')
FROM pg_catalog.pg_roles r WHERE r.rolname = $2`, old, target).Scan(&info.Super, &info.OldHasUsage)
	if errors.Is(err, sql.ErrNoRows) {
		return ReassignTargetInfo{}, nil
	}
	if err != nil {
		return ReassignTargetInfo{}, fmt.Errorf("failed to inspect role %q: %w", target, err)
	}
	info.Exists = true
	return info, nil
}

// DatabasesWithOwnedObjects returns the databases in which role owns objects
// (pg_shdepend, deptype 'o'), with whether each accepts connections.
func (c *Client) DatabasesWithOwnedObjects(ctx context.Context, role string) ([]DatabaseRef, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT d.datname, d.datallowconn FROM pg_catalog.pg_shdepend s
JOIN pg_catalog.pg_database d ON d.oid = s.dbid
WHERE s.deptype = 'o'
  AND s.refclassid = 'pg_catalog.pg_authid'::pg_catalog.regclass
  AND s.refobjid = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1)
ORDER BY d.datname`, role)
	if err != nil {
		return nil, fmt.Errorf("failed to list databases with objects owned by %q: %w", role, err)
	}
	defer func() { _ = rows.Close() }()
	var out []DatabaseRef
	for rows.Next() {
		var d DatabaseRef
		if err := rows.Scan(&d.Name, &d.AllowConns); err != nil {
			return nil, fmt.Errorf("failed to scan database: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DatabasesOwnedBy returns the names of the databases role owns.
func (c *Client) DatabasesOwnedBy(ctx context.Context, role string) ([]string, error) {
	out, err := c.listStrings(ctx, `SELECT d.datname FROM pg_catalog.pg_database d
JOIN pg_catalog.pg_roles r ON r.oid = d.datdba WHERE r.rolname = $1 ORDER BY d.datname`, role)
	if err != nil {
		return nil, fmt.Errorf("failed to list databases owned by %q: %w", role, err)
	}
	return out, nil
}

// buildReassignOwnedQueries returns the statements that hand the objects of
// old to target. DROP OWNED runs after the reassignment, so it only removes
// the privileges and default privileges left over.
func buildReassignOwnedQueries(old, target string) []string {
	return []string{
		"REASSIGN OWNED BY " + quoteIdent(old) + " TO " + quoteIdent(target),
		"DROP OWNED BY " + quoteIdent(old),
	}
}

// ReassignOwned runs REASSIGN OWNED BY old TO target and DROP OWNED BY old in
// the database this client is connected to, in one transaction.
func (c *Client) ReassignOwned(ctx context.Context, old, target string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	for _, q := range buildReassignOwnedQueries(old, target) {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("failed to reassign objects owned by %q to %q: %w", old, target, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit reassignment: %w", err)
	}
	return nil
}
