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
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	defaultSSLMode  = "disable"
	defaultDatabase = "postgres"
	// connectTimeout bounds the TCP connect of a DialAddress connection.
	connectTimeout = 10 * time.Second
)

// Client provides PostgreSQL database operations
type Client struct {
	db *sql.DB
}

// SSL modes understood by ConnectionConfig.SSLMode.
const (
	SSLModeDisable    = "disable"
	SSLModePrefer     = "prefer"
	SSLModeRequire    = "require"
	SSLModeVerifyFull = "verify-full"
)

// ConnectionConfig holds connection parameters for PostgreSQL
type ConnectionConfig struct {
	Host     string
	Port     int32
	User     string
	Password string
	Database string
	// SSLMode is a libpq sslmode. Defaults to "disable".
	SSLMode string
	// RootCertPEM is the PEM-encoded CA bundle used to verify the server
	// certificate. It is required when SSLMode is "verify-full" (there is no
	// fallback to the system trust store) and ignored otherwise.
	RootCertPEM []byte
	// DialAddress, when set, is the host:port the TCP connection is made to
	// instead of Host:Port. Host is still used for everything else, in
	// particular to verify the server certificate, so a single pod can be
	// reached by its IP while verify-full checks the Service DNS name.
	DialAddress string
}

// fixedAddressDialer dials one fixed address, whatever address lib/pq asks
// for.
type fixedAddressDialer struct {
	address string
	d       net.Dialer
}

func (f fixedAddressDialer) Dial(network, _ string) (net.Conn, error) {
	return f.d.Dial(network, f.address)
}

func (f fixedAddressDialer) DialTimeout(network, _ string, timeout time.Duration) (net.Conn, error) {
	d := f.d
	d.Timeout = timeout
	return d.Dial(network, f.address)
}

func (f fixedAddressDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return f.d.DialContext(ctx, network, f.address)
}

