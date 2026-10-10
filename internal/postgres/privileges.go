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
	"regexp"
	"slices"
	"strings"
)

// Privilege keywords accepted by the GRANT/REVOKE builders.
const (
	PrivilegeAll       = "ALL"
	PrivilegeConnect   = "CONNECT"
	PrivilegeCreate    = "CREATE"
	PrivilegeTemporary = "TEMPORARY"
	PrivilegeTemp      = "TEMP"
	PrivilegeUsage     = "USAGE"
	PrivilegeSet       = "SET"

	// privilegeAllPrivileges is the long form of ALL.
	privilegeAllPrivileges = "ALL PRIVILEGES"
)

// paramSearchPath is the search_path parameter, a list parameter.
const paramSearchPath = "search_path"

// Privilege keywords are spliced into GRANT/REVOKE statements (they cannot be
// passed as bind parameters), so every privilege is checked against a fixed
// allow-list before a statement is built. The CRDs enforce the same lists;
// this is defense in depth against objects that bypassed validation.
var (
	// schemaPrivileges are the privileges accepted for GRANT ... ON SCHEMA.
	schemaPrivileges = []string{PrivilegeUsage, PrivilegeCreate, PrivilegeAll}
	// databasePrivileges are the privileges accepted for GRANT ... ON
	// DATABASE. TEMP is an alias of TEMPORARY.
	databasePrivileges = []string{PrivilegeConnect, PrivilegeCreate, PrivilegeTemporary, PrivilegeTemp, PrivilegeAll}
	// parameterPrivileges are the privileges accepted for GRANT ... ON
	// PARAMETER. ALTER SYSTEM is deliberately not offered: it lets the
	// grantee rewrite the server configuration.
	parameterPrivileges = []string{PrivilegeSet}
)

// MinParameterPrivilegesVersion is the first server_version_num that supports
// GRANT ... ON PARAMETER (PostgreSQL 15).
const MinParameterPrivilegesVersion = 150000

// parameterNameRe matches a configuration parameter name: identifiers
// separated by dots (custom parameters such as myapp.tenant_id).
var parameterNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// maxParameterNameLength bounds parameter names (two 63-byte identifiers).
const maxParameterNameLength = 127

