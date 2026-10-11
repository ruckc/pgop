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
	"slices"
	"strconv"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Names used by TestObjectGrantsIntegration.
const (
	ogDB       = "pgop_og_test"
	ogOwner    = "og_owner"
	ogApp      = "og_app"
	ogMigrator = "og_migrator"
	ogOutsider = "og_outsider"
	ogSuper    = "og_super"
	ogSchema   = "app"
	ogMarker   = "pgop-test-marker"
	// ogWeird is a table name with a quote, a semicolon and spaces.
	ogWeird = `Weird"Name; DROP TABLE app.t1; --`
)

// ogIT is the state of TestObjectGrantsIntegration.
type ogIT struct {
	t        *testing.T
	ctx      context.Context
	conn     *sql.DB // superuser connection to ogDB
	pg       *postgres.Client
	version  int
	database *postgresv1alpha1.Database
	checker  func() *granteeChecker
}

func (it *ogIT) exec(q string) {
	it.t.Helper()
	if _, err := it.conn.ExecContext(it.ctx, q); err != nil {
		it.t.Fatalf("%s: %v", q, err)
	}
}

func (it *ogIT) query(q string) string {
	it.t.Helper()
	var s sql.NullString
	if err := it.conn.QueryRowContext(it.ctx, q).Scan(&s); err != nil {
		it.t.Fatalf("%s: %v", q, err)
	}
	return s.String
}

// as runs q as role (SET ROLE in one transaction).
func (it *ogIT) as(role, q string) {
	it.t.Helper()
	it.exec(`BEGIN; SET LOCAL ROLE ` + role + `; ` + q + `; COMMIT`)
}

func (it *ogIT) reconcile() error {
	schemas := map[string]bool{ogSchema: true}
	c := it.checker()
	return errors.Join(
		reconcileObjectGrants(it.ctx, it.pg, it.database, schemas, it.version, c, nil),
		reconcileDefaultPrivileges(it.ctx, it.pg, it.database, schemas, it.version, c, nil))
}

func (it *ogIT) mustReconcile() {
	it.t.Helper()
	if err := it.reconcile(); err != nil {
		it.t.Fatal(err)
	}
}

func (it *ogIT) wantReason(reason string, parts ...string) {
	it.t.Helper()
	err := it.reconcile()
	ce, ok := errors.AsType[*conditionError](err)
	if !ok || ce.reason != reason {
		it.t.Fatalf("want reason %s, got %v", reason, err)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			it.t.Errorf("%q not in %v", p, err)
		}
	}
}

// has reports a privilege check (has_*_privilege) for og_app.
func (it *ogIT) has(fn, object, privilege string) bool {
	it.t.Helper()
	return it.query(fmt.Sprintf(`SELECT CASE WHEN %s('%s', %s, '%s') THEN 'yes' END`, fn, ogApp, quoteLit(object), privilege)) == "yes"
}

func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (it *ogIT) setGrants(grants ...postgresv1alpha1.ObjectGrantSpec) {
	it.database.Spec.Schemas[0].ObjectGrants = grants
}

func (it *ogIT) ledger() []string {
	out := make([]string, 0, len(it.database.Status.ManagedObjectGrants))
	for _, g := range it.database.Status.ManagedObjectGrants {
		out = append(out, fmt.Sprintf("%s %s %s %s", g.Kind, g.Object, g.Role, strings.Join(g.Privileges, ",")))
	}
	return out
}

