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
	"slices"
	"strings"

	"github.com/lib/pq"
)

// Privileges on objects in a schema (tables, sequences, functions,
// procedures, types) and default privileges.
//
// Object names given by users are never spliced into SQL: they are passed as
// bind parameters to catalog queries, and statements only ever name objects
// with the server's own rendering of their OID (oid::regclass,
// oid::regprocedure, oid::regtype), read in the same session right before
// the statement. The operator's search_path is pg_catalog, so that rendering
// is always schema-qualified, and quoted where needed.

// PrivilegeTrigger is the TRIGGER table privilege.
const PrivilegeTrigger = "TRIGGER"

// SchemaObjectKind is a kind of object in a schema that privileges are
// granted on. Its value is the keyword of GRANT ... ON <kind>.
type SchemaObjectKind string

// Schema object kinds.
const (
	SchemaTable     SchemaObjectKind = "TABLE"
	SchemaSequence  SchemaObjectKind = "SEQUENCE"
	SchemaFunction  SchemaObjectKind = "FUNCTION"
	SchemaProcedure SchemaObjectKind = "PROCEDURE"
	SchemaType      SchemaObjectKind = "TYPE"
)

// SchemaObjectPrivileges are the privileges accepted per schema object kind
// (ALL is expanded by the caller and not accepted here).
var SchemaObjectPrivileges = map[SchemaObjectKind][]string{
	SchemaTable: {PrivilegeSelect, PrivilegeInsert, PrivilegeUpdate, PrivilegeDelete, PrivilegeTruncate,
		PrivilegeReferences, PrivilegeTrigger, PrivilegeMaintain},
	SchemaSequence:  {PrivilegeUsage, PrivilegeSelect, PrivilegeUpdate},
	SchemaFunction:  {PrivilegeExecute},
	SchemaProcedure: {PrivilegeExecute},
	SchemaType:      {PrivilegeUsage},
}

// ACLItem is one privilege an object's ACL gives a grantee, granted by the
// object's owner.
type ACLItem struct {
	// Grantee is the grantee's OID, 0 for PUBLIC.
	Grantee   int64
	Privilege string
	Grantable bool
}

// SchemaObject is an object of a schema, as listed by ListSchemaObjects.
type SchemaObject struct {
	OID int64
	// Identity is the object as the server renders it (oid::regclass,
	// oid::regprocedure or oid::regtype: schema-qualified, quoted, with the
	// argument types of a routine).
	Identity string
	// Name is the object's name in its schema (relname, proname, typname).
	Name string
	// Owner is the owner's name, OwnerOID its OID; OwnerSuperuser reports
	// whether it is a superuser and OwnerIsSessionUser whether it is the role
	// the client is connected as (the operator).
	Owner              string
	OwnerOID           int64
	OwnerSuperuser     bool
	OwnerIsSessionUser bool
	// Extension is the extension the object belongs to (pg_depend deptype
	// 'e'), "" when none.
	Extension string
	// SecurityDefiner and Language describe a routine.
	SecurityDefiner bool
	Language        string
	// SubKind is pg_class.relkind for tables, pg_proc.prokind for routines
	// and pg_type.typtype for types.
	SubKind string
	// ACL lists the privileges granted by the owner (the owner's own
	// included, PostgreSQL's defaults when the ACL is NULL).
	ACL []ACLItem
}

// Held returns the privileges the role with OID grantee (0 for PUBLIC)
// holds directly on the object from its owner, and those it may grant
// (the owner may grant all of its own privileges), both sorted.
func (o SchemaObject) Held(grantee int64) (privileges, grantable []string) {
	for _, a := range o.ACL {
		if a.Grantee != grantee {
			continue
		}
		privileges = append(privileges, a.Privilege)
		if a.Grantable || a.Grantee == o.OwnerOID {
			grantable = append(grantable, a.Privilege)
		}
	}
	slices.Sort(privileges)
	slices.Sort(grantable)
	return slices.Compact(privileges), slices.Compact(grantable)
}

