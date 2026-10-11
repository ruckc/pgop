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

func TestBuildSchemaObjectPrivilegesQuery(t *testing.T) {
	tests := []struct {
		grant     bool
		kind      SchemaObjectKind
		canonical string
		grantee   string
		privs     []string
		wgo       bool
		mode      RevokeMode
		want      string
	}{
		{true, SchemaTable, "app.orders", "app_ro", []string{"select", "insert"}, false, RevokeMode{},
			`GRANT SELECT, INSERT ON TABLE app.orders TO "app_ro"`},
		{true, SchemaTable, `app."We""ird"`, "pUBLIC", []string{PrivilegeTrigger, PrivilegeMaintain}, false, RevokeMode{},
			`GRANT TRIGGER, MAINTAIN ON TABLE app."We""ird" TO PUBLIC`},
		{true, SchemaFunction, `app."q""uote"(integer)`, `r"x`, []string{PrivilegeExecute}, true, RevokeMode{},
			`GRANT EXECUTE ON FUNCTION app."q""uote"(integer) TO "r""x" WITH GRANT OPTION`},
		{true, SchemaProcedure, "app.p()", "a", []string{PrivilegeExecute}, false, RevokeMode{},
			`GRANT EXECUTE ON PROCEDURE app.p() TO "a"`},
		{true, SchemaType, "app.posint", "a", []string{PrivilegeUsage}, false, RevokeMode{}, `GRANT USAGE ON TYPE app.posint TO "a"`},
		{false, SchemaSequence, "app.s", "a", []string{PrivilegeUsage, PrivilegeUpdate}, false,
			RevokeMode{GrantOptionOnly: true, Cascade: true}, `REVOKE GRANT OPTION FOR USAGE, UPDATE ON SEQUENCE app.s FROM "a" CASCADE`},
	}
	for _, tt := range tests {
		got, err := buildSchemaObjectPrivilegesQuery(tt.grant, tt.kind, tt.canonical, tt.grantee, tt.privs, tt.wgo, tt.mode)
		if err != nil || got != tt.want {
			t.Errorf("got %q, %v; want %q", got, err, tt.want)
		}
	}
	for name, call := range map[string]func() (string, error){
		"privilege of another kind": func() (string, error) {
			return buildSchemaObjectPrivilegesQuery(true, SchemaSequence, "app.s", "a", []string{PrivilegeExecute}, false, RevokeMode{})
		},
		"injected privilege": func() (string, error) {
			return buildSchemaObjectPrivilegesQuery(true, SchemaTable, "app.t", "a", []string{"SELECT ON TABLE x TO y; --"}, false, RevokeMode{})
		},
		"ALL is expanded by the caller": func() (string, error) {
			return buildSchemaObjectPrivilegesQuery(true, SchemaTable, "app.t", "a", []string{PrivilegeAll}, false, RevokeMode{})
		},
		"grant option to PUBLIC": func() (string, error) {
			return buildSchemaObjectPrivilegesQuery(true, SchemaTable, "app.t", "PUBLIC", []string{PrivilegeSelect}, true, RevokeMode{})
		},
		"none as grantee": func() (string, error) {
			return buildSchemaObjectPrivilegesQuery(true, SchemaTable, "app.t", "NONE", []string{PrivilegeSelect}, false, RevokeMode{})
		},
		"empty object": func() (string, error) {
			return buildSchemaObjectPrivilegesQuery(false, SchemaTable, "", "a", []string{PrivilegeSelect}, false, RevokeMode{})
		},
		"unknown kind": func() (string, error) {
			return buildSchemaObjectPrivilegesQuery(true, "DATABASE", "x", "a", []string{PrivilegeConnect}, false, RevokeMode{})
		},
	} {
		if q, err := call(); err == nil {
			t.Errorf("%s: built %q", name, q)
		}
	}
}

