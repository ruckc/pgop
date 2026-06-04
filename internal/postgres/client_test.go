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
	"testing"
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

func TestEscapeString(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple string",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "string with single quote",
			input:    "it's",
			expected: "it''s",
		},
		{
			name:     "string with multiple quotes",
			input:    "it's a 'test'",
			expected: "it''s a ''test''",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "password with special chars",
			input:    "p@ss'w0rd!",
			expected: "p@ss''w0rd!",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := escapeString(tt.input)
			if result != tt.expected {
				t.Errorf("escapeString(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
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
			name: "superuser role",
			opts: RoleOptions{
				Login:           true,
				Superuser:       true,
				CreateDB:        true,
				CreateRole:      true,
				Inherit:         true,
				Replication:     true,
				BypassRLS:       true,
				ConnectionLimit: 100,
			},
			expected: []string{
				attrLogin,
				"SUPERUSER",
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
				Superuser:       true,
				ConnectionLimit: 50,
			},
			contains: []string{
				`ALTER ROLE "app_user"`,
				"NOLOGIN",
				"SUPERUSER",
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
		Superuser:       false,
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
	if opts.Superuser {
		t.Error("Superuser should be false")
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