// checkPrivileges upper-cases privs (ALL PRIVILEGES is read as ALL) and
// checks each against allowed. It returns the upper-cased privileges in their
// original order, without duplicates.
func checkPrivileges(kind string, privs, allowed []string) ([]string, error) {
	if len(privs) == 0 {
		return nil, fmt.Errorf("no %s privileges given", kind)
	}
	out := make([]string, 0, len(privs))
	for _, p := range privs {
		u := strings.ToUpper(strings.Join(strings.Fields(p), " "))
		if u == privilegeAllPrivileges {
			u = PrivilegeAll
		}
		if !slices.Contains(allowed, u) {
			return nil, fmt.Errorf("invalid %s privilege %q (allowed: %s)", kind, p, strings.Join(allowed, ", "))
		}
		if !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out, nil
}

// NormalizeDatabasePrivileges validates database privileges and returns them
// in canonical form: sorted, without duplicates, TEMP as TEMPORARY and ALL
// expanded to CONNECT, CREATE and TEMPORARY (the privileges ALL covers on a
// database).
func NormalizeDatabasePrivileges(privs []string) ([]string, error) {
	checked, err := checkPrivileges("database", privs, databasePrivileges)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range checked {
		switch p {
		case PrivilegeAll:
			out = append(out, PrivilegeConnect, PrivilegeCreate, PrivilegeTemporary)
		case PrivilegeTemp:
			out = append(out, PrivilegeTemporary)
		default:
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// NormalizeSchemaPrivileges validates schema privileges and returns them in
// canonical form: sorted, without duplicates and ALL expanded to CREATE and
// USAGE (the privileges ALL covers on a schema).
func NormalizeSchemaPrivileges(privs []string) ([]string, error) {
	checked, err := checkPrivileges("schema", privs, schemaPrivileges)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range checked {
		if p == PrivilegeAll {
			out = append(out, PrivilegeCreate, PrivilegeUsage)
			continue
		}
		out = append(out, p)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// NormalizeParameterPrivileges validates parameter privileges and returns
// them sorted and without duplicates. Empty privs means SET.
func NormalizeParameterPrivileges(privs []string) ([]string, error) {
	if len(privs) == 0 {
		return []string{PrivilegeSet}, nil
	}
	out, err := checkPrivileges("parameter", privs, parameterPrivileges)
	if err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

// NormalizeParameterName validates a configuration parameter name and returns
// it lowercased (parameter names are case-insensitive).
func NormalizeParameterName(name string) (string, error) {
	if len(name) > maxParameterNameLength || !parameterNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid parameter name %q", name)
	}
	return strings.ToLower(name), nil
}

// quoteParameterName validates name and quotes each dot-separated part, so
// parameter names that are SQL keywords parse. Parts are lowercased first,
// which is what PostgreSQL does with an unquoted name.
func quoteParameterName(name string) (string, error) {
	n, err := NormalizeParameterName(name)
	if err != nil {
		return "", err
	}
	parts := strings.Split(n, ".")
	for i, p := range parts {
		parts[i] = quoteIdent(p)
	}
	return strings.Join(parts, "."), nil
}

// ObjectKind is the kind of object privileges are granted on. Each kind has
// a fixed privilege allow-list and renders its object name itself, so a new
// kind (tables, functions, ...) plugs into the same GRANT/REVOKE builders.
type ObjectKind string

// Object kinds supported by GrantPrivileges and RevokePrivileges.
const (
	ObjectDatabase  ObjectKind = "DATABASE"
	ObjectSchema    ObjectKind = "SCHEMA"
	ObjectParameter ObjectKind = "PARAMETER"
)

// objectKind describes how privileges on one kind of object are granted.
type objectKind struct {
	// label names the kind in errors ("database privilege").
	label string
	// privileges is the allow-list of privilege keywords.
	privileges []string
	// render returns the object as it appears after ON, quoted.
	render func(name string) (string, error)
}

// quotedName renders an object whose name is a single identifier.
func quotedName(keyword string) func(string) (string, error) {
	return func(name string) (string, error) {
		if name == "" {
			return "", errors.New("empty object name")
		}
		return keyword + " " + quoteIdent(name), nil
	}
}

var objectKinds = map[ObjectKind]objectKind{
	ObjectDatabase: {label: "database", privileges: databasePrivileges, render: quotedName("DATABASE")},
	ObjectSchema:   {label: "schema", privileges: schemaPrivileges, render: quotedName("SCHEMA")},
	ObjectParameter: {label: "parameter", privileges: parameterPrivileges, render: func(name string) (string, error) {
		quoted, err := quoteParameterName(name)
		if err != nil {
			return "", err
		}
		return "PARAMETER " + quoted, nil
	}},
}

// PrivilegeObject is an object privileges are granted on.
type PrivilegeObject struct {
	Kind ObjectKind
	// Name is the database, schema or parameter name (unquoted).
	Name string
}

func (o PrivilegeObject) String() string {
	if k, ok := objectKinds[o.Kind]; ok {
		return fmt.Sprintf("%s %q", k.label, o.Name)
	}
	return fmt.Sprintf("%s %q", o.Kind, o.Name)
}

// render checks privs against the kind's allow-list and renders the object.
func (o PrivilegeObject) render(privs []string) (checked []string, object string, err error) {
	k, ok := objectKinds[o.Kind]
	if !ok {
		return nil, "", fmt.Errorf("unsupported object kind %q", o.Kind)
	}
	if checked, err = checkPrivileges(k.label, privs, k.privileges); err != nil {
		return nil, "", err
	}
	if object, err = k.render(o.Name); err != nil {
		return nil, "", err
	}
	return checked, object, nil
}

// PublicGrantee is the PUBLIC pseudo-role: every role, present and future.
const PublicGrantee = "PUBLIC"

// IsPublic reports whether grantee names the PUBLIC pseudo-role. PostgreSQL
// reads both the keyword PUBLIC and the quoted identifier "public" as the
// pseudo-role (no role can be named public), so every letter case of public
// is treated as PUBLIC: a role literally named "PUBLIC" can never be a
// grantee.
func IsPublic(grantee string) bool {
	return strings.EqualFold(grantee, PublicGrantee)
}

// CanonicalGrantee returns PUBLIC for every spelling of public and grantee
// unchanged otherwise.
func CanonicalGrantee(grantee string) string {
	if IsPublic(grantee) {
		return PublicGrantee
	}
	return grantee
}

// renderGrantee renders the grantee of a GRANT or REVOKE: the bare keyword
// PUBLIC for the pseudo-role (a quoted "PUBLIC" would name an ordinary role
// called PUBLIC), otherwise a quoted identifier. The reserved role name none
// is refused.
func renderGrantee(grantee string) (string, error) {
	switch {
	case grantee == "":
		return "", errors.New("empty grantee")
	case IsPublic(grantee):
		return PublicGrantee, nil
	case strings.EqualFold(grantee, "none"):
		return "", fmt.Errorf("invalid grantee %q: the role name none is reserved", grantee)
	}
	return quoteIdent(grantee), nil
}

// buildGrantQuery builds GRANT <privs> ON <object> TO <grantee>. object and
// grantee must already be rendered; privs must already be checked against an
// allow-list.
func buildGrantQuery(privs []string, object, grantee string, withGrantOption bool) string {
	query := fmt.Sprintf("GRANT %s ON %s TO %s", strings.Join(privs, ", "), object, grantee)
	if withGrantOption {
		query += " WITH GRANT OPTION"
	}
	return query
}

// RevokeMode selects the form of a REVOKE statement.
type RevokeMode struct {
	// GrantOptionOnly revokes only the grant option (REVOKE GRANT OPTION FOR).
	GrantOptionOnly bool
	// Cascade also revokes privileges the grantee passed on to others. Use
	// it when the privileges were granted WITH GRANT OPTION: without it the
	// REVOKE fails with "dependent privileges exist".
	Cascade bool
}

// buildRevokeQuery builds REVOKE [GRANT OPTION FOR] <privs> ON <object> FROM
// <grantee> [CASCADE]. object and grantee must already be rendered; privs
// must already be checked.
func buildRevokeQuery(privs []string, object, grantee string, mode RevokeMode) string {
	prefix := "REVOKE "
	if mode.GrantOptionOnly {
		prefix += "GRANT OPTION FOR "
	}
	query := fmt.Sprintf("%s%s ON %s FROM %s", prefix, strings.Join(privs, ", "), object, grantee)
	if mode.Cascade {
		query += " CASCADE"
	}
	return query
}

// buildGrantPrivilegesQuery builds the GRANT of privileges on obj to grantee
// (a role name or PUBLIC). Privileges are checked against the object kind's
// allow-list, names are quoted, and the grant option cannot be given to
// PUBLIC.
func buildGrantPrivilegesQuery(obj PrivilegeObject, grantee string, privileges []string, withGrantOption bool) (string, error) {
	privs, object, err := obj.render(privileges)
	if err != nil {
		return "", err
	}
	to, err := renderGrantee(grantee)
	if err != nil {
		return "", err
	}
	if withGrantOption && to == PublicGrantee {
		return "", errors.New("the grant option cannot be granted to PUBLIC")
	}
	return buildGrantQuery(privs, object, to, withGrantOption), nil
}

// buildRevokePrivilegesQuery builds the REVOKE of privileges (or, with
// mode.GrantOptionOnly, of their grant option) on obj from grantee.
func buildRevokePrivilegesQuery(obj PrivilegeObject, grantee string, privileges []string, mode RevokeMode) (string, error) {
	privs, object, err := obj.render(privileges)
	if err != nil {
		return "", err
	}
	from, err := renderGrantee(grantee)
	if err != nil {
		return "", err
	}
	return buildRevokeQuery(privs, object, from, mode), nil
}

// GrantPrivileges grants privileges on obj to grantee (a role name, or
// PUBLIC in any letter case). The privileges are checked against the object
// kind's allow-list before any SQL is built.
func (c *Client) GrantPrivileges(ctx context.Context, obj PrivilegeObject, grantee string, privileges []string, withGrantOption bool) error {
	query, err := buildGrantPrivilegesQuery(obj, grantee, privileges, withGrantOption)
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to grant privileges on %s to %q: %w", obj, grantee, err)
	}
	return nil
}

// RevokePrivileges revokes privileges (or, with mode.GrantOptionOnly, only
// their grant option) on obj from grantee.
func (c *Client) RevokePrivileges(ctx context.Context, obj PrivilegeObject, grantee string, privileges []string, mode RevokeMode) error {
	query, err := buildRevokePrivilegesQuery(obj, grantee, privileges, mode)
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to revoke privileges on %s from %q: %w", obj, grantee, err)
	}
	return nil
}

// publicPrivilegesQueries list the privileges PUBLIC holds on a database or
// schema: the entries for grantee 0 in its ACL, or in the default ACL when
// the ACL is NULL (PostgreSQL's built-in defaults). A missing object yields
// no row; an object PUBLIC holds nothing on yields one row with a NULL
// privilege. The schema query reads the catalog of the database the client
// is connected to.
var publicPrivilegesQueries = map[ObjectKind]string{
	ObjectDatabase: `SELECT a.privilege_type FROM pg_catalog.pg_database d
LEFT JOIN LATERAL pg_catalog.aclexplode(COALESCE(d.datacl, pg_catalog.acldefault('d', d.datdba))) a ON a.grantee = 0
WHERE d.datname = $1`,
	ObjectSchema: `SELECT a.privilege_type FROM pg_catalog.pg_namespace n
LEFT JOIN LATERAL pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a ON a.grantee = 0
WHERE n.nspname = $1`,
}

// PublicPrivileges returns the privileges PUBLIC holds on obj (a database,
// or a schema of the connected database), sorted. found is false when the
// object does not exist.
func (c *Client) PublicPrivileges(ctx context.Context, obj PrivilegeObject) (privileges []string, found bool, err error) {
	query, ok := publicPrivilegesQueries[obj.Kind]
	if !ok {
		return nil, false, fmt.Errorf("unsupported object kind %q", obj.Kind)
	}
	rows, err := c.db.QueryContext(ctx, query, obj.Name)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read the privileges of PUBLIC on %s: %w", obj, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		found = true
		var p sql.NullString
		if err := rows.Scan(&p); err != nil {
			return nil, false, fmt.Errorf("failed to scan privilege: %w", err)
		}
		if p.Valid {
			privileges = append(privileges, p.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("failed to read the privileges of PUBLIC on %s: %w", obj, err)
	}
	slices.Sort(privileges)
	return slices.Compact(privileges), found, nil
}

// SchemaExists reports whether the schema exists in the database the client
// is connected to.
func (c *Client) SchemaExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := c.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)", name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check schema existence: %w", err)
	}
	return exists, nil
}

// listQuoteParameters are the parameters whose SET values are lists of
// identifiers (GUC_LIST_QUOTE): PostgreSQL quotes each SET argument as an
// identifier, so a list must be passed as separate arguments.
// (session_preload_libraries and local_preload_libraries are list
// parameters too, but they are denied by DeniedParameter.)
var listQuoteParameters = []string{paramSearchPath, settingTempTablespaces}

// errUnterminatedQuote reports a list element with an unterminated quote.
var errUnterminatedQuote = errors.New("unterminated quoted identifier")

// splitIdentifierList splits a postgresql.conf style identifier list
// ('"$user", public') into its elements, like PostgreSQL's
// SplitIdentifierString: elements are separated by commas, surrounding
// whitespace is dropped, double-quoted elements keep their case (with ""
// meaning a quote) and unquoted elements are lowercased.
func splitIdentifierList(s string) ([]string, error) {
	var out []string
	i := 0
	n := len(s)
	isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
	for {
		for i < n && isSpace(s[i]) {
			i++
		}
		var elem strings.Builder
		if i < n && s[i] == '"' {
			i++
			for {
				if i >= n {
					return nil, errUnterminatedQuote
				}
				if s[i] == '"' {
					if i+1 < n && s[i+1] == '"' {
						elem.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				elem.WriteByte(s[i])
				i++
			}
			for i < n && isSpace(s[i]) {
				i++
			}
		} else {
			start := i
			for i < n && s[i] != ',' {
				i++
			}
			word := strings.TrimRight(s[start:i], " \t\n\r")
			if strings.ContainsAny(word, " \t\n\r\"") {
				return nil, fmt.Errorf("invalid list element %q (quote it with double quotes)", word)
			}
			elem.WriteString(strings.ToLower(word))
		}
		if elem.Len() == 0 {
			return nil, errors.New("empty list element")
		}
		out = append(out, elem.String())
		if i >= n {
			return out, nil
		}
		if s[i] != ',' {
			return nil, fmt.Errorf("unexpected %q after quoted identifier", s[i])
		}
		i++
	}
}

// formatSettingValue renders value as the argument list of SET for
// parameter: one string literal, or for list parameters one literal per
// element. An empty list cannot be expressed this way (SET search_path TO ”
// stores a schema literally named ""), so it is rejected.
func formatSettingValue(parameter, value string) (string, error) {
	if !slices.Contains(listQuoteParameters, parameter) {
		return quoteLiteral(value), nil
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s: an empty list is not supported; remove the key to reset the parameter", parameter)
	}
	elems, err := splitIdentifierList(value)
	if err != nil {
		return "", fmt.Errorf("invalid list value for %s: %w", parameter, err)
	}
	lits := make([]string, len(elems))
	for i, e := range elems {
		lits[i] = quoteLiteral(e)
	}
	return strings.Join(lits, ", "), nil
}

func buildAlterDatabaseSetQuery(database, parameter, value string) (string, error) {
	name, err := quoteParameterName(parameter)
	if err != nil {
		return "", err
	}
	norm, _ := NormalizeParameterName(parameter)
	v, err := formatSettingValue(norm, value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ALTER DATABASE %s SET %s TO %s", quoteIdent(database), name, v), nil
}

func buildAlterDatabaseResetQuery(database, parameter string) (string, error) {
	name, err := quoteParameterName(parameter)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ALTER DATABASE %s RESET %s", quoteIdent(database), name), nil
}

// SetDatabaseParameter sets a per-database default for a configuration
// parameter (ALTER DATABASE ... SET).
func (c *Client) SetDatabaseParameter(ctx context.Context, database, parameter, value string) error {
	query, err := buildAlterDatabaseSetQuery(database, parameter, value)
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to set %s on database %q: %w", parameter, database, err)
	}
	return nil
}

// ResetDatabaseParameter removes a per-database default for a configuration
// parameter (ALTER DATABASE ... RESET).
func (c *Client) ResetDatabaseParameter(ctx context.Context, database, parameter string) error {
	query, err := buildAlterDatabaseResetQuery(database, parameter)
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to reset %s on database %q: %w", parameter, database, err)
	}
	return nil
}

// deniedParameters are parameters that pgop never sets per database and
// never grants SET on, whatever their context: they switch the session's
// identity (role, session_authorization), load code into sessions
// (*_preload_libraries, dynamic_library_path, jit_provider) or bypass
// triggers, foreign keys and replication safeguards
// (session_replication_role) or large-object permission checks
// (lo_compat_privileges).
var deniedParameters = []string{
	settingRole, "session_authorization",
	"session_preload_libraries", "local_preload_libraries", "shared_preload_libraries",
	"dynamic_library_path", "jit_provider",
	"session_replication_role", "lo_compat_privileges",
}

// deniedParameterPrefixes are custom parameter namespaces that pgop never
// sets or grants, because they control auditing or security extensions that
// may only be loaded later (when the parameter is still a placeholder and its
// context cannot be checked).
var deniedParameterPrefixes = []string{"pgaudit.", "set_user.", "anon.", "sepgsql."}

// DeniedParameter reports whether pgop refuses to set or grant the parameter
// name (case-insensitive) regardless of the server's view of it.
func DeniedParameter(name string) bool {
	n := strings.ToLower(name)
	if slices.Contains(deniedParameters, n) {
		return true
	}
	for _, p := range deniedParameterPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// ParameterContext returns the context of a configuration parameter as
// reported by pg_settings (user, superuser, postmaster, ...). found is false
// for parameters the server does not know (custom placeholders).
func (c *Client) ParameterContext(ctx context.Context, name string) (pgContext string, found bool, err error) {
	err = c.db.QueryRowContext(ctx, "SELECT context FROM pg_settings WHERE lower(name) = lower($1)", name).Scan(&pgContext)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to look up parameter %q: %w", name, err)
	}
	return pgContext, true, nil
}
