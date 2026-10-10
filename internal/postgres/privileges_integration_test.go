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
	"os"
	"slices"
	"strconv"
	"testing"
)

// TestPrivilegesIntegration checks the generic GRANT/REVOKE builders, the
// PUBLIC grantee and the PUBLIC privilege lookup against a real server. Like
// TestOperatorSessionPinsIntegration it runs only when PGOP_TEST_PGHOST is
// set.
func TestPrivilegesIntegration(t *testing.T) {
	host := os.Getenv("PGOP_TEST_PGHOST")
	if host == "" {
		t.Skip("PGOP_TEST_PGHOST not set")
	}
	port, err := strconv.Atoi(os.Getenv("PGOP_TEST_PGPORT"))
	if err != nil {
		t.Fatalf("PGOP_TEST_PGPORT: %v", err)
	}
	cfg := ConnectionConfig{Host: host, Port: int32(port), User: os.Getenv("PGOP_TEST_PGUSER"), Password: os.Getenv("PGOP_TEST_PGPASSWORD")}
	ctx := context.Background()
	admin, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	exec := func(c *Client, q string) {
		t.Helper()
		if _, err := c.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const db = "pgop_priv_test"
	cleanup := func() {
		_, _ = admin.db.ExecContext(ctx, `DROP DATABASE IF EXISTS `+db)
		_, _ = admin.db.ExecContext(ctx, `DROP ROLE IF EXISTS pgop_priv_reader`)
		_, _ = admin.db.ExecContext(ctx, `DROP ROLE IF EXISTS "PUBLIC"`)
	}
	cleanup()
	t.Cleanup(cleanup)
	exec(admin, `CREATE ROLE pgop_priv_reader`)
	// A role literally named "PUBLIC" (quoted, upper case) is a legal
	// ordinary role; pgop must never grant to it by accident.
	exec(admin, `CREATE ROLE "PUBLIC"`)
	exec(admin, `CREATE DATABASE `+db)

	dbObj := PrivilegeObject{Kind: ObjectDatabase, Name: db}
	publicOn := func(c *Client, obj PrivilegeObject) []string {
		t.Helper()
		privs, found, err := c.PublicPrivileges(ctx, obj)
		if err != nil || !found {
			t.Fatalf("PublicPrivileges(%s) = %v %t %v", obj, privs, found, err)
		}
		return privs
	}
	if got := publicOn(admin, dbObj); !slices.Equal(got, []string{PrivilegeConnect, PrivilegeTemporary}) {
		t.Errorf("default PUBLIC privileges on the database = %v", got)
	}
	if _, found, err := admin.PublicPrivileges(ctx, PrivilegeObject{Kind: ObjectDatabase, Name: "pgop_priv_missing"}); err != nil || found {
		t.Errorf("PublicPrivileges of a missing database: found=%t err=%v", found, err)
	}

	if err := admin.RevokePrivileges(ctx, dbObj, testPublic, []string{PrivilegeConnect}, RevokeMode{}); err != nil {
		t.Fatal(err)
	}
	if got := publicOn(admin, dbObj); !slices.Equal(got, []string{PrivilegeTemporary}) {
		t.Errorf("after REVOKE CONNECT FROM PUBLIC: %v", got)
	}
	if err := admin.GrantPrivileges(ctx, dbObj, "PUBLIC", []string{PrivilegeConnect}, false); err != nil {
		t.Fatal(err)
	}
	if got := publicOn(admin, dbObj); !slices.Equal(got, []string{PrivilegeConnect, PrivilegeTemporary}) {
		t.Errorf("after GRANT CONNECT TO PUBLIC: %v", got)
	}
	var quotedRoleHasCreate bool
	if err := admin.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database d, aclexplode(d.datacl) a
WHERE d.datname = $1 AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'PUBLIC'))`, db).Scan(&quotedRoleHasCreate); err != nil {
		t.Fatal(err)
	}
	if quotedRoleHasCreate {
		t.Error(`the role named "PUBLIC" received a grant meant for the PUBLIC pseudo-role`)
	}

	checkRoleGrantsIntegration(ctx, t, admin, dbObj, cfg.User)

	// Schemas, on a connection to the database.
	dbCfg := cfg
	dbCfg.Database = db
	c, err := NewClient(dbCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	exec(c, `CREATE SCHEMA app`)
	checkSchemaPrivilegesIntegration(ctx, t, c, publicOn)
	_ = c.Close()
}

// checkRoleGrantsIntegration checks grants with the grant option, cascading
// revokes and LookupRole (see TestPrivilegesIntegration).
func checkRoleGrantsIntegration(ctx context.Context, t *testing.T, admin *Client, dbObj PrivilegeObject, operator string) {
	t.Helper()
	if err := admin.GrantPrivileges(ctx, dbObj, "pgop_priv_reader", []string{PrivilegeCreate}, true); err != nil {
		t.Fatal(err)
	}
	if err := admin.RevokePrivileges(ctx, dbObj, "pgop_priv_reader", []string{PrivilegeCreate}, RevokeMode{GrantOptionOnly: true, Cascade: true}); err != nil {
		t.Fatal(err)
	}
	if err := admin.RevokePrivileges(ctx, dbObj, "pgop_priv_reader", []string{PrivilegeCreate}, RevokeMode{}); err != nil {
		t.Fatal(err)
	}

	if r, err := admin.LookupRole(ctx, "pgop_priv_reader"); err != nil || r == nil || r.Superuser || r.Name != "pgop_priv_reader" {
		t.Errorf("LookupRole = %+v %v", r, err)
	}
	if r, err := admin.LookupRole(ctx, operator); err != nil || r == nil || !r.Superuser {
		t.Errorf("LookupRole(operator) = %+v %v, want a superuser", r, err)
	}
	if r, err := admin.LookupRole(ctx, "pgop_priv_missing"); err != nil || r != nil {
		t.Errorf("LookupRole of a missing role = %+v %v", r, err)
	}
}

// checkSchemaPrivilegesIntegration checks schema grants and PUBLIC's
// privileges on schemas, on a connection to a database whose schema app
// exists (see TestPrivilegesIntegration).
func checkSchemaPrivilegesIntegration(ctx context.Context, t *testing.T, c *Client, publicOn func(*Client, PrivilegeObject) []string) {
	t.Helper()
	schemaObj := PrivilegeObject{Kind: ObjectSchema, Name: testPublic}
	before := publicOn(c, schemaObj)
	if !slices.Contains(before, PrivilegeUsage) {
		t.Errorf("default PUBLIC privileges on schema public = %v, want USAGE", before)
	}
	if err := c.RevokePrivileges(ctx, schemaObj, "PUBLIC", []string{PrivilegeUsage}, RevokeMode{}); err != nil {
		t.Fatal(err)
	}
	if got := publicOn(c, schemaObj); slices.Contains(got, PrivilegeUsage) {
		t.Errorf("after REVOKE USAGE ON SCHEMA public FROM PUBLIC: %v", got)
	}
	if got := publicOn(c, PrivilegeObject{Kind: ObjectSchema, Name: testMember}); len(got) != 0 {
		t.Errorf("PUBLIC privileges on a new schema = %v, want none", got)
	}
	if err := c.GrantPrivileges(ctx, PrivilegeObject{Kind: ObjectSchema, Name: testMember}, "pgop_priv_reader",
		[]string{PrivilegeUsage, PrivilegeCreate}, true); err != nil {
		t.Fatal(err)
	}
	if err := c.RevokePrivileges(ctx, PrivilegeObject{Kind: ObjectSchema, Name: testMember}, "pgop_priv_reader",
		[]string{PrivilegeCreate, PrivilegeUsage}, RevokeMode{Cascade: true}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{testMember: true, testPublic: true, "pgop_missing": false} {
		if got, err := c.SchemaExists(ctx, name); err != nil || got != want {
			t.Errorf("SchemaExists(%s) = %t %v", name, got, err)
		}
	}
	if _, found, err := c.PublicPrivileges(ctx, PrivilegeObject{Kind: ObjectSchema, Name: "pgop_missing"}); err != nil || found {
		t.Errorf("PublicPrivileges of a missing schema: found=%t err=%v", found, err)
	}
}
