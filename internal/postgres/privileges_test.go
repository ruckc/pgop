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
	"slices"
	"strings"
	"testing"

	"github.com/lib/pq"
)

// injectionPrivileges are privilege values that try to smuggle SQL into a
// GRANT statement. Every builder must reject them.
var injectionPrivileges = []string{
	"USAGE; DROP DATABASE postgres; --",
	"CONNECT ON DATABASE postgres TO public; --",
	"SET ON PARAMETER x TO y; ALTER SYSTEM",
	"ALL PRIVILEGES; DROP DATABASE x",
	"ALTER SYSTEM",
	"SELECT",
	"",
	"USAGE,CREATE",
}

// testPublic is the lower-case spelling of PUBLIC (and the schema public).
const testPublic = "public"

func schemaGrantQuery(schema, role string, privs []string, wgo bool) (string, error) {
	return buildGrantPrivilegesQuery(PrivilegeObject{Kind: ObjectSchema, Name: schema}, role, privs, wgo)
}

func databaseGrantQuery(db, role string, privs []string, wgo bool) (string, error) {
	return buildGrantPrivilegesQuery(PrivilegeObject{Kind: ObjectDatabase, Name: db}, role, privs, wgo)
}

func databaseRevokeQuery(db, role string, privs []string, mode RevokeMode) (string, error) {
	return buildRevokePrivilegesQuery(PrivilegeObject{Kind: ObjectDatabase, Name: db}, role, privs, mode)
}

func parameterGrantQuery(param, role string, privs []string, wgo bool) (string, error) {
	return buildGrantPrivilegesQuery(PrivilegeObject{Kind: ObjectParameter, Name: param}, role, privs, wgo)
}

func parameterRevokeQuery(param, role string, privs []string, mode RevokeMode) (string, error) {
	return buildRevokePrivilegesQuery(PrivilegeObject{Kind: ObjectParameter, Name: param}, role, privs, mode)
}

func TestPublicGrantee(t *testing.T) {
	// Every spelling of public is the PUBLIC keyword, never a quoted
	// identifier: "PUBLIC" (quoted) would name an ordinary role.
	for _, g := range []string{"PUBLIC", testPublic, "Public", "pUbLiC"} {
		got, err := databaseGrantQuery("app_db", g, []string{PrivilegeConnect}, false)
		if err != nil {
			t.Fatalf("%s: %v", g, err)
		}
		if want := `GRANT CONNECT ON DATABASE "app_db" TO PUBLIC`; got != want {
			t.Errorf("%s: got %q, want %q", g, got, want)
		}
		got, err = buildRevokePrivilegesQuery(PrivilegeObject{Kind: ObjectSchema, Name: testPublic}, g,
			[]string{PrivilegeCreate, PrivilegeUsage}, RevokeMode{})
		if err != nil {
			t.Fatalf("%s: %v", g, err)
		}
		if want := `REVOKE CREATE, USAGE ON SCHEMA "public" FROM PUBLIC`; got != want {
			t.Errorf("%s: got %q, want %q", g, got, want)
		}
		if CanonicalGrantee(g) != PublicGrantee {
			t.Errorf("CanonicalGrantee(%q) = %q", g, CanonicalGrantee(g))
		}
	}
	// Names that merely contain public are ordinary, quoted roles.
	for _, g := range []string{"public_reader", " public", "public ", `"public"`, "publicx"} {
		got, err := databaseGrantQuery("d", g, []string{PrivilegeConnect}, false)
		if err != nil {
			t.Fatalf("%q: %v", g, err)
		}
		if want := `GRANT CONNECT ON DATABASE "d" TO ` + quoteIdent(g); got != want {
			t.Errorf("%q: got %q, want %q", g, got, want)
		}
		if IsPublic(g) {
			t.Errorf("IsPublic(%q) = true", g)
		}
	}
	if _, err := databaseGrantQuery("d", testPublic, []string{PrivilegeConnect}, true); err == nil {
		t.Error("granted the grant option to PUBLIC")
	}
	for _, g := range []string{"", "none", "NONE"} {
		if _, err := databaseGrantQuery("d", g, []string{PrivilegeConnect}, false); err == nil {
			t.Errorf("accepted grantee %q", g)
		}
	}
}