// aclColumns renders the owner-granted ACL of an object as three arrays
// sorted the same way (grantee, privilege, grantable).
func aclColumns(acl, defaultKind, owner string) string {
	return `(SELECT pg_catalog.array_agg(a.grantee::int8 ORDER BY a.grantee, a.privilege_type)
  FROM pg_catalog.aclexplode(COALESCE(` + acl + `, pg_catalog.acldefault('` + defaultKind + `', ` + owner + `))) a WHERE a.grantor = ` + owner + `),
(SELECT pg_catalog.array_agg(a.privilege_type::text ORDER BY a.grantee, a.privilege_type)
  FROM pg_catalog.aclexplode(COALESCE(` + acl + `, pg_catalog.acldefault('` + defaultKind + `', ` + owner + `))) a WHERE a.grantor = ` + owner + `),
(SELECT pg_catalog.array_agg(a.is_grantable ORDER BY a.grantee, a.privilege_type)
  FROM pg_catalog.aclexplode(COALESCE(` + acl + `, pg_catalog.acldefault('` + defaultKind + `', ` + owner + `))) a WHERE a.grantor = ` + owner + `)`
}

// extensionOf renders the extension an object of catalog class belongs to.
func extensionOf(class, oid string) string {
	return `COALESCE((SELECT e.extname::text FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_extension e ON e.oid = d.refobjid
  WHERE d.classid = '` + class + `'::pg_catalog.regclass AND d.objid = ` + oid + `
    AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'e' LIMIT 1), '')`
}

// ownerColumns renders the owner's name, OID, superuser flag and whether it
// is the session user, from the pg_roles row o.
const ownerColumns = `o.rolname::text, o.oid::int8, o.rolsuper, o.rolname = session_user`

// schemaObjectQueries list the objects of a kind in schema $1: every one
// when $2 is true, otherwise those named in $3 (and, for routines, those
// whose OID is in $5). At most $4 rows are returned.
var schemaObjectQueries = map[SchemaObjectKind]string{
	SchemaTable:     relationQuery(`'r', 'p', 'v', 'm', 'f'`, "r"),
	SchemaSequence:  relationQuery(`'S'`, "s"),
	SchemaFunction:  routineQuery(`'f', 'a', 'w'`),
	SchemaProcedure: routineQuery(`'p'`),
	SchemaType: `SELECT t.oid::int8, t.oid::pg_catalog.regtype::text, t.typname::text, ` + ownerColumns + `,
  ` + extensionOf("pg_catalog.pg_type", "t.oid") + `, false, '', t.typtype::text,
  ` + aclColumns("t.typacl", "T", "t.typowner") + `
FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
JOIN pg_catalog.pg_roles o ON o.oid = t.typowner
WHERE n.nspname = $1 AND t.typtype IN ('b', 'c', 'd', 'e', 'r')
  AND (t.typtype <> 'c' OR (SELECT c.relkind FROM pg_catalog.pg_class c WHERE c.oid = t.typrelid) = 'c')
  AND NOT (t.typelem <> 0 AND t.typsubscript = 'pg_catalog.array_subscript_handler'::pg_catalog.regproc)
  AND ($2 OR t.typname = ANY($3))
ORDER BY 2 LIMIT $4`,
}

func relationQuery(relkinds, defaultKind string) string {
	return `SELECT c.oid::int8, c.oid::pg_catalog.regclass::text, c.relname::text, ` + ownerColumns + `,
  ` + extensionOf("pg_catalog.pg_class", "c.oid") + `, false, '', c.relkind::text,
  ` + aclColumns("c.relacl", defaultKind, "c.relowner") + `
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_roles o ON o.oid = c.relowner
WHERE n.nspname = $1 AND c.relkind IN (` + relkinds + `) AND ($2 OR c.relname = ANY($3))
ORDER BY 2 LIMIT $4`
}

