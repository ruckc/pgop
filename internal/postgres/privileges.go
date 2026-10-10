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
		if u == "ALL PRIVILEGES" {
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

// buildGrantQuery builds GRANT <privs> ON <object> TO <role>. object must
// already be quoted; privs must already be checked against an allow-list.
func buildGrantQuery(privs []string, object, role string, withGrantOption bool) string {
	query := fmt.Sprintf("GRANT %s ON %s TO %s", strings.Join(privs, ", "), object, quoteIdent(role))
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
// <role> [CASCADE]. object must already be quoted; privs must already be
// checked.
func buildRevokeQuery(privs []string, object, role string, mode RevokeMode) string {
	prefix := "REVOKE "
	if mode.GrantOptionOnly {
		prefix += "GRANT OPTION FOR "
	}
	query := fmt.Sprintf("%s%s ON %s FROM %s", prefix, strings.Join(privs, ", "), object, quoteIdent(role))
	if mode.Cascade {
		query += " CASCADE"
	}
	return query
}

func buildGrantSchemaPrivilegesQuery(schema, role string, privileges []string, withGrantOption bool) (string, error) {
	privs, err := checkPrivileges("schema", privileges, schemaPrivileges)
	if err != nil {
		return "", err
	}
	return buildGrantQuery(privs, "SCHEMA "+quoteIdent(schema), role, withGrantOption), nil
}

func buildGrantDatabasePrivilegesQuery(database, role string, privileges []string, withGrantOption bool) (string, error) {
	privs, err := checkPrivileges("database", privileges, databasePrivileges)
	if err != nil {
		return "", err
	}
	return buildGrantQuery(privs, "DATABASE "+quoteIdent(database), role, withGrantOption), nil
}

func buildRevokeDatabasePrivilegesQuery(database, role string, privileges []string, mode RevokeMode) (string, error) {
	privs, err := checkPrivileges("database", privileges, databasePrivileges)
	if err != nil {
		return "", err
	}
	return buildRevokeQuery(privs, "DATABASE "+quoteIdent(database), role, mode), nil
}

func buildGrantParameterQuery(parameter, role string, privileges []string, withGrantOption bool) (string, error) {
	privs, err := checkPrivileges("parameter", privileges, parameterPrivileges)
	if err != nil {
		return "", err
	}
	name, err := quoteParameterName(parameter)
	if err != nil {
		return "", err
	}
	return buildGrantQuery(privs, "PARAMETER "+name, role, withGrantOption), nil
}

func buildRevokeParameterQuery(parameter, role string, privileges []string, mode RevokeMode) (string, error) {
	privs, err := checkPrivileges("parameter", privileges, parameterPrivileges)
	if err != nil {
		return "", err
	}
	name, err := quoteParameterName(parameter)
	if err != nil {
		return "", err
	}
	return buildRevokeQuery(privs, "PARAMETER "+name, role, mode), nil
}

// GrantDatabasePrivileges grants database-level privileges on database to
// role. The privileges are checked against the database allow-list.
func (c *Client) GrantDatabasePrivileges(ctx context.Context, database, role string, privileges []string, withGrantOption bool) error {
	query, err := buildGrantDatabasePrivilegesQuery(database, role, privileges, withGrantOption)
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to grant privileges on database %q to %q: %w", database, role, err)
	}
	return nil
}

// RevokeDatabasePrivileges revokes database-level privileges (or, with
// mode.GrantOptionOnly, only their grant option) on database from role.
func (c *Client) RevokeDatabasePrivileges(ctx context.Context, database, role string, privileges []string, mode RevokeMode) error {
	query, err := buildRevokeDatabasePrivilegesQuery(database, role, privileges, mode)
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to revoke privileges on database %q from %q: %w", database, role, err)
	}
	return nil
}

// GrantParameterPrivileges grants privileges on a configuration parameter
// to role (PostgreSQL 15+).
func (c *Client) GrantParameterPrivileges(ctx context.Context, parameter, role string, privileges []string, withGrantOption bool) error {
	query, err := buildGrantParameterQuery(parameter, role, privileges, withGrantOption)
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to grant privileges on parameter %q to %q: %w", parameter, role, err)
	}
	return nil
}

// RevokeParameterPrivileges revokes privileges (or, with
// mode.GrantOptionOnly, only their grant option) on a configuration parameter
// from role.
func (c *Client) RevokeParameterPrivileges(ctx context.Context, parameter, role string, privileges []string, mode RevokeMode) error {
	query, err := buildRevokeParameterQuery(parameter, role, privileges, mode)
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to revoke privileges on parameter %q from %q: %w", parameter, role, err)
	}
	return nil
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