func TestGranteeAndObjectInjection(t *testing.T) {
	hostile := []string{`x" TO PUBLIC; DROP DATABASE postgres; --`, `PUBLIC; DROP ROLE x`, `public"`, "a\x00b"}
	for _, h := range hostile {
		got, err := databaseGrantQuery(h, h, []string{PrivilegeConnect}, false)
		if err != nil {
			t.Fatalf("%q: %v", h, err)
		}
		want := `GRANT CONNECT ON DATABASE ` + quoteIdent(h) + ` TO ` + quoteIdent(h)
		if got != want {
			t.Errorf("%q: got %q, want %q", h, got, want)
		}
	}
	if _, err := buildGrantPrivilegesQuery(PrivilegeObject{Kind: "TABLE", Name: "t"}, "r", []string{"SELECT"}, false); err == nil {
		t.Error("accepted an unsupported object kind")
	}
	if _, err := schemaGrantQuery("", "r", []string{PrivilegeUsage}, false); err == nil {
		t.Error("accepted an empty schema name")
	}
}

func TestNormalizeSchemaPrivileges(t *testing.T) {
	got, err := NormalizeSchemaPrivileges([]string{" Usage", privilegeAllPrivileges, "Create"})
	if err != nil || !slices.Equal(got, []string{PrivilegeCreate, PrivilegeUsage}) {
		t.Errorf("got %v, %v", got, err)
	}
	got, err = NormalizeSchemaPrivileges([]string{"USAGE "})
	if err != nil || !slices.Equal(got, []string{PrivilegeUsage}) {
		t.Errorf("got %v, %v", got, err)
	}
	for _, p := range injectionPrivileges {
		if _, err := NormalizeSchemaPrivileges([]string{p}); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
}

func TestBuildGrantSchemaPrivilegesQuery(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		role   string
		privs  []string
		wgo    bool
		want   string
	}{
		{"single", testMember, "reader", []string{PrivilegeUsage}, false, `GRANT USAGE ON SCHEMA "app" TO "reader"`},
		{"multiple with grant option", testMember, "writer", []string{PrivilegeUsage, PrivilegeCreate}, true,
			`GRANT USAGE, CREATE ON SCHEMA "app" TO "writer" WITH GRANT OPTION`},
		{"lowercase normalized, duplicates dropped", testMember, "r", []string{"usage", " Create ", PrivilegeUsage}, false,
			`GRANT USAGE, CREATE ON SCHEMA "app" TO "r"`},
		{"hostile identifiers are quoted", `a"; DROP SCHEMA x; --`, `r"; --`, []string{PrivilegeAll}, false,
			`GRANT ALL ON SCHEMA "a""; DROP SCHEMA x; --" TO "r""; --"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := schemaGrantQuery(tt.schema, tt.role, tt.privs, tt.wgo)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPrivilegeBuildersRejectInjection(t *testing.T) {
	for _, p := range injectionPrivileges {
		privs := []string{p}
		if _, err := schemaGrantQuery("s", "r", privs, false); err == nil {
			t.Errorf("schema grant accepted privilege %q", p)
		}
		if _, err := databaseGrantQuery("d", "r", privs, false); err == nil {
			t.Errorf("database grant accepted privilege %q", p)
		}
		if _, err := databaseRevokeQuery("d", "r", privs, RevokeMode{}); err == nil {
			t.Errorf("database revoke accepted privilege %q", p)
		}
		if _, err := parameterGrantQuery("work_mem", "r", privs, false); err == nil {
			t.Errorf("parameter grant accepted privilege %q", p)
		}
		if _, err := parameterRevokeQuery("work_mem", "r", privs, RevokeMode{}); err == nil {
			t.Errorf("parameter revoke accepted privilege %q", p)
		}
		// A valid privilege next to a hostile one must not slip through.
		if _, err := schemaGrantQuery("s", "r", []string{PrivilegeUsage, p}, false); err == nil {
			t.Errorf("schema grant accepted privilege list with %q", p)
		}
	}
	if _, err := schemaGrantQuery("s", "r", nil, false); err == nil {
		t.Error("schema grant accepted empty privileges")
	}
	// Privileges valid for one object type are not valid for another.
	if _, err := schemaGrantQuery("s", "r", []string{PrivilegeConnect}, false); err == nil {
		t.Error("schema grant accepted CONNECT")
	}
	if _, err := databaseGrantQuery("d", "r", []string{PrivilegeUsage}, false); err == nil {
		t.Error("database grant accepted USAGE")
	}
}

func TestBuildDatabasePrivilegeQueries(t *testing.T) {
	got, err := databaseGrantQuery("app_db", testMember, []string{PrivilegeConnect, "temp"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := `GRANT CONNECT, TEMP ON DATABASE "app_db" TO "app"`; got != want {
		t.Errorf("grant: got %q, want %q", got, want)
	}
	got, err = databaseGrantQuery(`d"b`, `r"x`, []string{PrivilegeAll}, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := `GRANT ALL ON DATABASE "d""b" TO "r""x" WITH GRANT OPTION`; got != want {
		t.Errorf("grant: got %q, want %q", got, want)
	}
	got, err = databaseRevokeQuery("app_db", testMember, []string{PrivilegeCreate, PrivilegeTemporary}, RevokeMode{Cascade: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `REVOKE CREATE, TEMPORARY ON DATABASE "app_db" FROM "app" CASCADE`; got != want {
		t.Errorf("revoke: got %q, want %q", got, want)
	}
	got, err = databaseRevokeQuery("app_db", testMember, []string{PrivilegeConnect}, RevokeMode{GrantOptionOnly: true, Cascade: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `REVOKE GRANT OPTION FOR CONNECT ON DATABASE "app_db" FROM "app" CASCADE`; got != want {
		t.Errorf("revoke grant option: got %q, want %q", got, want)
	}
}

func TestNormalizeDatabasePrivileges(t *testing.T) {
	tests := []struct {
		in   []string
		want []string
	}{
		{[]string{PrivilegeConnect}, []string{PrivilegeConnect}},
		{[]string{"temp", PrivilegeTemporary, "connect"}, []string{PrivilegeConnect, PrivilegeTemporary}},
		{[]string{PrivilegeAll}, []string{PrivilegeConnect, PrivilegeCreate, PrivilegeTemporary}},
		{[]string{PrivilegeCreate, PrivilegeAll, PrivilegeTemp}, []string{PrivilegeConnect, PrivilegeCreate, PrivilegeTemporary}},
	}
	for _, tt := range tests {
		got, err := NormalizeDatabasePrivileges(tt.in)
		if err != nil {
			t.Fatalf("%v: %v", tt.in, err)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%v: got %v, want %v", tt.in, got, tt.want)
		}
	}
	for _, bad := range [][]string{nil, {PrivilegeUsage}, {PrivilegeConnect, "DROP"}} {
		if _, err := NormalizeDatabasePrivileges(bad); err == nil {
			t.Errorf("%v: expected error", bad)
		}
	}
}

func TestNormalizeParameterPrivileges(t *testing.T) {
	got, err := NormalizeParameterPrivileges(nil)
	if err != nil || !slices.Equal(got, []string{PrivilegeSet}) {
		t.Errorf("nil: got %v, %v", got, err)
	}
	got, err = NormalizeParameterPrivileges([]string{"set", PrivilegeSet})
	if err != nil || !slices.Equal(got, []string{PrivilegeSet}) {
		t.Errorf("set: got %v, %v", got, err)
	}
	for _, bad := range []string{"ALTER SYSTEM", PrivilegeAll, "ALTER_SYSTEM"} {
		if _, err := NormalizeParameterPrivileges([]string{bad}); err == nil {
			t.Errorf("%q: expected error (ALTER SYSTEM is not offered)", bad)
		}
	}
}

func TestParameterNames(t *testing.T) {
	valid := map[string]string{
		"work_mem":              `"work_mem"`,
		"DateStyle":             `"datestyle"`,
		"myapp.tenant_id":       `"myapp"."tenant_id"`,
		"a.b.c":                 `"a"."b"."c"`,
		"myapp.select":          `"myapp"."select"`,
		"_x9":                   `"_x9"`,
		"plpgsql.check_asserts": `"plpgsql"."check_asserts"`,
	}
	for in, want := range valid {
		got, err := quoteParameterName(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %s, want %s", in, got, want)
		}
	}
	invalid := []string{
		"", "1abc", "work mem", `work_mem"`, "a..b", ".a", "a.", "x; DROP DATABASE y",
		"work_mem TO 1; --", "a-b", "a$b", strings.Repeat("a", 128),
	}
	for _, in := range invalid {
		if _, err := quoteParameterName(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
		if _, err := buildAlterDatabaseSetQuery("d", in, "1"); err == nil {
			t.Errorf("set %q: expected error", in)
		}
		if _, err := buildAlterDatabaseResetQuery("d", in); err == nil {
			t.Errorf("reset %q: expected error", in)
		}
		if _, err := parameterGrantQuery(in, "r", []string{PrivilegeSet}, false); err == nil {
			t.Errorf("grant on %q: expected error", in)
		}
	}
}

func TestBuildParameterQueries(t *testing.T) {
	got, err := parameterGrantQuery("log_statement", "auditor", []string{PrivilegeSet}, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := `GRANT SET ON PARAMETER "log_statement" TO "auditor"`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got, err = parameterGrantQuery("MyApp.Tenant", `a"b`, []string{"set"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := `GRANT SET ON PARAMETER "myapp"."tenant" TO "a""b" WITH GRANT OPTION`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got, err = parameterRevokeQuery("log_statement", "auditor", []string{PrivilegeSet}, RevokeMode{GrantOptionOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `REVOKE GRANT OPTION FOR SET ON PARAMETER "log_statement" FROM "auditor"`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got, err = parameterRevokeQuery("log_statement", "auditor", []string{PrivilegeSet}, RevokeMode{})
	if err != nil {
		t.Fatal(err)
	}
	if want := `REVOKE SET ON PARAMETER "log_statement" FROM "auditor"`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildAlterDatabaseSetQuery(t *testing.T) {
	tests := []struct {
		param, value, want string
	}{
		{"work_mem", "64MB", `ALTER DATABASE "db" SET "work_mem" TO '64MB'`},
		{settingStatementTimeout, "0", `ALTER DATABASE "db" SET "statement_timeout" TO '0'`},
		{"DateStyle", "ISO, MDY", `ALTER DATABASE "db" SET "datestyle" TO 'ISO, MDY'`},
		{"myapp.tenant", "x'; DROP DATABASE db; --", `ALTER DATABASE "db" SET "myapp"."tenant" TO 'x''; DROP DATABASE db; --'`},
		{"myapp.path", `C:\temp`, `ALTER DATABASE "db" SET "myapp"."path" TO E'C:\\temp'`},
		{"application_name", "", `ALTER DATABASE "db" SET "application_name" TO ''`},
		{paramSearchPath, `"$user", public`, `ALTER DATABASE "db" SET "search_path" TO '$user', 'public'`},
		{paramSearchPath, `App, "My Schema", "a""b"`, `ALTER DATABASE "db" SET "search_path" TO 'app', 'My Schema', 'a"b'`},
		{paramSearchPath, "public", `ALTER DATABASE "db" SET "search_path" TO 'public'`},
		{paramSearchPath, "", ""},
		{paramSearchPath, "  ", ""},
		{paramSearchPath, `x'); DROP DATABASE db; --`, ""},
		{settingTempTablespaces, `"a"x`, ""},
		{paramSearchPath, `a,,b`, ""},
		{paramSearchPath, `"unterminated`, ""},
	}
	for _, tt := range tests {
		got, err := buildAlterDatabaseSetQuery("db", tt.param, tt.value)
		if tt.want == "" {
			if err == nil {
				t.Errorf("%s=%q: expected error, got %q", tt.param, tt.value, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s=%q: %v", tt.param, tt.value, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s=%q: got %q, want %q", tt.param, tt.value, got, tt.want)
		}
	}

	got, err := buildAlterDatabaseResetQuery(`d"b`, "Work_Mem")
	if err != nil {
		t.Fatal(err)
	}
	if want := `ALTER DATABASE "d""b" RESET "work_mem"`; got != want {
		t.Errorf("reset: got %q, want %q", got, want)
	}
}

func TestAllPrivilegesAndCase(t *testing.T) {
	got, err := schemaGrantQuery("s", "r", []string{"all  privileges", "usage"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := `GRANT ALL, USAGE ON SCHEMA "s" TO "r"`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	privs, err := NormalizeDatabasePrivileges([]string{"All Privileges"})
	if err != nil || !slices.Equal(privs, []string{PrivilegeConnect, PrivilegeCreate, PrivilegeTemporary}) {
		t.Errorf("got %v, %v", privs, err)
	}
	if _, err := NormalizeParameterPrivileges([]string{privilegeAllPrivileges}); err == nil {
		t.Error("parameter grant accepted ALL PRIVILEGES")
	}
}

func TestDeniedParameter(t *testing.T) {
	for _, name := range []string{
		settingRole, "ROLE", "session_authorization", "session_preload_libraries", "local_preload_libraries",
		"shared_preload_libraries", "dynamic_library_path", "jit_provider", "session_replication_role", "lo_compat_privileges",
		"pgaudit.log", "PgAudit.Role", "set_user.block_alter_system", "anon.salt", "sepgsql.permissive",
	} {
		if !DeniedParameter(name) {
			t.Errorf("%q should be denied", name)
		}
	}
	for _, name := range []string{"maintenance_work_mem", paramSearchPath, settingStatementTimeout, "myapp.tenant", "rolex", "pgauditx"} {
		if DeniedParameter(name) {
			t.Errorf("%q should not be denied", name)
		}
	}
}

func TestDependentObjectsError(t *testing.T) {
	pqErr := &pq.Error{Code: sqlStateDependentObjects, Message: "role cannot be dropped", Detail: "privileges for database d"}
	err := dependentObjectsError("app", fmt.Errorf("wrapped: %w", pqErr))
	depErr, ok := errors.AsType[*DependentObjectsError](err)
	if !ok {
		t.Fatalf("expected a DependentObjectsError, got %v", err)
	}
	if depErr.Detail != "privileges for database d" || !strings.Contains(depErr.Error(), `role "app"`) {
		t.Errorf("unexpected error %q", depErr.Error())
	}
	if !errors.Is(err, pqErr) {
		t.Error("the PostgreSQL error is not wrapped")
	}
	if dependentObjectsError("app", &pq.Error{Code: "42704"}) != nil {
		t.Error("a different SQLSTATE was converted")
	}
	long := &pq.Error{Code: sqlStateDependentObjects, Detail: strings.Repeat("x", 5000)}
	if de, _ := errors.AsType[*DependentObjectsError](dependentObjectsError("app", long)); len(de.Detail) > maxDetailLength+3 {
		t.Errorf("detail not truncated: %d", len(de.Detail))
	}
}
