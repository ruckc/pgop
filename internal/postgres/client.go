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
	"fmt"
	"strings"

	_ "github.com/lib/pq"
)

// Client provides PostgreSQL database operations
type Client struct {
	db *sql.DB
}

// ConnectionConfig holds connection parameters for PostgreSQL
type ConnectionConfig struct {
	Host     string
	Port     int32
	User     string
	Password string
	Database string
	SSLMode  string
}

// NewClient creates a new PostgreSQL client connection
func NewClient(cfg ConnectionConfig) (*Client, error) {
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}
	if cfg.Database == "" {
		cfg.Database = "postgres"
	}

	connStr := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database, cfg.SSLMode,
	)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
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
	Password        string
}

// CreateRole creates a new PostgreSQL role with the given options
func (c *Client) CreateRole(ctx context.Context, name string, opts RoleOptions) error {
	// Check if role exists
	var exists bool
	err := c.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)", name).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check role existence: %w", err)
	}

	var query string
	if exists {
		query = c.buildAlterRoleQuery(name, opts)
	} else {
		query = c.buildCreateRoleQuery(name, opts)
	}

	_, err = c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to create/alter role: %w", err)
	}

	return nil
}

func (c *Client) buildCreateRoleQuery(name string, opts RoleOptions) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("CREATE ROLE %s", quoteIdent(name)))

	parts = append(parts, c.buildRoleOptions(opts)...)

	return strings.Join(parts, " ")
}

func (c *Client) buildAlterRoleQuery(name string, opts RoleOptions) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("ALTER ROLE %s", quoteIdent(name)))

	parts = append(parts, c.buildRoleOptions(opts)...)

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
		parts = append(parts, fmt.Sprintf("PASSWORD '%s'", escapeString(opts.Password)))
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

// GrantRole grants membership in a role to another role
func (c *Client) GrantRole(ctx context.Context, role, member string) error {
	query := fmt.Sprintf("GRANT %s TO %s", quoteIdent(role), quoteIdent(member))
	_, err := c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to grant role: %w", err)
	}
	return nil
}

// RevokeRole revokes membership in a role from another role
func (c *Client) RevokeRole(ctx context.Context, role, member string) error {
	query := fmt.Sprintf("REVOKE %s FROM %s", quoteIdent(role), quoteIdent(member))
	_, err := c.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to revoke role: %w", err)
	}
	return nil
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
		query += fmt.Sprintf(" VERSION '%s'", escapeString(version))
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

// escapeString escapes a string for use in SQL
func escapeString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
