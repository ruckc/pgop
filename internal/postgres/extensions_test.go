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
	"slices"
	"strings"
	"testing"
)

func TestBuildCreateExtensionQuery(t *testing.T) {
	tests := []struct {
		name, schema, version string
		cascade               bool
		want                  string
	}{
		{"pg_trgm", "", "", false, `CREATE EXTENSION "pg_trgm"`},
		{"uuid-ossp", "ext", "1.1", false, `CREATE EXTENSION "uuid-ossp" SCHEMA "ext" VERSION '1.1'`},
		{"earthdistance", "geo", "", true, `CREATE EXTENSION "earthdistance" SCHEMA "geo" CASCADE`},
		// Hostile names and versions stay quoted identifiers and literals.
		{`x"y`, `s"`, `1'0`, false, `CREATE EXTENSION "x""y" SCHEMA "s""" VERSION '1''0'`},
		{`a"; DROP DATABASE x; --`, `pub"lic`, `1.0'; DROP ROLE r; --`, true,
			`CREATE EXTENSION "a""; DROP DATABASE x; --" SCHEMA "pub""lic" VERSION '1.0''; DROP ROLE r; --' CASCADE`},
		{"e", "", `1\'`, false, `CREATE EXTENSION "e" VERSION E'1\\'''`},
	}
	for _, tt := range tests {
		if got := buildCreateExtensionQuery(tt.name, tt.schema, tt.version, tt.cascade); got != tt.want {
			t.Errorf("buildCreateExtensionQuery(%q, %q, %q, %t) = %q, want %q", tt.name, tt.schema, tt.version, tt.cascade, got, tt.want)
		}
	}
}

func TestBuildUpdateAndDropExtensionQuery(t *testing.T) {
	if got, want := buildUpdateExtensionQuery("hstore", "1.8"), `ALTER EXTENSION "hstore" UPDATE TO '1.8'`; got != want {
		t.Errorf("update = %q, want %q", got, want)
	}
	if got, want := buildUpdateExtensionQuery(`h"s`, `1'; DROP ROLE x; --`), `ALTER EXTENSION "h""s" UPDATE TO '1''; DROP ROLE x; --'`; got != want {
		t.Errorf("hostile update = %q, want %q", got, want)
	}
	if got, want := buildDropExtensionQuery(`x"; DROP`), `DROP EXTENSION "x""; DROP" RESTRICT`; got != want {
		t.Errorf("drop = %q, want %q", got, want)
	}
}

func TestBuildMemberPrivilegesQueries(t *testing.T) {
	got, err := buildMemberPrivilegesQueries(true, MemberRoutines, []string{"partman.run_maintenance()", `"S".f(integer)`},
		[]string{"execute"}, "app")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`GRANT EXECUTE ON ROUTINE partman.run_maintenance(), "S".f(integer) TO "app"`}; !slices.Equal(got, want) {
		t.Errorf("grant = %q, want %q", got, want)
	}
	got, err = buildMemberPrivilegesQueries(false, MemberTables, []string{"partman.part_config"}, []string{PrivilegeSelect, "UPDATE"}, "public")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`REVOKE SELECT, UPDATE ON TABLE partman.part_config FROM PUBLIC`}; !slices.Equal(got, want) {
		t.Errorf("revoke = %q, want %q", got, want)
	}

	// Batches of memberBatch objects.
	ids := make([]string, memberBatch+1)
	for i := range ids {
		ids[i] = "s.seq"
	}
	got, err = buildMemberPrivilegesQueries(true, MemberSequences, ids, []string{"USAGE"}, "app")
	if err != nil || len(got) != 2 {
		t.Fatalf("batches = %d, %v", len(got), err)
	}
	if strings.Count(got[0], "s.seq") != memberBatch || strings.Count(got[1], "s.seq") != 1 {
		t.Errorf("batch sizes wrong: %q", got)
	}

	for _, tt := range []struct {
		kind  MemberKind
		privs []string
	}{
		{MemberTables, []string{"TRIGGER"}},                        // never offered on extension tables
		{MemberTables, []string{"SELECT; DROP TABLE x"}},           // injection attempt
		{MemberTables, []string{"ALL"}},                            // expanded by the caller
		{MemberSequences, []string{"DELETE"}},                      // not a sequence privilege
		{MemberRoutines, []string{PrivilegeExecute, "USAGE"}},      // not a routine privilege
		{MemberKind("FUNCTION; DROP"), []string{PrivilegeExecute}}, // unknown kind
	} {
		if _, err := buildMemberPrivilegesQueries(true, tt.kind, []string{"s.x"}, tt.privs, "app"); err == nil {
			t.Errorf("%s %v: accepted", tt.kind, tt.privs)
		}
	}
	if _, err := buildMemberPrivilegesQueries(true, MemberRoutines, []string{"s.f()"}, []string{PrivilegeExecute}, "none"); err == nil {
		t.Error("grantee none accepted")
	}
	if _, err := buildMemberPrivilegesQueries(true, MemberRoutines, []string{""}, []string{PrivilegeExecute}, "app"); err == nil {
		t.Error("empty identity accepted")
	}
}