// TestObjectGrantsIntegration runs the object grant and default privilege
// reconcilers against a real server. It runs only when PGOP_TEST_PGHOST is
// set (see TestGrantEngineIntegration); it drops and re-creates its database
// and roles, so it can be re-run.
func TestObjectGrantsIntegration(t *testing.T) {
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
	raw := func(database string) *sql.DB {
		t.Helper()
		conn, err := sql.Open("postgres", fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			host, port, user, password, database))
		if err != nil {
			t.Fatal(err)
		}
		conn.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	admin := raw("postgres")
	roles := []string{ogOwner, ogApp, ogMigrator, ogOutsider, ogSuper}
	cleanup := func() {
		_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+ogDB+` WITH (FORCE)`)
		for _, r := range roles {
			_, _ = admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+r)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	setup := &ogIT{t: t, ctx: ctx, conn: admin}
	for _, r := range roles {
		setup.exec(`CREATE ROLE ` + r)
	}
	setup.exec(`ALTER ROLE ` + ogSuper + ` SUPERUSER`)
	for _, r := range []string{ogOwner, ogApp, ogMigrator} {
		setup.exec(`COMMENT ON ROLE ` + r + ` IS '` + ogMarker + `'`)
	}
	setup.exec(`CREATE DATABASE ` + ogDB + ` OWNER ` + ogOwner)

	cfg := postgres.ConnectionConfig{Host: host, Port: int32(port), User: user, Password: password, Database: ogDB}
	pg, err := postgres.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.Close() }()
	version, err := pg.ServerVersionNum(ctx)
	if err != nil {
		t.Fatal(err)
	}
	managed := managedRoles{ogOwner: ogMarker, ogApp: ogMarker, ogMigrator: ogMarker}
	it := &ogIT{t: t, ctx: ctx, conn: raw(ogDB), pg: pg, version: version,
		database: &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{
			Schemas: []postgresv1alpha1.SchemaSpec{{Name: ogSchema, Owner: ogOwner}}}},
		checker: func() *granteeChecker {
			return &granteeChecker{pg: pg, policy: &postgresv1alpha1.RolePolicySpec{}, managed: managed}
		}}
	ogITFixtures(it, user)
	ogITTables(it)
	ogITRoutinesAndTypes(it)
	ogITDefaultPrivileges(it)
	ogITRoleCleanup(it, admin)
}

// ogITFixtures creates the schema and its objects.
func ogITFixtures(it *ogIT, operator string) {
	it.exec(`CREATE SCHEMA ` + ogSchema + ` AUTHORIZATION ` + ogOwner)
	it.exec(`GRANT CREATE ON SCHEMA app TO ` + ogOutsider + `, ` + ogMigrator)
	it.as(ogOwner, `CREATE TABLE app.t1 (id int); CREATE TABLE app.`+pqIdent(ogWeird)+` (id int);
CREATE VIEW app.v1 AS SELECT 1 AS x; CREATE SEQUENCE app.s1;
CREATE FUNCTION app.f(integer) RETURNS integer LANGUAGE sql AS 'SELECT $1';
CREATE FUNCTION app.f(text) RETURNS text LANGUAGE sql AS 'SELECT $1';
CREATE FUNCTION app."q""uote"(integer) RETURNS integer LANGUAGE sql AS 'SELECT $1';
CREATE FUNCTION app.owner_definer() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1';
CREATE PROCEDURE app.p() LANGUAGE sql AS 'SELECT 1';
CREATE DOMAIN app.posint AS integer CHECK (VALUE > 0); CREATE TYPE app.mood AS ENUM ('ok')`)
	it.as(ogOutsider, `CREATE TABLE app.out_t (id int)`)
	it.as(ogSuper, `CREATE TABLE app.su_t (id int);
CREATE FUNCTION app.su_definer() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`)
	// Objects of the operator (the test's superuser connection).
	it.exec(`CREATE TABLE app.op_t (id int); CREATE VIEW app.op_v AS SELECT 1 AS x;
CREATE FUNCTION app.op_f() RETURNS integer LANGUAGE plpgsql AS 'BEGIN RETURN 1; END';
CREATE FUNCTION app.op_sd() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1';
CREATE FUNCTION app.op_c(integer) RETURNS integer LANGUAGE internal STRICT AS 'int4abs';
CREATE EXTENSION citext SCHEMA app`)
	if got := it.query(`SELECT session_user`); got != operator {
		it.t.Fatalf("session user %s, want %s", got, operator)
	}
}

func pqIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// ogITTables checks "*" and named table grants, the owner rules and revokes.
func ogITTables(it *ogIT) {
	t := it.t
	t.Log(`"*" grants SELECT on the tables of trusted owners only`)
	it.setGrants(postgresv1alpha1.ObjectGrantSpec{Role: ogApp, Kind: postgresv1alpha1.ObjectGrantTable,
		Objects: []string{selectAll}, Privileges: []string{postgres.PrivilegeSelect, postgres.PrivilegeTrigger}})
	it.mustReconcile()
	for obj, want := range map[string]bool{"app.t1": true, "app." + pqIdent(ogWeird): true, "app.v1": true, "app.op_t": true,
		"app.out_t": false, "app.su_t": false, "app.op_v": false} {
		if got := it.has("has_table_privilege", obj, "SELECT"); got != want {
			t.Errorf("SELECT on %s = %t, want %t", obj, got, want)
		}
	}
	if it.has("has_table_privilege", "app.op_t", "TRIGGER") || !it.has("has_table_privilege", "app.t1", "TRIGGER") {
		t.Error("TRIGGER must be granted on og_owner's table only, not on the operator's")
	}
	if it.query(`SELECT count(*) FROM app.t1`) != "0" {
		t.Fatal("app.t1 is gone")
	}
	st := it.database.Status.ObjectGrants
	if len(st) != 1 || st[0].Granted != 4 || st[0].Skipped != 4 {
		t.Fatalf("status.objectGrants = %+v", st)
	}

	t.Log("a table created later is picked up by the next reconcile")
	it.as(ogOwner, `CREATE TABLE app.t2 (id int)`)
	it.mustReconcile()
	if !it.has("has_table_privilege", "app.t2", "SELECT") {
		t.Fatal("app.t2 was not granted on")
	}

	t.Log("objects that stop matching are revoked, dropped ones forgotten")
	it.exec(`DROP TABLE app.t2`)
	it.setGrants(postgresv1alpha1.ObjectGrantSpec{Role: ogApp, Kind: postgresv1alpha1.ObjectGrantTable,
		Objects: []string{"t1", ogWeird}, Privileges: []string{postgres.PrivilegeSelect}})
	it.mustReconcile()
	if it.has("has_table_privilege", "app.v1", "SELECT") || it.has("has_table_privilege", "app.t1", "TRIGGER") ||
		!it.has("has_table_privilege", "app."+pqIdent(ogWeird), "SELECT") {
		t.Fatal("v1 and TRIGGER must be revoked, the weird table kept")
	}
	if l := it.ledger(); len(l) != 2 || slices.ContainsFunc(l, func(s string) bool { return strings.Contains(s, "t2") }) {
		t.Fatalf("ledger = %v", l)
	}

	t.Log("a renamed object keeps its entry (by OID); revoking follows the object")
	named := it.database.Spec.Schemas[0].ObjectGrants
	it.setGrants(postgresv1alpha1.ObjectGrantSpec{Role: ogApp, Kind: postgresv1alpha1.ObjectGrantTable,
		Objects: []string{selectAll}, Privileges: []string{postgres.PrivilegeSelect}})
	it.mustReconcile()
	before := len(it.ledger())
	it.as(ogOwner, `ALTER TABLE app.t1 RENAME TO t1_renamed`)
	it.mustReconcile()
	if l := it.ledger(); len(l) != before || !slices.ContainsFunc(l, func(s string) bool { return strings.Contains(s, "t1_renamed") }) ||
		!it.has("has_table_privilege", "app.t1_renamed", "SELECT") {
		t.Fatalf("ledger = %v", l)
	}
	it.as(ogOwner, `ALTER TABLE app.t1_renamed RENAME TO t1`)
	it.setGrants(named...)
	it.mustReconcile()
	if it.has("has_table_privilege", "app.v1", "SELECT") || len(it.ledger()) != 2 {
		t.Fatalf("narrowing again: %v", it.ledger())
	}

	t.Log("named objects of untrusted owners are refused; missing names are reported; names are never SQL")
	it.setGrants(postgresv1alpha1.ObjectGrantSpec{Role: ogApp, Kind: postgresv1alpha1.ObjectGrantTable,
		Objects: []string{"t1", "su_t", "out_t", "x'); DROP TABLE app.t1; --"}, Privileges: []string{postgres.PrivilegeSelect}})
	err := it.reconcile()
	for _, part := range []string{"su_t is owned by the superuser " + ogSuper, "out_t is owned by " + ogOutsider,
		"does not exist in schema app"} {
		if err == nil || !strings.Contains(err.Error(), part) {
			t.Errorf("%q not in %v", part, err)
		}
	}
	it.wantReason(ReasonObjectGrantSkipped)
	if it.query(`SELECT count(*) FROM app.t1`) != "0" || it.has("has_table_privilege", "app.su_t", "SELECT") {
		t.Fatal("injection or refused grant")
	}

	t.Log("sequences: ALL")
	it.setGrants(postgresv1alpha1.ObjectGrantSpec{Role: ogApp, Kind: postgresv1alpha1.ObjectGrantSequence,
		Objects: []string{selectAll}, Privileges: []string{postgres.PrivilegeAll}})
	it.mustReconcile()
	if !it.has("has_sequence_privilege", "app.s1", "USAGE") || !it.has("has_sequence_privilege", "app.s1", "UPDATE") ||
		it.has("has_table_privilege", "app.t1", "SELECT") {
		t.Fatal("sequence ALL not granted, or table grants not revoked")
	}
	it.setGrants()
	it.mustReconcile()
	if it.has("has_sequence_privilege", "app.s1", "USAGE") || len(it.database.Status.ManagedObjectGrants) != 0 {
		t.Fatalf("sequence grants not revoked: %v", it.ledger())
	}
}