func routineQuery(prokinds string) string {
	return `SELECT p.oid::int8, p.oid::pg_catalog.regprocedure::text, p.proname::text, ` + ownerColumns + `,
  ` + extensionOf("pg_catalog.pg_proc", "p.oid") + `, p.prosecdef, l.lanname::text, p.prokind::text,
  ` + aclColumns("p.proacl", "f", "p.proowner") + `
FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
JOIN pg_catalog.pg_language l ON l.oid = p.prolang
JOIN pg_catalog.pg_roles o ON o.oid = p.proowner
WHERE n.nspname = $1 AND p.prokind IN (` + prokinds + `) AND ($2 OR p.proname = ANY($3) OR p.oid::int8 = ANY($5))
ORDER BY 2 LIMIT $4`
}

// SchemaObjectSelector selects the objects ListSchemaObjects returns.
type SchemaObjectSelector struct {
	// All selects every object of the kind in the schema.
	All bool
	// Names selects objects by their name in the schema.
	Names []string
	// OIDs selects routines by OID (see ResolveRoutine).
	OIDs []int64
	// Limit is the most objects returned (0: no limit).
	Limit int
}

// ListSchemaObjects lists the objects of kind in schema (of the connected
// database) that sel selects, ordered by identity, with their owner, the
// extension they belong to and their ACL, in one catalog query.
func (c *Client) ListSchemaObjects(ctx context.Context, schema string, kind SchemaObjectKind, sel SchemaObjectSelector) ([]SchemaObject, error) {
	query, ok := schemaObjectQueries[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported object kind %q", kind)
	}
	limit := sel.Limit
	if limit <= 0 {
		limit = 1 << 30
	}
	args := []any{schema, sel.All, pq.Array(sel.Names), limit}
	if kind == SchemaFunction || kind == SchemaProcedure {
		args = append(args, pq.Array(sel.OIDs))
	}
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list the %s objects of schema %q: %w", strings.ToLower(string(kind)), schema, err)
	}
	defer func() { _ = rows.Close() }()
	var out []SchemaObject
	for rows.Next() {
		var o SchemaObject
		var grantees []int64
		var privileges []string
		var grantable []bool
		if err := rows.Scan(&o.OID, &o.Identity, &o.Name, &o.Owner, &o.OwnerOID, &o.OwnerSuperuser, &o.OwnerIsSessionUser,
			&o.Extension, &o.SecurityDefiner, &o.Language, &o.SubKind,
			pq.Array(&grantees), pq.Array(&privileges), pq.Array(&grantable)); err != nil {
			return nil, fmt.Errorf("failed to scan schema object: %w", err)
		}
		if len(grantees) != len(privileges) || len(grantees) != len(grantable) {
			return nil, fmt.Errorf("inconsistent ACL of %s", o.Identity)
		}
		for i := range grantees {
			o.ACL = append(o.ACL, ACLItem{Grantee: grantees[i], Privilege: privileges[i], Grantable: grantable[i]})
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to list the objects of schema %q: %w", schema, err)
	}
	return out, nil
}

// ResolveRoutine resolves the routine name(args) in schema with the server's
// regprocedure parser: name is quoted by the server and args (the argument
// types, comma-separated) are parsed as type names. It returns the OID, or
// found false when no such routine exists.
func (c *Client) ResolveRoutine(ctx context.Context, schema, name, args string) (oid int64, found bool, err error) {
	var v sql.NullInt64
	err = c.db.QueryRowContext(ctx, `SELECT pg_catalog.to_regprocedure(pg_catalog.quote_ident($1) || '.' ||
  pg_catalog.quote_ident($2) || '(' || $3 || ')')::oid::int8`, schema, name, args).Scan(&v)
	if err != nil {
		return 0, false, fmt.Errorf("failed to resolve %s(%s) in schema %q: %w", name, args, schema, err)
	}
	return v.Int64, v.Valid, nil
}

