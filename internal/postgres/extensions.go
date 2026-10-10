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

// ExtensionVersionInfo describes one version of an extension available on
// the server (pg_available_extension_versions).
type ExtensionVersionInfo struct {
	Name    string
	Version string
	// Trusted reports that a non-superuser with CREATE on the database may
	// install this version itself.
	Trusted bool
	// Superuser reports that only a superuser may install it (unless trusted).
	Superuser   bool
	Relocatable bool
	// Schema is the schema the control file requires, "" when none.
	Schema string
	// Requires lists the extensions this version requires.
	Requires []string
}

// ExtensionVersion returns the given version of an extension (its default
// version when version is empty) as pg_available_extension_versions lists
// it. found is false when the extension, or that version, is not available
// on the server.
func (c *Client) ExtensionVersion(ctx context.Context, name, version string) (info ExtensionVersionInfo, found bool, err error) {
	const query = `SELECT v.version, v.trusted, v.superuser, v.relocatable, COALESCE(v.schema::text, ''),
  COALESCE(v.requires::text[], '{}')
FROM pg_catalog.pg_available_extension_versions v
JOIN pg_catalog.pg_available_extensions e ON e.name = v.name
WHERE v.name = $1 AND v.version = COALESCE(NULLIF($2, ''), e.default_version)`
	info.Name = name
	err = c.db.QueryRowContext(ctx, query, name, version).Scan(&info.Version, &info.Trusted, &info.Superuser,
		&info.Relocatable, &info.Schema, pq.Array(&info.Requires))
	if errors.Is(err, sql.ErrNoRows) {
		return ExtensionVersionInfo{}, false, nil
	}
	if err != nil {
		return ExtensionVersionInfo{}, false, fmt.Errorf("failed to look up extension %q: %w", name, err)
	}
	return info, true, nil
}

// InstalledExtension is an extension installed in the connected database.
type InstalledExtension struct {
	Name    string
	Version string
	Schema  string
	// OID and Owner identify this installation: an extension dropped and
	// created again has another OID.
	OID   int64
	Owner string
}

// InstalledExtension returns the extension installed in the connected
// database under name, or nil when it is not installed.
func (c *Client) InstalledExtension(ctx context.Context, name string) (*InstalledExtension, error) {
	ext := &InstalledExtension{Name: name}
	err := c.db.QueryRowContext(ctx, `SELECT e.extversion, n.nspname, e.oid::pg_catalog.int8, pg_catalog.pg_get_userbyid(e.extowner)
FROM pg_catalog.pg_extension e
JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = $1`, name).Scan(&ext.Version, &ext.Schema, &ext.OID, &ext.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to look up installed extension %q: %w", name, err)
	}
	return ext, nil
}