// ogITRoutinesAndTypes checks routine signatures, the superuser function
// rule, extension members and types.
func ogITRoutinesAndTypes(it *ogIT) {
	t := it.t
	t.Log("a signature selects one overload; quotes in names are data")
	it.setGrants(postgresv1alpha1.ObjectGrantSpec{Role: ogApp, Kind: postgresv1alpha1.ObjectGrantFunction,
		Objects: []string{ogtSigF, `q"uote(integer)`}, Privileges: []string{postgres.PrivilegeExecute}})
	it.mustReconcile()
	if l := it.ledger(); !slices.Equal(l, []string{`function app."q""uote"(integer) og_app EXECUTE`,
		"function app.f(integer) og_app EXECUTE"}) {
		t.Fatalf("ledger = %v", l)
	}

	t.Log("a malformed or hostile signature resolves to nothing")
	it.setGrants(postgresv1alpha1.ObjectGrantSpec{Role: ogApp, Kind: postgresv1alpha1.ObjectGrantFunction,
		Objects:    []string{"f(integer); DROP TABLE app.t1; --)", "f(integer) , app.op_sd()", "nope(integer)"},
		Privileges: []string{postgres.PrivilegeExecute}})
	it.wantReason(ReasonObjectNotFound, "nope(integer)")
	if it.query(`SELECT count(*) FROM app.t1`) != "0" || len(it.database.Status.ManagedObjectGrants) != 0 {
		t.Fatalf("hostile signature: %v", it.ledger())
	}

	t.Log(`"*" skips superuser SECURITY DEFINER and C functions and extension members`)
	it.setGrants(
		postgresv1alpha1.ObjectGrantSpec{Role: ogApp, Kind: postgresv1alpha1.ObjectGrantFunction, Objects: []string{selectAll},
			Privileges: []string{postgres.PrivilegeExecute}},
		postgresv1alpha1.ObjectGrantSpec{Role: ogApp, Kind: postgresv1alpha1.ObjectGrantProcedure, Objects: []string{selectAll},
			Privileges: []string{postgres.PrivilegeAll}},
		postgresv1alpha1.ObjectGrantSpec{Role: ogApp, Kind: postgresv1alpha1.ObjectGrantType, Objects: []string{selectAll},
			Privileges: []string{postgres.PrivilegeUsage}})
	it.mustReconcile()
	granted := make([]string, 0, len(it.database.Status.ManagedObjectGrants))
	for _, g := range it.database.Status.ManagedObjectGrants {
		granted = append(granted, g.Object)
	}
	want := []string{"app.f(integer)", "app.f(text)", `app."q""uote"(integer)`, "app.owner_definer()", "app.op_f()",
		"app.p()", "app.mood", "app.posint"}
	slices.Sort(granted)
	slices.Sort(want)
	if !slices.Equal(granted, want) {
		t.Fatalf("granted on %v, want %v", granted, want)
	}
	for _, s := range it.database.Status.ObjectGrants {
		if s.Kind == postgresv1alpha1.ObjectGrantFunction && (s.Skipped < 3 ||
			!slices.ContainsFunc(s.SkippedExamples, func(e string) bool { return strings.Contains(e, "extension citext") })) {
			t.Errorf("function status = %+v", s)
		}
	}
	setObjectGrantsCondition(it.database)
	if c := it.database.Status.Conditions; len(c) != 1 || c[0].Reason != ReasonObjectGrantSkipped {
		t.Errorf("conditions = %+v", c)
	}
	it.setGrants()
	it.mustReconcile()
	if len(it.database.Status.ManagedObjectGrants) != 0 {
		t.Fatalf("not revoked: %v", it.ledger())
	}
}