// RoleOIDs returns the OIDs of the roles in names that exist.
func (c *Client) RoleOIDs(ctx context.Context, names []string) (map[string]int64, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT rolname::text, oid::int8 FROM pg_catalog.pg_roles WHERE rolname = ANY($1)`,
		pq.Array(names))
	if err != nil {
		return nil, fmt.Errorf("failed to look up roles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var oid int64
		if err := rows.Scan(&name, &oid); err != nil {
			return nil, fmt.Errorf("failed to scan role: %w", err)
		}
		out[name] = oid
	}
	return out, rows.Err()
}

// resolveObjectQueries render an object identity of a kind as the server
// does, once it is resolved and found in schema $2 (NULL otherwise). The
// identity is parsed by the server's regclass, regprocedure or regtype
// input function, never spliced.
var resolveObjectQueries = map[SchemaObjectKind]string{
	SchemaTable: `SELECT c.oid::int8, c.oid::pg_catalog.regclass::text FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.oid = pg_catalog.to_regclass($1) AND n.nspname = $2 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')`,
	SchemaSequence: `SELECT c.oid::int8, c.oid::pg_catalog.regclass::text FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.oid = pg_catalog.to_regclass($1) AND n.nspname = $2 AND c.relkind = 'S'`,
	SchemaFunction: `SELECT p.oid::int8, p.oid::pg_catalog.regprocedure::text FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
WHERE p.oid = pg_catalog.to_regprocedure($1) AND n.nspname = $2 AND p.prokind IN ('f', 'a', 'w')`,
	SchemaProcedure: `SELECT p.oid::int8, p.oid::pg_catalog.regprocedure::text FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
WHERE p.oid = pg_catalog.to_regprocedure($1) AND n.nspname = $2 AND p.prokind = 'p'`,
	SchemaType: `SELECT t.oid::int8, t.oid::pg_catalog.regtype::text FROM pg_catalog.pg_type t
JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
WHERE t.oid = pg_catalog.to_regtype($1) AND n.nspname = $2`,
}

// ResolveSchemaObject looks up the object identity (as rendered by
// ListSchemaObjects) of kind in schema and returns the server's current
// rendering of it, or found false when it no longer exists there (dropped,
// renamed, or moved to another schema).
func (c *Client) ResolveSchemaObject(ctx context.Context, kind SchemaObjectKind, schema, identity string) (canonical string, found bool, err error) {
	canonical, _, found, err = c.resolveSchemaObject(ctx, kind, schema, identity)
	return canonical, found, err
}

func (c *Client) resolveSchemaObject(ctx context.Context, kind SchemaObjectKind, schema, identity string) (
	canonical string, oid int64, found bool, err error) {
	query, ok := resolveObjectQueries[kind]
	if !ok {
		return "", 0, false, fmt.Errorf("unsupported object kind %q", kind)
	}
	if identity == "" {
		return "", 0, false, nil
	}
	err = c.db.QueryRowContext(ctx, query, identity, schema).Scan(&oid, &canonical)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("failed to resolve %s %s: %w", strings.ToLower(string(kind)), identity, err)
	}
	return canonical, oid, true, nil
}

// ErrObjectGone reports that the object a GRANT names no longer exists.
var ErrObjectGone = errors.New("the object no longer exists")

// buildSchemaObjectPrivilegesQuery builds GRANT (or REVOKE) of privileges on
// one object of kind, named by canonical: the server's own rendering of it
// (see ResolveSchemaObject), never user input. Privileges are checked against
// the kind's allow-list and the grant option cannot be granted to PUBLIC.
func buildSchemaObjectPrivilegesQuery(grant bool, kind SchemaObjectKind, canonical, grantee string, privileges []string,
	withGrantOption bool, mode RevokeMode) (string, error) {
	allowed, ok := SchemaObjectPrivileges[kind]
	if !ok {
		return "", fmt.Errorf("unsupported object kind %q", kind)
	}
	privs, err := checkPrivileges(strings.ToLower(string(kind)), privileges, allowed)
	if err != nil {
		return "", err
	}
	if canonical == "" {
		return "", errors.New("empty object identity")
	}
	who, err := renderGrantee(grantee)
	if err != nil {
		return "", err
	}
	object := string(kind) + " " + canonical
	if !grant {
		return buildRevokeQuery(privs, object, who, mode), nil
	}
	if withGrantOption && who == PublicGrantee {
		return "", errors.New("the grant option cannot be granted to PUBLIC")
	}
	return buildGrantQuery(privs, object, who, withGrantOption), nil
}

