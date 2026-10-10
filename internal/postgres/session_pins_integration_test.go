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
	"os"
	"strconv"
	"testing"
)

// TestOperatorSessionPinsIntegration checks against a real server that the
// operator's session settings win over ALTER DATABASE / ALTER ROLE ... SET.
// It runs only when PGOP_TEST_PGHOST is set (with PGOP_TEST_PGPORT,
// PGOP_TEST_PGUSER, a superuser, and PGOP_TEST_PGPASSWORD), for example
// against a throwaway postgres:18 container:
//
//	docker run -d --rm -e POSTGRES_USER=pgop_operator -e POSTGRES_PASSWORD=pw -p 127.0.0.1:55432:5432 postgres:18
//	PGOP_TEST_PGHOST=127.0.0.1 PGOP_TEST_PGPORT=55432 PGOP_TEST_PGUSER=pgop_operator PGOP_TEST_PGPASSWORD=pw go test ./internal/postgres/ -run Integration
func TestOperatorSessionPinsIntegration(t *testing.T) {
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
	exec := func(q string) {
		t.Helper()
		if _, err := admin.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const db = "pgop_pin_test"
	exec(`DROP DATABASE IF EXISTS ` + db)
	exec(`DROP ROLE IF EXISTS pgop_pin_tenant`)
	exec(`CREATE ROLE pgop_pin_tenant`)
	exec(`CREATE DATABASE ` + db + ` OWNER pgop_pin_tenant`)
	t.Cleanup(func() {
		_, _ = admin.db.ExecContext(ctx, `ALTER ROLE `+quoteIdent(cfg.User)+` IN DATABASE `+db+` RESET ALL`)
		_, _ = admin.db.ExecContext(ctx, `DROP DATABASE IF EXISTS `+db)
		_, _ = admin.db.ExecContext(ctx, `DROP ROLE IF EXISTS pgop_pin_tenant`)
	})
	for _, set := range []string{
		`search_path = evil, public`,
		`role = 'pgop_pin_tenant'`,
		`statement_timeout = 1`,
		`lock_timeout = 1`,
		`idle_in_transaction_session_timeout = 1`,
		`default_transaction_read_only = on`,
		`check_function_bodies = off`,
		`row_security = off`,
		`exit_on_error = on`,
	} {
		exec(`ALTER DATABASE ` + db + ` SET ` + set)
	}
	// Only on newer servers: idle_session_timeout (14+), transaction_timeout (17+).
	for _, set := range []string{`idle_session_timeout = 1`, `transaction_timeout = 1`} {
		_, _ = admin.db.ExecContext(ctx, `ALTER DATABASE `+db+` SET `+set)
	}
	exec(`ALTER ROLE ` + quoteIdent(cfg.User) + ` IN DATABASE ` + db + ` SET search_path = evil2`)

	cfg.Database = db
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	want := map[string]string{
		"search_path":                         "pg_catalog, pg_temp",
		settingRole:                           roleNone,
		settingStatementTimeout:               "0",
		"lock_timeout":                        "0",
		"idle_in_transaction_session_timeout": "0",
		"default_transaction_read_only":       settingOff,
		"check_function_bodies":               "on",
		"row_security":                        "on",
		"exit_on_error":                       settingOff,
		"idle_session_timeout":                "0",
		"transaction_timeout":                 "0",
	}
	for name, v := range want {
		var got sql.NullString
		if err := c.db.QueryRowContext(ctx, `SELECT pg_catalog.current_setting($1, true)`, name).Scan(&got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !got.Valid {
			continue // not a parameter on this server version
		}
		if got.String != v {
			t.Errorf("%s = %q, want %q", name, got.String, v)
		}
	}
	var user string
	if err := c.db.QueryRowContext(ctx, `SELECT current_user`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if user != cfg.User {
		t.Errorf("current_user = %q, want %q", user, cfg.User)
	}
	// Writes and a longer statement work despite the database defaults.
	if _, err := c.db.ExecContext(ctx, `CREATE TABLE public.pin_t (x int)`); err != nil {
		t.Errorf("write failed: %v", err)
	}
	if _, err := c.db.ExecContext(ctx, `SELECT pg_catalog.pg_sleep(0.05)`); err != nil {
		t.Errorf("statement failed: %v", err)
	}

	checkCatalogHelpers(ctx, t, admin, c, cfg.Database, exec)
}

// checkCatalogHelpers checks the ownership-marker and catalog helpers against
// the server (see TestOperatorSessionPinsIntegration).
func checkCatalogHelpers(ctx context.Context, t *testing.T, admin, c *Client, db string, exec func(string)) {
	t.Helper()
	// Ownership markers: COMMENT ON ROLE in the CREATE ROLE transaction,
	// COMMENT ON DATABASE from a connection to another database, and the
	// comment in the membership closure.
	const marker = "pgop:v1:ns/c/uid-1"
	if err := admin.CreateRole(ctx, "pgop_pin_marked", RoleOptions{Inherit: true, ConnectionLimit: -1, Comment: marker}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.db.ExecContext(ctx, `DROP ROLE IF EXISTS pgop_pin_marked`) })
	if exists, comment, err := admin.RoleComment(ctx, "pgop_pin_marked"); err != nil || !exists || comment != marker {
		t.Errorf("RoleComment = %t %q %v, want the marker", exists, comment, err)
	}
	if exists, _, err := admin.RoleComment(ctx, "pgop_pin_missing"); err != nil || exists {
		t.Errorf("RoleComment of a missing role = %t %v", exists, err)
	}
	closure, err := admin.MembershipClosure(ctx, "pgop_pin_marked")
	if err != nil || len(closure) != 1 || closure[0].Comment != marker {
		t.Errorf("MembershipClosure = %+v %v", closure, err)
	}
	if err := admin.CommentOnDatabase(ctx, db, marker+"'; DROP ROLE x; --"); err != nil {
		t.Fatal(err)
	}
	if exists, comment, err := admin.DatabaseComment(ctx, db); err != nil || !exists || comment != marker+"'; DROP ROLE x; --" {
		t.Errorf("DatabaseComment = %t %q %v", exists, comment, err)
	}

	// Connectability, and databases with schema privileges of a role.
	exec(`CREATE ROLE pgop_pin_member`)
	t.Cleanup(func() {
		_, _ = admin.db.ExecContext(ctx, `DROP DATABASE IF EXISTS `+db)
		_, _ = admin.db.ExecContext(ctx, `DROP ROLE IF EXISTS pgop_pin_member`)
	})
	if _, err := c.db.ExecContext(ctx, `GRANT USAGE ON SCHEMA public TO pgop_pin_member`); err != nil {
		t.Fatal(err)
	}
	if allow, err := admin.DatabaseAllowsConnections(ctx, db); err != nil || !allow {
		t.Errorf("DatabaseAllowsConnections = %t %v, want true", allow, err)
	}
	_ = c.Close()
	exec(`ALTER DATABASE ` + db + ` WITH ALLOW_CONNECTIONS false`)
	if allow, err := admin.DatabaseAllowsConnections(ctx, db); err != nil || allow {
		t.Errorf("DatabaseAllowsConnections = %t %v, want false", allow, err)
	}
	dbs, err := admin.DatabasesWithSchemaPrivileges(ctx, "pgop_pin_member")
	if err != nil || len(dbs) != 1 || dbs[0] != (DatabaseRef{Name: db, AllowConns: false}) {
		t.Errorf("DatabasesWithSchemaPrivileges = %+v %v", dbs, err)
	}
}