func TestBuildDefaultPrivilegesQuery(t *testing.T) {
	target := DefaultPrivilegesTarget{ForRole: `mig"rator`, Schema: `app"; DROP SCHEMA x; --`, Kind: DefaultTables}
	got, err := buildDefaultPrivilegesQuery(true, target, "app_ro", []string{PrivilegeSelect}, true, RevokeMode{})
	want := `ALTER DEFAULT PRIVILEGES FOR ROLE "mig""rator" IN SCHEMA "app""; DROP SCHEMA x; --" GRANT SELECT ON TABLES TO "app_ro" WITH GRANT OPTION`
	if err != nil || got != want {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
	got, err = buildDefaultPrivilegesQuery(false, DefaultPrivilegesTarget{ForRole: "m", Schema: "s1", Kind: DefaultFunctions},
		"Public", []string{PrivilegeExecute}, false, RevokeMode{Cascade: true})
	want = `ALTER DEFAULT PRIVILEGES FOR ROLE "m" IN SCHEMA "s1" REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC CASCADE`
	if err != nil || got != want {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
	for name, tc := range map[string]struct {
		target DefaultPrivilegesTarget
		privs  []string
		wgo    bool
		to     string
	}{
		"type privilege on sequences": {DefaultPrivilegesTarget{"m", "s1", DefaultSequences}, []string{PrivilegeExecute}, false, "a"},
		"unknown kind":                {DefaultPrivilegesTarget{"m", "s1", "SCHEMAS"}, []string{PrivilegeUsage}, false, "a"},
		"PUBLIC as forRole":           {DefaultPrivilegesTarget{"public", "s1", DefaultTypes}, []string{PrivilegeUsage}, false, "a"},
		"no schema":                   {DefaultPrivilegesTarget{"m", "", DefaultTypes}, []string{PrivilegeUsage}, false, "a"},
		"grant option to PUBLIC":      {DefaultPrivilegesTarget{"m", "s1", DefaultTypes}, []string{PrivilegeUsage}, true, "PUBLIC"},
	} {
		if q, err := buildDefaultPrivilegesQuery(true, tc.target, tc.to, tc.privs, tc.wgo, RevokeMode{}); err == nil {
			t.Errorf("%s: built %q", name, q)
		}
	}
}

func TestSchemaObjectHeld(t *testing.T) {
	o := SchemaObject{OwnerOID: 10, ACL: []ACLItem{
		{Grantee: 10, Privilege: PrivilegeSelect}, {Grantee: 10, Privilege: PrivilegeInsert},
		{Grantee: 0, Privilege: PrivilegeSelect},
		{Grantee: 20, Privilege: PrivilegeUpdate, Grantable: true}, {Grantee: 20, Privilege: PrivilegeSelect},
	}}
	if p, g := o.Held(10); !slices.Equal(p, []string{PrivilegeInsert, PrivilegeSelect}) || !slices.Equal(g, p) {
		t.Errorf("owner: %v %v", p, g)
	}
	if p, g := o.Held(20); !slices.Equal(p, []string{PrivilegeSelect, PrivilegeUpdate}) || !slices.Equal(g, []string{PrivilegeUpdate}) {
		t.Errorf("grantee: %v %v", p, g)
	}
	if p, g := o.Held(0); !slices.Equal(p, []string{PrivilegeSelect}) || g != nil {
		t.Errorf("PUBLIC: %v %v", p, g)
	}
}

func TestSchemaObjectQueriesUseBindParameters(t *testing.T) {
	for kind, q := range schemaObjectQueries {
		if !strings.Contains(q, "$1") || !strings.Contains(q, "LIMIT $4") {
			t.Errorf("%s: %s", kind, q)
		}
		if (kind == SchemaFunction || kind == SchemaProcedure) != strings.Contains(q, "$5") {
			t.Errorf("%s: $5 is only used by routine queries", kind)
		}
	}
	for kind := range SchemaObjectPrivileges {
		if _, ok := resolveObjectQueries[kind]; !ok {
			t.Errorf("%s has no resolve query", kind)
		}
	}
}