// GrantOnSchemaObject grants privileges on the object identity of kind in
// schema to grantee. The identity is resolved by the server first and the
// statement names the object with the server's rendering; ErrObjectGone is
// returned when it no longer exists, or, with oid set, when the name now
// stands for another object (the one pgop checked was dropped or renamed
// since it listed it).
func (c *Client) GrantOnSchemaObject(ctx context.Context, kind SchemaObjectKind, schema, identity string, oid int64,
	grantee string, privileges []string, withGrantOption bool) error {
	canonical, resolved, found, err := c.resolveSchemaObject(ctx, kind, schema, identity)
	if err != nil {
		return err
	}
	if !found || (oid != 0 && resolved != oid) {
		return fmt.Errorf("%s %s: %w", strings.ToLower(string(kind)), identity, ErrObjectGone)
	}
	query, err := buildSchemaObjectPrivilegesQuery(true, kind, canonical, grantee, privileges, withGrantOption, RevokeMode{})
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to grant privileges on %s %s to %q: %w", strings.ToLower(string(kind)), canonical, grantee, err)
	}
	return nil
}

// RevokeOnSchemaObject revokes privileges (or their grant option) on the
// object identity of kind in schema from grantee. An object that no longer
// exists holds nothing to revoke.
func (c *Client) RevokeOnSchemaObject(ctx context.Context, kind SchemaObjectKind, schema, identity, grantee string,
	privileges []string, mode RevokeMode) error {
	canonical, found, err := c.ResolveSchemaObject(ctx, kind, schema, identity)
	if err != nil || !found {
		return err
	}
	query, err := buildSchemaObjectPrivilegesQuery(false, kind, canonical, grantee, privileges, false, mode)
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to revoke privileges on %s %s from %q: %w", strings.ToLower(string(kind)), canonical, grantee, err)
	}
	return nil
}

// DefaultObjectKind is a kind of future objects default privileges apply
// to. Its value is the keyword of ALTER DEFAULT PRIVILEGES ... ON <kind>.
type DefaultObjectKind string

// Default privilege object kinds.
const (
	DefaultTables    DefaultObjectKind = "TABLES"
	DefaultSequences DefaultObjectKind = "SEQUENCES"
	DefaultFunctions DefaultObjectKind = "FUNCTIONS"
	DefaultTypes     DefaultObjectKind = "TYPES"
)

// defaultObjectKinds maps a default privilege kind to its
// pg_default_acl.defaclobjtype and the object kind whose privileges apply.
var defaultObjectKinds = map[DefaultObjectKind]struct {
	objtype string
	object  SchemaObjectKind
}{
	DefaultTables:    {"r", SchemaTable},
	DefaultSequences: {"S", SchemaSequence},
	DefaultFunctions: {"f", SchemaFunction},
	DefaultTypes:     {"T", SchemaType},
}

// DefaultPrivilegesTarget names the default privileges of one role for one
// kind of object in one schema.
type DefaultPrivilegesTarget struct {
	ForRole string
	Schema  string
	Kind    DefaultObjectKind
}

func (t DefaultPrivilegesTarget) String() string {
	return fmt.Sprintf("default privileges for role %q in schema %q on %s", t.ForRole, t.Schema, strings.ToLower(string(t.Kind)))
}