// ExtensionUpdatePaths returns the update paths (chains of update scripts)
// of the extension that end at version to, each as the list of versions it
// goes through, source first. With from set, only the path from that
// version is returned (none when there is no such path).
func (c *Client) ExtensionUpdatePaths(ctx context.Context, name, from, to string) ([][]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT path FROM pg_catalog.pg_extension_update_paths($1)
WHERE target = $2 AND path IS NOT NULL AND ($3 = '' OR source = $3) ORDER BY source`, name, to, from)
	if err != nil {
		return nil, fmt.Errorf("failed to look up the update paths of extension %q: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	var out [][]string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("failed to scan update path: %w", err)
		}
		out = append(out, strings.Split(path, "--"))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to look up the update paths of extension %q: %w", name, err)
	}
	return out, nil
}

// buildCreateExtensionQuery builds CREATE EXTENSION. It never uses IF NOT
// EXISTS: callers check first, and an extension someone else created in the
// meantime must be reported, not taken over.
func buildCreateExtensionQuery(name, schema, version string, cascade bool) string {
	query := "CREATE EXTENSION " + quoteIdent(name)
	if schema != "" {
		query += " SCHEMA " + quoteIdent(schema)
	}
	if version != "" {
		query += " VERSION " + quoteLiteral(version)
	}
	if cascade {
		query += " CASCADE"
	}
	return query
}

// CreateExtension creates an extension in the connected database. It fails
// with ErrObjectExists when the extension exists.
//
// Operator sessions run with search_path pinned to pg_catalog (see
// buildDSN), which would make pg_catalog the default schema for the new
// extension. Without an explicit schema the statement therefore runs with
// search_path set to public for its transaction only, which is where the
// extension went with PostgreSQL's default search_path.
func (c *Client) CreateExtension(ctx context.Context, name, schema, version string, cascade bool) error {
	query := buildCreateExtensionQuery(name, schema, version, cascade)
	wrap := func(err error) error {
		if dup := duplicateObjectError(fmt.Sprintf("extension %q", name), err); dup != nil {
			return dup
		}
		return fmt.Errorf("failed to create extension %q: %w", name, err)
	}
	if schema != "" {
		if _, err := c.db.ExecContext(ctx, query); err != nil {
			return wrap(err)
		}
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return wrap(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path TO public"); err != nil {
		return wrap(err)
	}
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return wrap(err)
	}
	if err := tx.Commit(); err != nil {
		return wrap(err)
	}
	return nil
}

// buildUpdateExtensionQuery builds ALTER EXTENSION ... UPDATE TO.
func buildUpdateExtensionQuery(name, version string) string {
	return fmt.Sprintf("ALTER EXTENSION %s UPDATE TO %s", quoteIdent(name), quoteLiteral(version))
}

// UpdateExtension updates an installed extension to version.
func (c *Client) UpdateExtension(ctx context.Context, name, version string) error {
	if version == "" {
		return errors.New("no version to update to")
	}
	if _, err := c.db.ExecContext(ctx, buildUpdateExtensionQuery(name, version)); err != nil {
		return fmt.Errorf("failed to update extension %q to %q: %w", name, version, err)
	}
	return nil
}

// buildDropExtensionQuery builds DROP EXTENSION without CASCADE.
func buildDropExtensionQuery(name string) string {
	return "DROP EXTENSION " + quoteIdent(name) + " RESTRICT"
}

// DropExtension drops an extension without CASCADE. When other objects
// depend on it, PostgreSQL refuses and the error satisfies
// DependentObjectsExist.
func (c *Client) DropExtension(ctx context.Context, name string) error {
	if _, err := c.db.ExecContext(ctx, buildDropExtensionQuery(name)); err != nil {
		return fmt.Errorf("failed to drop extension %q: %w", name, err)
	}
	return nil
}

// DependentObjectsExist reports whether err is PostgreSQL's
// dependent_objects_still_exist error (SQLSTATE 2BP01), for example a DROP
// without CASCADE of an object others depend on.
func DependentObjectsExist(err error) bool {
	return DependentPrivilegesExist(err)
}

// SchemaWriters describes who can create objects in a schema.
type SchemaWriters struct {
	Exists bool
	// Owner is the schema's owner and OwnerSuperuser whether it is a
	// superuser (pg_database_owner is not: the caller resolves it to the
	// database's owner).
	Owner          string
	OwnerSuperuser bool
	// Creators lists the grantees other than the owner that hold CREATE on
	// the schema directly and are not superusers, PUBLIC for the
	// pseudo-role, sorted.
	Creators []string
}

// SchemaWriters returns who can create objects in the schema name of the
// connected database.
func (c *Client) SchemaWriters(ctx context.Context, name string) (SchemaWriters, error) {
	const query = `SELECT pg_catalog.pg_get_userbyid(n.nspowner), COALESCE(o.rolsuper, false),
  ARRAY(SELECT DISTINCT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE COALESCE(g.rolname::text, a.grantee::text) END
    FROM pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a
    LEFT JOIN pg_catalog.pg_roles g ON g.oid = a.grantee
    WHERE a.privilege_type = 'CREATE' AND a.grantee <> n.nspowner AND (a.grantee = 0 OR NOT COALESCE(g.rolsuper, false))
    ORDER BY 1)
FROM pg_catalog.pg_namespace n LEFT JOIN pg_catalog.pg_roles o ON o.oid = n.nspowner
WHERE n.nspname = $1`
	w := SchemaWriters{Exists: true}
	err := c.db.QueryRowContext(ctx, query, name).Scan(&w.Owner, &w.OwnerSuperuser, pq.Array(&w.Creators))
	if errors.Is(err, sql.ErrNoRows) {
		return SchemaWriters{}, nil
	}
	if err != nil {
		return SchemaWriters{}, fmt.Errorf("failed to read who can create in schema %q: %w", name, err)
	}
	return w, nil
}

// MemberKind is a kind of extension member object privileges are granted
// on. Its value is the keyword of GRANT ... ON <kind>.
type MemberKind string

// Extension member kinds.
const (
	MemberTables    MemberKind = "TABLE"
	MemberSequences MemberKind = "SEQUENCE"
	MemberRoutines  MemberKind = "ROUTINE"
)

// Table privileges offered on extension members. TRIGGER is deliberately
// missing: a trigger on a table the extension's superuser-run code writes to
// would run as that superuser.
const (
	PrivilegeSelect     = "SELECT"
	PrivilegeInsert     = "INSERT"
	PrivilegeUpdate     = "UPDATE"
	PrivilegeDelete     = "DELETE"
	PrivilegeTruncate   = "TRUNCATE"
	PrivilegeReferences = "REFERENCES"
	PrivilegeMaintain   = "MAINTAIN"
	PrivilegeExecute    = "EXECUTE"
)

// memberPrivileges are the privileges accepted per member kind (ALL is
// expanded by the caller and not accepted here).
var memberPrivileges = map[MemberKind][]string{
	MemberTables: {PrivilegeSelect, PrivilegeInsert, PrivilegeUpdate, PrivilegeDelete, PrivilegeTruncate,
		PrivilegeReferences, PrivilegeMaintain},
	MemberSequences: {PrivilegeUsage, PrivilegeSelect, PrivilegeUpdate},
	MemberRoutines:  {PrivilegeExecute},
}

// MinMaintainPrivilegeVersion is the first server_version_num with the
// MAINTAIN table privilege (PostgreSQL 17).
const MinMaintainPrivilegeVersion = 170000

// ExtensionMember is an object that belongs to an extension.
type ExtensionMember struct {
	// Identity is the object as the server renders it (oid::regclass or
	// oid::regprocedure, schema-qualified and quoted by the server). It is
	// only ever taken from the server, never from user input.
	Identity string
	// Eligible reports that pgop may grant on the object: a plain or
	// partitioned table, a sequence, or a function or procedure written in
	// SQL or PL/pgSQL that is not SECURITY DEFINER.
	Eligible bool
	// Held lists the privileges the grantee holds directly on the object
	// from its owner.
	Held []string
}

// extensionMemberSelect selects the members of extension $1 of one kind with
// the privileges grantee ($2 PUBLIC, $3 role name) holds on them.
var extensionMemberQueries = map[MemberKind]string{
	MemberTables: `SELECT c.oid::pg_catalog.regclass::text, c.relkind IN ('r', 'p'),
  ARRAY(SELECT a.privilege_type::text FROM pg_catalog.aclexplode(COALESCE(c.relacl, pg_catalog.acldefault('r', c.relowner))) a
    WHERE a.grantor = c.relowner AND a.grantee = ` + granteeOIDExpr + ` ORDER BY 1)
FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_class c ON c.oid = d.objid
WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
  AND d.refobjid = (SELECT oid FROM pg_catalog.pg_extension WHERE extname = $1) AND d.deptype = 'e'
  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
ORDER BY 1`,
	MemberSequences: `SELECT c.oid::pg_catalog.regclass::text, true,
  ARRAY(SELECT a.privilege_type::text FROM pg_catalog.aclexplode(COALESCE(c.relacl, pg_catalog.acldefault('s', c.relowner))) a
    WHERE a.grantor = c.relowner AND a.grantee = ` + granteeOIDExpr + ` ORDER BY 1)
FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_class c ON c.oid = d.objid
WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
  AND d.refobjid = (SELECT oid FROM pg_catalog.pg_extension WHERE extname = $1) AND d.deptype = 'e'
  AND c.relkind = 'S'
ORDER BY 1`,
	MemberRoutines: `SELECT p.oid::pg_catalog.regprocedure::text,
  p.prokind IN ('f', 'p') AND NOT p.prosecdef AND l.lanname IN ('sql', 'plpgsql'),
  ARRAY(SELECT a.privilege_type::text FROM pg_catalog.aclexplode(COALESCE(p.proacl, pg_catalog.acldefault('f', p.proowner))) a
    WHERE a.grantor = p.proowner AND a.grantee = ` + granteeOIDExpr + ` ORDER BY 1)
FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_proc p ON p.oid = d.objid
JOIN pg_catalog.pg_language l ON l.oid = p.prolang
WHERE d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
  AND d.refobjid = (SELECT oid FROM pg_catalog.pg_extension WHERE extname = $1) AND d.deptype = 'e'
ORDER BY 1`,
}

// ExtensionMembers lists the objects of kind that belong to the extension
// (pg_depend deptype 'e') in the connected database, with whether pgop may
// grant on them and what grantee (a role or PUBLIC) holds on them directly
// from their owner. It returns nothing when the extension is not installed.
func (c *Client) ExtensionMembers(ctx context.Context, extension string, kind MemberKind, grantee string) ([]ExtensionMember, error) {
	query, ok := extensionMemberQueries[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported extension member kind %q", kind)
	}
	rows, err := c.db.QueryContext(ctx, query, extension, IsPublic(grantee), grantee)
	if err != nil {
		return nil, fmt.Errorf("failed to list the %s members of extension %q: %w", strings.ToLower(string(kind)), extension, err)
	}
	defer func() { _ = rows.Close() }()
	var out []ExtensionMember
	for rows.Next() {
		var m ExtensionMember
		if err := rows.Scan(&m.Identity, &m.Eligible, pq.Array(&m.Held)); err != nil {
			return nil, fmt.Errorf("failed to scan extension member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to list the members of extension %q: %w", extension, err)
	}
	return out, nil
}

// memberBatch is the most objects one GRANT or REVOKE on members names.
const memberBatch = 100

// buildMemberPrivilegesQueries builds GRANT (or REVOKE) of privileges on the
// member objects identities (server-rendered, see ExtensionMember) of kind,
// in batches. Privileges are checked against the kind's allow-list.
func buildMemberPrivilegesQueries(grant bool, kind MemberKind, identities, privileges []string, grantee string) ([]string, error) {
	allowed, ok := memberPrivileges[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported extension member kind %q", kind)
	}
	privs, err := checkPrivileges(strings.ToLower(string(kind)), privileges, allowed)
	if err != nil {
		return nil, err
	}
	to, err := renderGrantee(grantee)
	if err != nil {
		return nil, err
	}
	var out []string
	for start := 0; start < len(identities); start += memberBatch {
		batch := identities[start:min(start+memberBatch, len(identities))]
		if slices.Contains(batch, "") {
			return nil, errors.New("empty object identity")
		}
		object := string(kind) + " " + strings.Join(batch, ", ")
		if grant {
			out = append(out, buildGrantQuery(privs, object, to, false))
		} else {
			out = append(out, buildRevokeQuery(privs, object, to, RevokeMode{}))
		}
	}
	return out, nil
}

// GrantOnMembers grants privileges on the extension member objects
// identities (as returned by ExtensionMembers) of kind to grantee.
func (c *Client) GrantOnMembers(ctx context.Context, kind MemberKind, identities, privileges []string, grantee string) error {
	return c.execMemberQueries(ctx, true, kind, identities, privileges, grantee)
}

// RevokeOnMembers revokes privileges on the extension member objects
// identities of kind from grantee.
func (c *Client) RevokeOnMembers(ctx context.Context, kind MemberKind, identities, privileges []string, grantee string) error {
	return c.execMemberQueries(ctx, false, kind, identities, privileges, grantee)
}

func (c *Client) execMemberQueries(ctx context.Context, grant bool, kind MemberKind, identities, privileges []string, grantee string) error {
	queries, err := buildMemberPrivilegesQueries(grant, kind, identities, privileges, grantee)
	if err != nil {
		return err
	}
	verb := "revoke"
	if grant {
		verb = "grant"
	}
	for _, q := range queries {
		if _, err := c.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("failed to %s privileges on extension %s objects for %q: %w", verb, strings.ToLower(string(kind)), grantee, err)
		}
	}
	return nil
}

// RevokeAllExtensionMemberPrivileges revokes every privilege role holds on
// objects that belong to an extension, in the connected database, with
// CASCADE (the role is about to be dropped).
func (c *Client) RevokeAllExtensionMemberPrivileges(ctx context.Context, role string) error {
	rows, err := c.db.QueryContext(ctx, `SELECT m.kind, m.ident FROM (
  SELECT CASE WHEN c.relkind = 'S' THEN 'SEQUENCE' ELSE 'TABLE' END AS kind, c.oid::pg_catalog.regclass::text AS ident, c.relacl AS acl
  FROM pg_catalog.pg_class c JOIN pg_catalog.pg_depend d ON d.objid = c.oid
  WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
    AND d.deptype = 'e' AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
  UNION ALL
  SELECT 'ROUTINE', p.oid::pg_catalog.regprocedure::text, p.proacl
  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_depend d ON d.objid = p.oid
  WHERE d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
    AND d.deptype = 'e'
) m WHERE EXISTS (SELECT 1 FROM pg_catalog.aclexplode(m.acl) a
  WHERE a.grantee = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1))`, role)
	if err != nil {
		return fmt.Errorf("failed to list extension object privileges of %q: %w", role, err)
	}
	type obj struct{ kind, ident string }
	var objs []obj
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.kind, &o.ident); err != nil {
			_ = rows.Close()
			return fmt.Errorf("failed to scan extension object: %w", err)
		}
		objs = append(objs, o)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("failed to list extension object privileges of %q: %w", role, err)
	}
	_ = rows.Close()
	for _, o := range objs {
		query := buildRevokeQuery([]string{PrivilegeAll}, o.kind+" "+o.ident, quoteIdent(role), RevokeMode{Cascade: true})
		if _, err := c.db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("failed to revoke privileges on %s %s from %q: %w", strings.ToLower(o.kind), o.ident, role, err)
		}
	}
	return nil
}
