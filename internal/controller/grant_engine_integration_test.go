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

package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"

	_ "github.com/lib/pq"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Names used by TestGrantEngineIntegration.
const (
	engX    = "eng_x"
	engHand = "hand"
)

// TestGrantEngineIntegration runs the grant reconcilers against a real
// server: declared grants the grantee already held (PostgreSQL's defaults,
// the owner's own privileges, grants made by hand) are never revoked when
// they leave the spec, and a revoke blocked by dependents pgop did not enable
// is skipped instead of failing forever. It runs only when PGOP_TEST_PGHOST
// is set, like the postgres package's integration tests:
//
//	docker run -d --rm -e POSTGRES_USER=pgop_operator -e POSTGRES_PASSWORD=pw -p 127.0.0.1:55432:5432 postgres:18
//	PGOP_TEST_PGHOST=127.0.0.1 PGOP_TEST_PGPORT=55432 PGOP_TEST_PGUSER=pgop_operator PGOP_TEST_PGPASSWORD=pw \
//	  go test ./internal/controller/ -run Integration
func TestGrantEngineIntegration(t *testing.T) {
	host := os.Getenv("PGOP_TEST_PGHOST")
	if host == "" {
		t.Skip("PGOP_TEST_PGHOST not set")
	}
	port, err := strconv.Atoi(os.Getenv("PGOP_TEST_PGPORT"))
	if err != nil {
		t.Fatalf("PGOP_TEST_PGPORT: %v", err)
	}
	user, password := os.Getenv("PGOP_TEST_PGUSER"), os.Getenv("PGOP_TEST_PGPASSWORD")
	ctx := context.Background()
	const db = "pgop_engine_test"
	raw := func(database string) *sql.DB {
		t.Helper()
		conn, err := sql.Open("postgres", fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			host, port, user, password, database))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	admin := raw("postgres")
	exec := func(conn *sql.DB, q string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	roles := []string{"eng_owner", engX, "eng_y"}
	cleanup := func() {
		_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+db)
		for _, r := range roles {
			_, _ = admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+r)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	for _, r := range roles {
		exec(admin, `CREATE ROLE `+r)
	}
	exec(admin, `CREATE DATABASE `+db)
	conn := raw(db)
	exec(conn, `CREATE SCHEMA app AUTHORIZATION eng_owner`)
	exec(conn, `CREATE SCHEMA hand`)

	adminPG, err := postgres.NewClient(postgres.ConnectionConfig{Host: host, Port: int32(port), User: user, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminPG.Close() }()
	pg, err := postgres.NewClient(postgres.ConnectionConfig{Host: host, Port: int32(port), User: user, Password: password, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.Close() }()
	checker := &granteeChecker{pg: pg, policy: &postgresv1alpha1.RolePolicySpec{AllowedExistingRoles: roles}}
	has := func(q string) bool {
		t.Helper()
		var b bool
		if err := conn.QueryRowContext(ctx, q).Scan(&b); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return b
	}

	database := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{
		Grants: []postgresv1alpha1.DatabaseGrantSpec{{Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeConnect}}},
		Schemas: []postgresv1alpha1.SchemaSpec{
			{Name: publicSchemaName, Grants: []postgresv1alpha1.GrantSpec{{Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeUsage}}}},
			{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{{Role: "eng_owner", Privileges: []string{postgres.PrivilegeAll}, WithGrantOption: true}}},
		},
	}}
	managedSchemas := setOf([]string{publicSchemaName, grantTestRole, engHand})
	reconcile := func() error {
		return errors.Join(
			reconcileDatabaseGrants(ctx, adminPG, database, db, checker),
			reconcileSchemaGrants(ctx, pg, database, managedSchemas, checker),
		)
	}

	step := func(s string) { t.Log(s) }
	step("declaring grants the grantees already hold records nothing")
	if err := reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(database.Status.ManagedGrants) != 0 || len(database.Status.ManagedSchemaGrants) != 0 {
		t.Fatalf("recorded pre-existing privileges: %+v %+v", database.Status.ManagedGrants, database.Status.ManagedSchemaGrants)
	}

	step("removing them keeps PostgreSQL's defaults and the owner's privileges")
	database.Spec.Grants, database.Spec.Schemas = nil, nil
	if err := reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT has_database_privilege('eng_y', '` + db + `', 'CONNECT')`, // through PUBLIC
		`SELECT has_schema_privilege('eng_y', 'public', 'USAGE')`,         // through PUBLIC
		`SELECT has_schema_privilege('eng_owner', 'app', 'CREATE')`,
		`SELECT has_schema_privilege('eng_owner', 'app', 'USAGE')`,
	} {
		if !has(q) {
			t.Errorf("%s: false after the declared grant was removed", q)
		}
	}

	step("a hand-made grant with grant option, passed on, is not touched")
	exec(conn, `GRANT USAGE ON SCHEMA hand TO eng_x WITH GRANT OPTION`)
	exec(conn, `SET ROLE eng_x; GRANT USAGE ON SCHEMA hand TO eng_y; RESET ROLE`)
	database.Spec.Schemas = []postgresv1alpha1.SchemaSpec{{Name: engHand, Grants: []postgresv1alpha1.GrantSpec{
		{Role: engX, Privileges: []string{postgres.PrivilegeUsage}}}}}
	if err := reconcile(); err != nil {
		t.Fatal(err)
	}
	database.Spec.Schemas = nil
	if err := reconcile(); err != nil {
		t.Fatalf("removing a declared, pre-existing grant failed: %v", err)
	}
	if !has(`SELECT has_schema_privilege('eng_y', 'hand', 'USAGE')`) {
		t.Error("the hand-made grant chain was revoked")
	}

	step("a revoke blocked by dependents pgop did not enable is skipped, not retried forever")
	exec(conn, `REVOKE USAGE ON SCHEMA hand FROM eng_x CASCADE`)
	database.Spec.Schemas = []postgresv1alpha1.SchemaSpec{{Name: engHand, Grants: []postgresv1alpha1.GrantSpec{
		{Role: engX, Privileges: []string{postgres.PrivilegeUsage}}}}}
	if err := reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(database.Status.ManagedSchemaGrants) != 1 {
		t.Fatalf("pgop's grant was not recorded: %+v", database.Status.ManagedSchemaGrants)
	}
	exec(conn, `GRANT USAGE ON SCHEMA hand TO eng_x WITH GRANT OPTION`) // by hand, on top of pgop's grant
	exec(conn, `SET ROLE eng_x; GRANT USAGE ON SCHEMA hand TO eng_y; RESET ROLE`)
	database.Spec.Schemas = nil
	err = reconcile()
	ce, ok := errors.AsType[*conditionError](err)
	if !ok || ce.reason != ReasonRevokeSkipped {
		t.Fatalf("want RevokeSkipped, got %v", err)
	}
	if len(database.Status.ManagedSchemaGrants) != 0 {
		t.Errorf("the skipped revoke stays tracked: %+v", database.Status.ManagedSchemaGrants)
	}
	if err := reconcile(); err != nil {
		t.Errorf("the next reconcile still fails: %v", err)
	}
}