// ogITDefaultPrivileges checks default privileges for a migrator role.
func ogITDefaultPrivileges(it *ogIT) {
	t := it.t
	schema := &it.database.Spec.Schemas[0]
	t.Log("default privileges apply to tables the migrator creates later")
	schema.DefaultPrivileges = []postgresv1alpha1.DefaultPrivilegeSpec{
		{ForRole: ogMigrator, Role: ogApp, Kind: postgresv1alpha1.DefaultPrivilegeTable, Privileges: []string{postgres.PrivilegeSelect}},
		{ForRole: ogMigrator, Role: ogApp, Kind: postgresv1alpha1.DefaultPrivilegeFunction, Privileges: []string{postgres.PrivilegeExecute},
			WithGrantOption: true},
	}
	it.mustReconcile()
	it.as(ogMigrator, `CREATE TABLE app.m1 (id int)`)
	if !it.has("has_table_privilege", "app.m1", "SELECT") {
		t.Fatal("default privileges not applied")
	}
	if n := len(it.database.Status.ManagedDefaultPrivileges); n != 2 {
		t.Fatalf("ledger = %+v", it.database.Status.ManagedDefaultPrivileges)
	}

	t.Log("forRole must be a non-superuser role a Role manages")
	schema.DefaultPrivileges = append(schema.DefaultPrivileges,
		postgresv1alpha1.DefaultPrivilegeSpec{ForRole: ogOutsider, Role: ogApp, Kind: postgresv1alpha1.DefaultPrivilegeTable,
			Privileges: []string{postgres.PrivilegeSelect}},
		postgresv1alpha1.DefaultPrivilegeSpec{ForRole: ogSuper, Role: ogApp, Kind: postgresv1alpha1.DefaultPrivilegeTable,
			Privileges: []string{postgres.PrivilegeSelect}})
	it.wantReason(ReasonDefaultPrivilegeNotAllowed, ogOutsider+" is not managed", ogSuper+" is a superuser")
	if it.query(`SELECT count(*) FROM pg_default_acl WHERE defaclrole IN ('`+ogOutsider+`'::regrole, '`+ogSuper+`'::regrole)`) != "0" {
		t.Fatal("default privileges set for a refused forRole")
	}

	t.Log("removing an entry revokes it")
	defaultACL := func() string {
		return it.query(`SELECT string_agg(defaclobjtype::text || ':' || defaclacl::text, ' ' ORDER BY 1) FROM pg_default_acl`)
	}
	if got := defaultACL(); !strings.Contains(got, "f:") {
		t.Fatalf("pg_default_acl = %s", got)
	}
	schema.DefaultPrivileges = schema.DefaultPrivileges[:1]
	it.mustReconcile()
	if got := defaultACL(); strings.Contains(got, "f:") || !strings.Contains(got, "r:") {
		t.Fatalf("pg_default_acl = %s", got)
	}
	schema.DefaultPrivileges = nil
	it.mustReconcile()
	it.as(ogMigrator, `CREATE TABLE app.m2 (id int)`)
	if it.has("has_table_privilege", "app.m2", "SELECT") || defaultACL() != "" {
		t.Fatal("default privileges not revoked")
	}

	t.Log("restore the table entry for the role cleanup")
	schema.DefaultPrivileges = []postgresv1alpha1.DefaultPrivilegeSpec{{ForRole: ogMigrator, Role: ogApp,
		Kind: postgresv1alpha1.DefaultPrivilegeTable, Privileges: []string{postgres.PrivilegeSelect}}}
	it.mustReconcile()
	it.as(ogMigrator, `CREATE TABLE app.m3 (id int)`)
	if !it.has("has_table_privilege", "app.m3", "SELECT") {
		t.Fatal("default privileges not applied")
	}
}

