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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lib/pq"
)

const (
	attrLogin         = "LOGIN"
	attrNoSuperuser   = "NOSUPERUSER"
	attrNoCreateDB    = "NOCREATEDB"
	attrNoCreateRole  = "NOCREATEROLE"
	attrNoReplication = "NOREPLICATION"
	attrNoBypassRLS   = "NOBYPASSRLS"
	attrConnLimitNeg1 = "CONNECTION LIMIT -1"
	attrNoInherit     = "NOINHERIT"
	testMember        = "app"
	testParent        = "parent"
	testPGUser        = "postgres"
	testPGPassword    = "secret"
	testPGSSLMode     = "disable"
)

func TestQuoteIdent(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple identifier",
			input:    "my_table",
			expected: `"my_table"`,
		},
		{
			name:     "identifier with spaces",
			input:    "my table",
			expected: `"my table"`,
		},
		{
			name:     "identifier with double quotes",
			input:    `my"table`,
			expected: `"my""table"`,
		},
		{
			name:     "empty string",
			input:    "",
			expected: `""`,
		},
		{
			name:     "identifier with special chars",
			input:    "user-name",
			expected: `"user-name"`,
		},
		{
			name:     "uppercase identifier",
			input:    "MyTable",
			expected: `"MyTable"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := quoteIdent(tt.input)
			if result != tt.expected {
				t.Errorf("quoteIdent(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestQuoteLiteral(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple string", "hello", `'hello'`},
		{"string with single quote", "don't", `'don''t'`},
		{"string with multiple quotes", "it's a 'test'", `'it''s a ''test'''`},
		{"empty string", "", `''`},
		{"password with special chars", "p@ss'w0rd!", `'p@ss''w0rd!'`},
		// A backslash makes the literal an escape string, so it means the
		// same whatever standard_conforming_strings is set to.
		{"backslash", `a\b`, `E'a\\b'`},
		{"trailing backslash", `abc\`, `E'abc\\'`},
		{"backslash before a quote", `a\'; DROP ROLE x; --`, `E'a\\''; DROP ROLE x; --'`},
		{"double quotes are not special", `a"b`, `'a"b'`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := quoteLiteral(tt.input)
			if result != tt.expected {
				t.Errorf("quoteLiteral(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// unquoteLiteral parses a literal produced by quoteLiteral the way
// PostgreSQL does, for both settings of standard_conforming_strings when the
// literal is a plain '...' string, so the round trip proves the quoting.
func unquoteLiteral(t *testing.T, lit string, standardConformingStrings bool) string {
	t.Helper()
	escape := strings.HasPrefix(lit, "E'")
	if escape {
		lit = lit[1:]
	}
	if len(lit) < 2 || lit[0] != '\'' || lit[len(lit)-1] != '\'' {
		t.Fatalf("not a quoted literal: %q", lit)
	}
	body := lit[1 : len(lit)-1]
	backslashEscapes := escape || !standardConformingStrings
	var out strings.Builder
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\'':
			if i+1 >= len(body) || body[i+1] != '\'' {
				t.Fatalf("unescaped quote ends the literal early in %q", lit)
			}
			out.WriteByte('\'')
			i++
		case c == '\\' && backslashEscapes:
			if i+1 >= len(body) {
				t.Fatalf("dangling backslash in %q", lit)
			}
			out.WriteByte(body[i+1])
			i++
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

func TestQuoteLiteralRoundTrip(t *testing.T) {
	for _, in := range []string{"", "plain", "it's", `a\b`, `\'`, `'\`, `\\'' --`, `x\'); DROP ROLE postgres; --`} {
		for _, scs := range []bool{true, false} {
			if got := unquoteLiteral(t, quoteLiteral(in), scs); got != in {
				t.Errorf("quoteLiteral(%q) parses back as %q (standard_conforming_strings=%v)", in, got, scs)
			}
		}
	}
}

func TestRedactedError(t *testing.T) {
	const pw = `s3cr'et\pw`
	err := redactedError("failed to create/alter role \"app\"",
		fmt.Errorf("syntax error at or near PASSWORD %s and %s and %s", quoteLiteral(pw), escapeString(pw), pw), pw)
	if strings.Contains(err.Error(), "s3cr") {
		t.Errorf("redactedError leaked the password: %q", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") || !strings.HasPrefix(err.Error(), "failed to create/alter role \"app\": ") {
		t.Errorf("unexpected redacted error: %q", err)
	}

	pqErr := &pq.Error{Code: "22023", Message: "invalid connection limit: -999",
		Detail: "PASSWORD '" + pw + "'", Hint: pw, InternalQuery: pw}
	err = redactedError("failed", fmt.Errorf("wrapped: %w", pqErr), pw)
	if err.Error() != "failed: invalid connection limit: -999 (SQLSTATE 22023)" {
		t.Errorf("unexpected error for a server error: %q", err)
	}
	if _, ok := errors.AsType[*pq.Error](err); ok {
		t.Error("redactedError must not wrap the original error")
	}
}

func TestBuildRoleOptions(t *testing.T) {
	client := &Client{}

	tests := []struct {
		name     string
		opts     RoleOptions
		expected []string
	}{
		{
			name: "default login role",
			opts: RoleOptions{
				Login:           true,
				Inherit:         true,
				ConnectionLimit: -1,
			},
			expected: []string{
				attrLogin,
				attrNoSuperuser,
				attrNoCreateDB,
				attrNoCreateRole,
				"INHERIT",
				attrNoReplication,
				attrNoBypassRLS,
				attrConnLimitNeg1,
			},
		},
		{
			name: "privileged role is still never a superuser",
			opts: RoleOptions{
				Login:           true,
				CreateDB:        true,
				CreateRole:      true,
				Inherit:         true,
				Replication:     true,
				BypassRLS:       true,
				ConnectionLimit: 100,
			},
			expected: []string{
				attrLogin,
				attrNoSuperuser,
				"CREATEDB",
				"CREATEROLE",
				"INHERIT",
				"REPLICATION",
				"BYPASSRLS",
				"CONNECTION LIMIT 100",
			},
		},
		{
			name: "nologin role",
			opts: RoleOptions{
				Login:           false,
				ConnectionLimit: 0,
			},
			expected: []string{
				"NOLOGIN",
				attrNoSuperuser,
				attrNoCreateDB,
				attrNoCreateRole,
				attrNoInherit,
				attrNoReplication,
				attrNoBypassRLS,
				"CONNECTION LIMIT 0",
			},
		},
		{
			name: "role with password",
			opts: RoleOptions{
				Login:           true,
				ConnectionLimit: -1,
				Password:        "secret123",
			},
			expected: []string{
				attrLogin,
				attrNoSuperuser,
				attrNoCreateDB,
				attrNoCreateRole,
				attrNoInherit,
				attrNoReplication,
				attrNoBypassRLS,
				attrConnLimitNeg1,
				"PASSWORD 'secret123'",
			},
		},
		{
			name: "password with special chars",
			opts: RoleOptions{
				Login:           true,
				ConnectionLimit: -1,
				Password:        "pass'word",
			},
			expected: []string{
				attrLogin,
				attrNoSuperuser,
				attrNoCreateDB,
				attrNoCreateRole,
				attrNoInherit,
				attrNoReplication,
				attrNoBypassRLS,
				attrConnLimitNeg1,
				"PASSWORD 'pass''word'",
			},
		},
		{
			name: "password with backslash and quote",
			opts: RoleOptions{
				Login:           true,
				ConnectionLimit: -1,
				Password:        `pa\ss'word`,
			},
			expected: []string{
				attrLogin,
				attrNoSuperuser,
				attrNoCreateDB,
				attrNoCreateRole,
				attrNoInherit,
				attrNoReplication,
				attrNoBypassRLS,
				attrConnLimitNeg1,
				`PASSWORD E'pa\\ss''word'`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := client.buildRoleOptions(tt.opts)
			if len(result) != len(tt.expected) {
				t.Errorf("buildRoleOptions() returned %d items, want %d", len(result), len(tt.expected))
				t.Logf("Got: %v", result)
				t.Logf("Want: %v", tt.expected)
				return
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("buildRoleOptions()[%d] = %q, want %q", i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestBuildCreateRoleQuery(t *testing.T) {
	client := &Client{}

	tests := []struct {
		name     string
		roleName string
		opts     RoleOptions
		contains []string
	}{
		{
			name:     "simple role",
			roleName: "app_user",
			opts: RoleOptions{
				Login:           true,
				ConnectionLimit: -1,
			},
			contains: []string{
				`CREATE ROLE "app_user"`,
				"LOGIN",
				"CONNECTION LIMIT -1",
			},
		},
		{
			name:     "role with special name",
			roleName: "my-user",
			opts: RoleOptions{
				Login:           true,
				ConnectionLimit: 10,
			},
			contains: []string{
				`CREATE ROLE "my-user"`,
				"LOGIN",
				"CONNECTION LIMIT 10",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := client.buildCreateRoleQuery(tt.roleName, tt.opts)
			for _, c := range tt.contains {
				if !containsString(result, c) {
					t.Errorf("buildCreateRoleQuery() = %q, should contain %q", result, c)
				}
			}
		})
	}
}

func TestBuildAlterRoleQuery(t *testing.T) {
	client := &Client{}

	tests := []struct {
		name     string
		roleName string
		opts     RoleOptions
		contains []string
	}{
		{
			name:     "alter role",
			roleName: "app_user",
			opts: RoleOptions{
				Login:           false,
				ConnectionLimit: 50,
			},
			contains: []string{
				`ALTER ROLE "app_user"`,
				"NOLOGIN",
				attrNoSuperuser,
				"CONNECTION LIMIT 50",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := client.buildAlterRoleQuery(tt.roleName, tt.opts)
			for _, c := range tt.contains {
				if !containsString(result, c) {
					t.Errorf("buildAlterRoleQuery() = %q, should contain %q", result, c)
				}
			}
		})
	}
}

func TestBuildRoleQueryPassword(t *testing.T) {
	client := &Client{}
	opts := RoleOptions{Login: true, Password: "s3cret", KeepExistingPassword: true}

	tests := []struct {
		name         string
		exists       bool
		opts         RoleOptions
		wantPrefix   string
		wantPassword bool
	}{
		{"new role always gets the password", false, opts, `CREATE ROLE "app"`, true},
		{"existing role keeps an unchanged password", true, opts, `ALTER ROLE "app"`, false},
		{"existing role gets a changed password", true,
			RoleOptions{Login: true, Password: "s3cret"}, `ALTER ROLE "app"`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := client.buildRoleQuery(testMember, tt.exists, tt.opts)
			if !strings.HasPrefix(q, tt.wantPrefix) {
				t.Errorf("buildRoleQuery() = %q, want prefix %q", q, tt.wantPrefix)
			}
			if got := strings.Contains(q, "PASSWORD"); got != tt.wantPassword {
				t.Errorf("buildRoleQuery() = %q, contains PASSWORD = %v, want %v", q, got, tt.wantPassword)
			}
		})
	}
}

func TestConnectionConfig(t *testing.T) {
	// Test that ConnectionConfig struct can be created
	cfg := ConnectionConfig{
		Host:     "localhost",
		Port:     5432,
		User:     testPGUser,
		Password: testPGPassword,
		Database: "testdb",
		SSLMode:  testPGSSLMode,
	}

	if cfg.Host != "localhost" {
		t.Errorf("Host = %q, want %q", cfg.Host, "localhost")
	}
	if cfg.Port != 5432 {
		t.Errorf("Port = %d, want %d", cfg.Port, 5432)
	}
	if cfg.User != testPGUser {
		t.Errorf("User = %q, want %q", cfg.User, testPGUser)
	}
	if cfg.Database != "testdb" {
		t.Errorf("Database = %q, want %q", cfg.Database, "testdb")
	}
	if cfg.SSLMode != testPGSSLMode {
		t.Errorf("SSLMode = %q, want %q", cfg.SSLMode, testPGSSLMode)
	}
}

func TestRoleOptions(t *testing.T) {
	// Test that RoleOptions struct can be created
	opts := RoleOptions{
		Login:           true,
		CreateDB:        true,
		CreateRole:      false,
		Inherit:         true,
		Replication:     false,
		BypassRLS:       false,
		ConnectionLimit: 10,
		Password:        testPGPassword,
	}

	if !opts.Login {
		t.Error("Login should be true")
	}
	if !opts.CreateDB {
		t.Error("CreateDB should be true")
	}
	if opts.ConnectionLimit != 10 {
		t.Errorf("ConnectionLimit = %d, want %d", opts.ConnectionLimit, 10)
	}
	if opts.Password != testPGPassword {
		t.Errorf("Password = %q, want %q", opts.Password, testPGPassword)
	}
}

func TestBuildGrantRoleQuery(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		member   string
		opts     MembershipOptions
		expected string
	}{
		{"no options", testParent, testMember, MembershipOptions{}, `GRANT "parent" TO "app"`},
		{"admin true", testParent, testMember, MembershipOptions{Admin: true}, `GRANT "parent" TO "app" WITH ADMIN OPTION`},
		{"inherit true", testParent, testMember, MembershipOptions{Inherit: new(true)}, `GRANT "parent" TO "app" WITH INHERIT TRUE`},
		{"inherit false", testParent, testMember, MembershipOptions{Inherit: new(false)}, `GRANT "parent" TO "app" WITH INHERIT FALSE`},
		{"set true", testParent, testMember, MembershipOptions{Set: new(true)}, `GRANT "parent" TO "app" WITH SET TRUE`},
		{"set false", testParent, testMember, MembershipOptions{Set: new(false)}, `GRANT "parent" TO "app" WITH SET FALSE`},
		{
			"all combined", testParent, testMember,
			MembershipOptions{Admin: true, Inherit: new(false), Set: new(true)},
			`GRANT "parent" TO "app" WITH ADMIN OPTION, INHERIT FALSE, SET TRUE`,
		},
		{"identifier quoting", `we"ird`, "my-app", MembershipOptions{}, `GRANT "we""ird" TO "my-app"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildGrantRoleQuery(tt.role, tt.member, tt.opts); got != tt.expected {
				t.Errorf("buildGrantRoleQuery() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// Helper function
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestBuildCreateExtensionQuery(t *testing.T) {
	tests := []struct{ name, schema, version, want string }{
		{"pg_trgm", "", "", `CREATE EXTENSION IF NOT EXISTS "pg_trgm"`},
		{"uuid-ossp", "ext", "1.1", `CREATE EXTENSION IF NOT EXISTS "uuid-ossp" SCHEMA "ext" VERSION '1.1'`},
		{`x"y`, `s"`, `1'0`, `CREATE EXTENSION IF NOT EXISTS "x""y" SCHEMA "s""" VERSION '1''0'`},
	}
	for _, tt := range tests {
		if got := buildCreateExtensionQuery(tt.name, tt.schema, tt.version); got != tt.want {
			t.Errorf("buildCreateExtensionQuery(%q, %q, %q) = %q, want %q", tt.name, tt.schema, tt.version, got, tt.want)
		}
	}
}