// NewClient creates a new PostgreSQL client connection
func NewClient(cfg ConnectionConfig) (*Client, error) {
	connStr, err := buildDSN(cfg)
	if err != nil {
		return nil, err
	}

	var db *sql.DB
	if cfg.DialAddress != "" {
		connector, err := pq.NewConnector(connStr)
		if err != nil {
			return nil, fmt.Errorf("failed to open connection: %w", err)
		}
		connector.Dialer(fixedAddressDialer{address: cfg.DialAddress, d: net.Dialer{Timeout: connectTimeout}})
		db = sql.OpenDB(connector)
	} else if db, err = sql.Open("postgres", connStr); err != nil {
		return nil, fmt.Errorf("failed to open connection: %w", err)
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &Client{db: db}, nil
}

// Close closes the database connection
func (c *Client) Close() error {
	return c.db.Close()
}

// ReloadConfig asks the server to re-read its configuration files
// (pg_reload_conf()). PostgreSQL also reloads its TLS certificate, key and CA
// files on reload, so this rotates certificates without a restart.
func (c *Client) ReloadConfig(ctx context.Context) error {
	var ok bool
	if err := c.db.QueryRowContext(ctx, "SELECT pg_reload_conf()").Scan(&ok); err != nil {
		return fmt.Errorf("failed to reload configuration: %w", err)
	}
	if !ok {
		return fmt.Errorf("pg_reload_conf() returned false")
	}
	return nil
}

// FileSetting is a row of pg_file_settings: an entry of a configuration file
// as the server would read it now. Error is empty when the entry is valid.
type FileSetting struct {
	SourceFile string
	Name       string
	Setting    string
	Error      string
}

// Setting is the part of a pg_settings row needed to follow configuration
// changes.
type Setting struct {
	Name           string
	Context        string
	Source         string
	SourceFile     string
	PendingRestart bool
}

// ConfigFile returns the path of the main configuration file the server was
// started with (SHOW config_file).
func (c *Client) ConfigFile(ctx context.Context) (string, error) {
	var path string
	if err := c.db.QueryRowContext(ctx, "SHOW config_file").Scan(&path); err != nil {
		return "", fmt.Errorf("failed to read config_file: %w", err)
	}
	return path, nil
}

// FileSettings returns the contents of pg_file_settings: what the server
// would load from its configuration files on a reload. Requires a superuser
// (or pg_read_all_settings).
func (c *Client) FileSettings(ctx context.Context) ([]FileSetting, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT sourcefile, name, setting, error FROM pg_file_settings ORDER BY sourcefile, sourceline")
	if err != nil {
		return nil, fmt.Errorf("failed to read pg_file_settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FileSetting
	for rows.Next() {
		var file, name, setting, errMsg sql.NullString
		if err := rows.Scan(&file, &name, &setting, &errMsg); err != nil {
			return nil, fmt.Errorf("failed to scan pg_file_settings: %w", err)
		}
		out = append(out, FileSetting{SourceFile: file.String, Name: name.String, Setting: setting.String, Error: errMsg.String})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read pg_file_settings: %w", err)
	}
	return out, nil
}

// Settings returns pg_settings as seen by this session.
func (c *Client) Settings(ctx context.Context) ([]Setting, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT name, context, source, sourcefile, pending_restart FROM pg_settings ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("failed to read pg_settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Setting
	for rows.Next() {
		var s Setting
		var source, file sql.NullString
		if err := rows.Scan(&s.Name, &s.Context, &source, &file, &s.PendingRestart); err != nil {
			return nil, fmt.Errorf("failed to scan pg_settings: %w", err)
		}
		s.Source, s.SourceFile = source.String, file.String
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read pg_settings: %w", err)
	}
	return out, nil
}

// RoleOptions defines PostgreSQL role attributes
type RoleOptions struct {
	Login           bool
	Superuser       bool
	CreateDB        bool
	CreateRole      bool
	Inherit         bool
	Replication     bool
	BypassRLS       bool
	ConnectionLimit int32
	// Password is the role's plaintext password. CreateRole never sends it to
	// the server: it sends a SCRAM-SHA-256 verifier computed from it.
	Password string
	// KeepExistingPassword omits PASSWORD from ALTER ROLE when the role
	// already exists, so an unchanged password is not re-sent (it would
	// otherwise show up in server logs with log_statement=ddl). A newly
	// created role always gets Password.
	KeepExistingPassword bool
}

// CreateRole creates a new PostgreSQL role with the given options, or updates
// an existing one. The password is sent as a client-side computed
// SCRAM-SHA-256 verifier, so the plaintext never reaches the server or its
// logs. Errors are redacted: they never contain the password or verifier.
func (c *Client) CreateRole(ctx context.Context, name string, opts RoleOptions) error {
	// Check if role exists
	var exists bool
	err := c.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)", name).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check role existence: %w", err)
	}

	if exists && opts.KeepExistingPassword {
		opts.Password = ""
	}
	plaintext := opts.Password
	if opts.Password != "" {
		if opts.Password, err = ScramSHA256Verifier(opts.Password); err != nil {
			return err
		}
	}

	if _, err = c.db.ExecContext(ctx, c.buildRoleQuery(name, exists, opts)); err != nil {
		return redactedError(fmt.Sprintf("failed to create/alter role %q", name), err, plaintext, opts.Password)
	}

	return nil
}

// redactedError returns an error for err, prefixed with msg, that does not
// wrap err and has every occurrence of the given secrets (and their quoted
// SQL forms) replaced, so a password can never end up in a condition message
// or log line. For a server error only its message and SQLSTATE are kept (not
// its detail, hint or query).
func redactedError(msg string, err error, secrets ...string) error {
	text := err.Error()
	if pqErr, ok := errors.AsType[*pq.Error](err); ok {
		text = fmt.Sprintf("%s (SQLSTATE %s)", pqErr.Message, pqErr.Code)
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		for _, form := range []string{quoteLiteral(s), escapeString(s), s} {
			text = strings.ReplaceAll(text, form, "[REDACTED]")
		}
	}
	return fmt.Errorf("%s: %s", msg, text)
}

// buildRoleQuery returns the CREATE ROLE (role absent) or ALTER ROLE (role
// present) statement for opts. opts.Password is emitted as given (CreateRole
// passes a SCRAM verifier).
func (c *Client) buildRoleQuery(name string, exists bool, opts RoleOptions) string {
	if !exists {
		return c.buildCreateRoleQuery(name, opts)
	}
	if opts.KeepExistingPassword {
		opts.Password = ""
	}
	return c.buildAlterRoleQuery(name, opts)
}

func (c *Client) buildCreateRoleQuery(name string, opts RoleOptions) string {
	roleOpts := c.buildRoleOptions(opts)
	parts := make([]string, 0, 1+len(roleOpts))
	parts = append(parts, fmt.Sprintf("CREATE ROLE %s", quoteIdent(name)))
	parts = append(parts, roleOpts...)
	return strings.Join(parts, " ")
}

func (c *Client) buildAlterRoleQuery(name string, opts RoleOptions) string {
	roleOpts := c.buildRoleOptions(opts)
	parts := make([]string, 0, 1+len(roleOpts))
	parts = append(parts, fmt.Sprintf("ALTER ROLE %s", quoteIdent(name)))
	parts = append(parts, roleOpts...)
	return strings.Join(parts, " ")
}

func (c *Client) buildRoleOptions(opts RoleOptions) []string {
	var parts []string

	if opts.Login {
		parts = append(parts, "LOGIN")
	} else {
		parts = append(parts, "NOLOGIN")
	}

	if opts.Superuser {
		parts = append(parts, "SUPERUSER")
	} else {
		parts = append(parts, "NOSUPERUSER")
	}

	if opts.CreateDB {
		parts = append(parts, "CREATEDB")
	} else {
		parts = append(parts, "NOCREATEDB")
	}

	if opts.CreateRole {
		parts = append(parts, "CREATEROLE")
	} else {
		parts = append(parts, "NOCREATEROLE")
	}

	if opts.Inherit {
		parts = append(parts, "INHERIT")
	} else {
		parts = append(parts, "NOINHERIT")
	}

	if opts.Replication {
		parts = append(parts, "REPLICATION")
	} else {
		parts = append(parts, "NOREPLICATION")
	}

	if opts.BypassRLS {
		parts = append(parts, "BYPASSRLS")
	} else {
		parts = append(parts, "NOBYPASSRLS")
	}

	parts = append(parts, fmt.Sprintf("CONNECTION LIMIT %d", opts.ConnectionLimit))

	if opts.Password != "" {
		parts = append(parts, "PASSWORD "+quoteLiteral(opts.Password))
	}

	return parts
}

// DropRole drops a PostgreSQL role
func (c *Client) DropRole(ctx context.Context, name string) error {
	query := fmt.Sprintf("DROP ROLE IF EXISTS %s", quoteIdent(name))
	_, err := c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to drop role: %w", err)
	}
	return nil
}