// ogITRoleCleanup checks that the ledger-based cleanup lets og_app be
// dropped.
func ogITRoleCleanup(it *ogIT, admin *sql.DB) {
	t := it.t
	it.setGrants(postgresv1alpha1.ObjectGrantSpec{Role: ogApp, Kind: postgresv1alpha1.ObjectGrantTable,
		Objects: []string{"t1"}, Privileges: []string{postgres.PrivilegeSelect}, WithGrantOption: true})
	it.mustReconcile()
	if _, err := admin.ExecContext(it.ctx, `DROP ROLE `+ogApp); err == nil {
		t.Fatal("DROP ROLE should fail while og_app holds privileges")
	}
	t.Log("the Database pauses og_app first (its Role is being deleted) and revokes its ledger entries")
	paused := it.checker()
	paused.deleting = map[string]bool{ogApp: true}
	schemas := map[string]bool{ogSchema: true}
	_ = reconcileObjectGrants(it.ctx, it.pg, it.database, schemas, it.version, paused, nil)
	_ = reconcileDefaultPrivileges(it.ctx, it.pg, it.database, schemas, it.version, paused, nil)
	if len(it.database.Status.ManagedObjectGrants) != 0 || len(it.database.Status.ManagedDefaultPrivileges) != 0 {
		t.Fatalf("paused entries not revoked: %v %+v", it.ledger(), it.database.Status.ManagedDefaultPrivileges)
	}
	t.Log("the Role cleanup still revokes what the default privileges gave og_app on m1 and m3")
	cluster := &postgresv1alpha1.Cluster{}
	cluster.Name, cluster.UID = "c", ogtUID
	db := it.database.DeepCopy()
	db.Spec.ClusterRef.Name = cluster.Name
	db.Status.DatabaseName, db.Status.ClusterUID = ogDB, ogtUID
	rec := recordedObjectAccessFrom([]postgresv1alpha1.Database{*db}, cluster, ogApp)[ogDB]
	if rec == nil {
		t.Fatal("nothing recorded for og_app")
	}
	if err := revokeRecordedObjectAccess(it.ctx, it.pg, ogApp, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(it.ctx, `DROP ROLE `+ogApp); err != nil {
		t.Fatalf("DROP ROLE after the cleanup: %v", err)
	}
	if it.query(`SELECT count(*) FROM pg_default_acl`) != "0" {
		t.Fatal("pg_default_acl entry left behind")
	}
	t.Log("a dropped grantee is forgotten")
	it.setGrants()
	it.database.Spec.Schemas[0].DefaultPrivileges = nil
	it.mustReconcile()
	if len(it.database.Status.ManagedObjectGrants) != 0 || len(it.database.Status.ManagedDefaultPrivileges) != 0 {
		t.Fatalf("ledgers not emptied: %+v %+v", it.database.Status.ManagedObjectGrants, it.database.Status.ManagedDefaultPrivileges)
	}
}