// buildDefaultPrivilegesQuery builds ALTER DEFAULT PRIVILEGES FOR ROLE r IN
// SCHEMA s GRANT (or REVOKE) privileges ON <kind> TO (FROM) grantee.
func buildDefaultPrivilegesQuery(grant bool, t DefaultPrivilegesTarget, grantee string, privileges []string,
	withGrantOption bool, mode RevokeMode) (string, error) {
	k, ok := defaultObjectKinds[t.Kind]
	if !ok {
		return "", fmt.Errorf("unsupported default privilege kind %q", t.Kind)
	}
	privs, err := checkPrivileges(strings.ToLower(string(k.object)), privileges, SchemaObjectPrivileges[k.object])
	if err != nil {
		return "", err
	}
	if t.ForRole == "" || t.Schema == "" {
		return "", errors.New("default privileges need a role and a schema")
	}
	if IsPublic(t.ForRole) {
		return "", errors.New("default privileges cannot be set for PUBLIC")
	}
	who, err := renderGrantee(grantee)
	if err != nil {
		return "", err
	}
	prefix := fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s ", quoteIdent(t.ForRole), quoteIdent(t.Schema))
	if !grant {
		return prefix + buildRevokeQuery(privs, string(t.Kind), who, mode), nil
	}
	if withGrantOption && who == PublicGrantee {
		return "", errors.New("the grant option cannot be granted to PUBLIC")
	}
	return prefix + buildGrantQuery(privs, string(t.Kind), who, withGrantOption), nil
}

// GrantDefaultPrivileges adds privileges to the default privileges t for
// grantee.
func (c *Client) GrantDefaultPrivileges(ctx context.Context, t DefaultPrivilegesTarget, grantee string, privileges []string,
	withGrantOption bool) error {
	query, err := buildDefaultPrivilegesQuery(true, t, grantee, privileges, withGrantOption, RevokeMode{})
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to grant %s to %q: %w", t, grantee, err)
	}
	return nil
}

// RevokeDefaultPrivileges removes privileges (or their grant option) from
// the default privileges t for grantee.
func (c *Client) RevokeDefaultPrivileges(ctx context.Context, t DefaultPrivilegesTarget, grantee string, privileges []string,
	mode RevokeMode) error {
	query, err := buildDefaultPrivilegesQuery(false, t, grantee, privileges, false, mode)
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to revoke %s from %q: %w", t, grantee, err)
	}
	return nil
}

// HeldDefaultPrivileges returns the privileges the default privileges t give
// grantee (a role or PUBLIC) in the connected database, and those with the
// grant option, both sorted. Only the schema-specific entries (IN SCHEMA)
// count.
func (c *Client) HeldDefaultPrivileges(ctx context.Context, t DefaultPrivilegesTarget, grantee string) (privileges, grantable []string, err error) {
	k, ok := defaultObjectKinds[t.Kind]
	if !ok {
		return nil, nil, fmt.Errorf("unsupported default privilege kind %q", t.Kind)
	}
	rows, err := c.db.QueryContext(ctx, `SELECT a.privilege_type::text, a.is_grantable FROM pg_catalog.pg_default_acl d
JOIN pg_catalog.pg_namespace n ON n.oid = d.defaclnamespace
CROSS JOIN LATERAL pg_catalog.aclexplode(d.defaclacl) a
WHERE d.defaclrole = (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = $1) AND n.nspname = $2
  AND d.defaclobjtype = $3 AND a.grantor = d.defaclrole
  AND a.grantee = CASE WHEN $4 THEN 0::oid ELSE (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = $5) END`,
		t.ForRole, t.Schema, k.objtype, IsPublic(grantee), grantee)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read %s: %w", t, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p string
		var g bool
		if err := rows.Scan(&p, &g); err != nil {
			return nil, nil, fmt.Errorf("failed to scan default privilege: %w", err)
		}
		privileges = append(privileges, p)
		if g {
			grantable = append(grantable, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("failed to read %s: %w", t, err)
	}
	slices.Sort(privileges)
	slices.Sort(grantable)
	return slices.Compact(privileges), slices.Compact(grantable), nil
}