// RoleExists checks if a role exists
func (c *Client) RoleExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := c.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)", name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check role existence: %w", err)
	}
	return exists, nil
}

// MinMembershipOptionsVersion is the first server_version_num that supports
// the INHERIT and SET grant options (PostgreSQL 16).
const MinMembershipOptionsVersion = 160000

// bootstrapSuperuserOID is the OID of the bootstrap superuser. On PostgreSQL
// 16+, grants made by any superuser are recorded with it as the grantor.
const bootstrapSuperuserOID = 10

// MembershipOptions are the options of a role membership grant. Nil
// Inherit/Set are not emitted, so PostgreSQL's default applies to a new grant
// and an existing grant keeps its current value.
type MembershipOptions struct {
	Admin   bool
	Inherit *bool
	Set     *bool
}

// MembershipState is the current state of a membership grant. Inherit and Set
// are nil on servers older than PostgreSQL 16.
type MembershipState struct {
	Admin   bool
	Inherit *bool
	Set     *bool
}

// ServerVersionNum returns the server's server_version_num (e.g. 180001).
func (c *Client) ServerVersionNum(ctx context.Context) (int, error) {
	var v string
	if err := c.db.QueryRowContext(ctx, "SHOW server_version_num").Scan(&v); err != nil {
		return 0, fmt.Errorf("failed to read server version: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("failed to parse server_version_num %q: %w", v, err)
	}
	return n, nil
}

// buildGrantRoleQuery builds GRANT role TO member, adding a WITH clause for
// ADMIN (only when true; it is removed with REVOKE ADMIN OPTION FOR) and for
// the INHERIT/SET options that are set.
func buildGrantRoleQuery(role, member string, opts MembershipOptions) string {
	query := fmt.Sprintf("GRANT %s TO %s", quoteIdent(role), quoteIdent(member))
	var with []string
	if opts.Admin {
		with = append(with, "ADMIN OPTION")
	}
	if opts.Inherit != nil {
		with = append(with, "INHERIT "+strings.ToUpper(strconv.FormatBool(*opts.Inherit)))
	}
	if opts.Set != nil {
		with = append(with, "SET "+strings.ToUpper(strconv.FormatBool(*opts.Set)))
	}
	if len(with) > 0 {
		query += " WITH " + strings.Join(with, ", ")
	}
	return query
}

// GrantRole grants membership in a role to another role. On an existing grant,
// PostgreSQL 16+ updates the options given in opts.
func (c *Client) GrantRole(ctx context.Context, role, member string, opts MembershipOptions) error {
	if _, err := c.db.ExecContext(ctx, buildGrantRoleQuery(role, member, opts)); err != nil {
		return fmt.Errorf("failed to grant role %q to %q: %w", role, member, err)
	}
	return nil
}

// RevokeAdminOption removes the ADMIN option from member's membership in role.
func (c *Client) RevokeAdminOption(ctx context.Context, role, member string) error {
	query := fmt.Sprintf("REVOKE ADMIN OPTION FOR %s FROM %s", quoteIdent(role), quoteIdent(member))
	if _, err := c.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("failed to revoke admin option for role %q from %q: %w", role, member, err)
	}
	return nil
}

// RevokeRole revokes membership in a role from another role
func (c *Client) RevokeRole(ctx context.Context, role, member string) error {
	query := fmt.Sprintf("REVOKE %s FROM %s", quoteIdent(role), quoteIdent(member))
	_, err := c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to revoke role %q from %q: %w", role, member, err)
	}
	return nil
}

// ListMemberships returns the roles member belongs to, keyed by role name.
// serverVersion selects the query: on PostgreSQL 16+ only grants recorded with
// the bootstrap superuser (or the current user) as grantor are returned, since
// those are the grants that GRANT/REVOKE issued by pgop operate on.
func (c *Client) ListMemberships(ctx context.Context, member string, serverVersion int) (map[string]MembershipState, error) {
	var query string
	if serverVersion >= MinMembershipOptionsVersion {
		query = fmt.Sprintf(`SELECT r.rolname, m.admin_option, m.inherit_option, m.set_option
FROM pg_auth_members m
JOIN pg_roles r ON r.oid = m.roleid
JOIN pg_roles u ON u.oid = m.member
WHERE u.rolname = $1
  AND m.grantor IN (%d, (SELECT oid FROM pg_roles WHERE rolname = current_user))`, bootstrapSuperuserOID)
	} else {
		query = `SELECT r.rolname, m.admin_option, NULL::boolean, NULL::boolean
FROM pg_auth_members m
JOIN pg_roles r ON r.oid = m.roleid
JOIN pg_roles u ON u.oid = m.member
WHERE u.rolname = $1`
	}
	rows, err := c.db.QueryContext(ctx, query, member)
	if err != nil {
		return nil, fmt.Errorf("failed to list memberships of %q: %w", member, err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]MembershipState{}
	for rows.Next() {
		var name string
		var admin bool
		var inherit, set sql.NullBool
		if err := rows.Scan(&name, &admin, &inherit, &set); err != nil {
			return nil, fmt.Errorf("failed to scan membership: %w", err)
		}
		st := MembershipState{Admin: admin}
		if inherit.Valid {
			st.Inherit = &inherit.Bool
		}
		if set.Valid {
			st.Set = &set.Bool
		}
		out[name] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to list memberships of %q: %w", member, err)
	}
	return out, nil
}

// CreateDatabase creates a new database
func (c *Client) CreateDatabase(ctx context.Context, name, owner string) error {
	// Check if database exists
	var exists bool
	err := c.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check database existence: %w", err)
	}

	if exists {
		// Update owner if needed
		if owner != "" {
			query := fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", quoteIdent(name), quoteIdent(owner))
			_, err = c.db.ExecContext(ctx, query)
			if err != nil {
				return fmt.Errorf("failed to alter database owner: %w", err)
			}
		}
		return nil
	}

	query := fmt.Sprintf("CREATE DATABASE %s", quoteIdent(name))
	if owner != "" {
		query += fmt.Sprintf(" OWNER %s", quoteIdent(owner))
	}

	_, err = c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to create database: %w", err)
	}

	return nil
}

// DropDatabase drops a database
func (c *Client) DropDatabase(ctx context.Context, name string) error {
	query := fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdent(name))
	_, err := c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to drop database: %w", err)
	}
	return nil
}

// DatabaseExists checks if a database exists
func (c *Client) DatabaseExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := c.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check database existence: %w", err)
	}
	return exists, nil
}

// CreateExtension creates a PostgreSQL extension in a database
// Note: This must be called on a connection to the target database
func (c *Client) CreateExtension(ctx context.Context, name, schema, version string) error {
	query := fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s", quoteIdent(name))
	if schema != "" {
		query += fmt.Sprintf(" SCHEMA %s", quoteIdent(schema))
	}
	if version != "" {
		query += " VERSION " + quoteLiteral(version)
	}

	_, err := c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to create extension: %w", err)
	}
	return nil
}

// CreateSchema creates a schema in the current database
func (c *Client) CreateSchema(ctx context.Context, name, owner string) error {
	// Check if schema exists
	var exists bool
	err := c.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)", name).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check schema existence: %w", err)
	}

	if exists {
		if owner != "" {
			query := fmt.Sprintf("ALTER SCHEMA %s OWNER TO %s", quoteIdent(name), quoteIdent(owner))
			_, err = c.db.ExecContext(ctx, query)
			if err != nil {
				return fmt.Errorf("failed to alter schema owner: %w", err)
			}
		}
		return nil
	}

	query := fmt.Sprintf("CREATE SCHEMA %s", quoteIdent(name))
	if owner != "" {
		query += fmt.Sprintf(" AUTHORIZATION %s", quoteIdent(owner))
	}

	_, err = c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to create schema: %w", err)
	}
	return nil
}

// GrantSchemaPrivileges grants privileges on a schema to a role
func (c *Client) GrantSchemaPrivileges(ctx context.Context, schema, role string, privileges []string, withGrantOption bool) error {
	privs := strings.Join(privileges, ", ")
	query := fmt.Sprintf("GRANT %s ON SCHEMA %s TO %s", privs, quoteIdent(schema), quoteIdent(role))
	if withGrantOption {
		query += " WITH GRANT OPTION"
	}

	_, err := c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to grant schema privileges: %w", err)
	}
	return nil
}

// quoteIdent quotes an identifier (table name, role name, etc.)
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral quotes s as a SQL string literal that is correct whatever
// standard_conforming_strings is set to: single quotes are doubled and, when
// s contains a backslash, backslashes are doubled and the literal is written
// as an escape string (E'...').
func quoteLiteral(s string) string {
	s = strings.ReplaceAll(s, `'`, `''`)
	if strings.Contains(s, `\`) {
		return `E'` + strings.ReplaceAll(s, `\`, `\\`) + `'`
	}
	return `'` + s + `'`
}

// escapeString doubles single quotes. It is not safe for building SQL (use
// quoteLiteral); it is only used to redact secrets from error text.
func escapeString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
