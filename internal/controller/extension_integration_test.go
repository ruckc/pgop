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

// Names used by TestExtensionsIntegration.
const (
	extITDB     = "pgop_ext_test"
	extITOwner  = "ext_owner"
	extITEvil   = "ext_evil"
	extITHstore = "hstore"
	extITDblink = "dblink"
)

// extIT is the state of TestExtensionsIntegration.
type extIT struct {
	t        *testing.T
	ctx      context.Context
	conn     *sql.DB
	pg       *postgres.Client
	version  int
	database *postgresv1alpha1.Database
	policy   *postgresv1alpha1.RolePolicySpec
	checker  *granteeChecker
}

func (it *extIT) exec(q string) {
	it.t.Helper()
	if _, err := it.conn.ExecContext(it.ctx, q); err != nil {
		it.t.Fatalf("%s: %v", q, err)
	}
}

func (it *extIT) query(q string) string {
	it.t.Helper()
	var s sql.NullString
	if err := it.conn.QueryRowContext(it.ctx, q).Scan(&s); err != nil {
		it.t.Fatalf("%s: %v", q, err)
	}
	return s.String
}

// extVersion returns "version@schema" of an installed extension, "" when it
// is not installed.
func (it *extIT) extVersion(name string) string {
	return it.query(`SELECT max(extversion || '@' || extnamespace::regnamespace::text) FROM pg_extension WHERE extname = '` + name + `'`)
}

// reconcile runs the extension and extension grant reconcilers.
func (it *extIT) reconcile() error {
	states, err := reconcileExtensions(it.ctx, it.pg, it.database, it.policy, nil, nil)
	return errors.Join(err, reconcileExtensionGrants(it.ctx, it.pg, it.database, states, it.version, it.checker, nil))
}

func (it *extIT) mustReconcile() {
	it.t.Helper()
	if err := it.reconcile(); err != nil {
		it.t.Fatal(err)
	}
}

func (it *extIT) wantReason(reason string, parts ...string) {
	it.t.Helper()
	err := it.reconcile()
	if got := extReason(err); got != reason {
		it.t.Fatalf("want reason %s, got %v", reason, err)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			it.t.Errorf("%q not in %v", p, err)
		}
	}
}

// spec returns the spec entry of the extension name.
func (it *extIT) spec(name string) *postgresv1alpha1.ExtensionSpec {
	i := slices.IndexFunc(it.database.Spec.Extensions, func(e postgresv1alpha1.ExtensionSpec) bool { return e.Name == name })
	if i < 0 {
		it.t.Fatalf("%s is not in the spec", name)
	}
	return &it.database.Spec.Extensions[i]
}

// TestExtensionsIntegration runs the extension reconcilers against a real
// server with the contrib extensions of the postgres image: trusted ones
// (pg_trgm, hstore, citext, cube, ltree) and untrusted ones (dblink,
// earthdistance). It runs only when PGOP_TEST_PGHOST is set, like
// TestGrantEngineIntegration.
func TestExtensionsIntegration(t *testing.T) {
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
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	admin := raw("postgres")
	roles := []string{extITOwner, extApp, extITEvil}
	cleanup := func() {
		_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+extITDB+` WITH (FORCE)`)
		for _, r := range roles {
			_, _ = admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+r)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	setup := &extIT{t: t, ctx: ctx, conn: admin}
	for _, r := range roles {
		setup.exec(`CREATE ROLE ` + r)
	}
	setup.exec(`CREATE DATABASE ` + extITDB + ` OWNER ` + extITOwner)

	cfg := postgres.ConnectionConfig{Host: host, Port: int32(port), User: user, Password: password}
	adminPG, err := postgres.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adminPG.Close() }()
	cfg.Database = extITDB
	pg, err := postgres.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.Close() }()
	version, err := pg.ServerVersionNum(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policy := &postgresv1alpha1.RolePolicySpec{AllowedExistingRoles: roles}
	it := &extIT{t: t, ctx: ctx, conn: raw(extITDB), pg: pg, version: version, database: &postgresv1alpha1.Database{},
		policy: policy, checker: &granteeChecker{pg: pg, policy: policy}}

	extITPolicyAndSchemas(it, user)
	extITVersions(it)
	extITGrants(it)
	extITRemoval(it)

	t.Log("role cleanup revokes privileges on extension objects")
	it.spec(extCitext).Grants = []postgresv1alpha1.ExtensionGrantSpec{{Role: extApp, Functions: []string{postgres.PrivilegeExecute}}}
	it.mustReconcile()
	dbs, err := adminPG.DatabasesWithObjectPrivileges(ctx, extApp)
	if err != nil || !slices.ContainsFunc(dbs, func(d postgres.DatabaseRef) bool { return d.Name == extITDB }) {
		t.Fatalf("DatabasesWithObjectPrivileges = %+v %v", dbs, err)
	}
	if err := pg.RevokeAllSchemaPrivileges(ctx, extApp); err != nil {
		t.Fatal(err)
	}
	if err := pg.RevokeAllExtensionMemberPrivileges(ctx, extApp); err != nil {
		t.Fatal(err)
	}
	if err := adminPG.DropRole(ctx, extApp); err != nil {
		t.Fatalf("DROP ROLE after the cleanup: %v", err)
	}
}

// extITPolicyAndSchemas checks the policy, cascade and the schema rules.
func extITPolicyAndSchemas(it *extIT, operator string) {
	t := it.t
	t.Log("a trusted extension is installed into public of an owned database; an untrusted one is refused")
	it.database.Spec.Extensions = []postgresv1alpha1.ExtensionSpec{{Name: polTrgm}, {Name: extITDblink}}
	it.wantReason(ReasonExtensionNotAllowed, extITDblink)
	if got := it.extVersion(polTrgm); !strings.HasSuffix(got, "@public") {
		t.Errorf("pg_trgm = %q", got)
	}
	if it.extVersion(extITDblink) != "" {
		t.Fatal("dblink was installed")
	}

	t.Log("allowed, the untrusted extension still may not run in public, which the (non-superuser) owner can write to")
	it.policy.AllowedExtensions = []string{extITDblink, extEarth}
	it.wantReason(ReasonExtensionSchemaNotAllowed, extITOwner)
	if it.extVersion(extITDblink) != "" {
		t.Fatal("dblink was installed into public")
	}

	t.Log("with a schema of its own, created by pgop and owned by the operator, it is installed")
	it.spec(extITDblink).Schema = "dblink_s"
	it.mustReconcile()
	if got := it.query(`SELECT nspowner::regrole::text FROM pg_namespace WHERE nspname = 'dblink_s'`); got != operator {
		t.Errorf("dblink_s owned by %q", got)
	}
	if got := it.extVersion(extITDblink); !strings.HasSuffix(got, "@dblink_s") {
		t.Errorf("dblink = %q", got)
	}

	t.Log("cascade checks the dependency and installs it")
	it.database.Spec.Extensions = append(it.database.Spec.Extensions, postgresv1alpha1.ExtensionSpec{Name: extEarth, Schema: "geo"})
	it.wantReason(ReasonExtensionDependencyMissing, extCube)
	it.spec(extEarth).Cascade = true
	it.mustReconcile()
	if !strings.HasSuffix(it.extVersion(extCube), "@geo") || !strings.HasSuffix(it.extVersion(extEarth), "@geo") {
		t.Errorf("cube = %q, earthdistance = %q", it.extVersion(extCube), it.extVersion(extEarth))
	}

	t.Log("a schema another role can create in is refused")
	it.exec(`CREATE SCHEMA shared AUTHORIZATION ` + extITOwner)
	it.exec(`GRANT CREATE ON SCHEMA shared TO ` + extITEvil)
	it.database.Spec.Extensions = append(it.database.Spec.Extensions, postgresv1alpha1.ExtensionSpec{Name: "ltree", Schema: "shared"})
	it.wantReason(ReasonExtensionSchemaNotAllowed, extITEvil+" can create")
	if it.extVersion("ltree") != "" {
		t.Fatal("ltree was installed into a writable schema")
	}
	it.exec(`REVOKE CREATE ON SCHEMA shared FROM ` + extITEvil)
	it.mustReconcile()
}

// extITVersions installs an old version, updates it and refuses a
// downgrade.
func extITVersions(it *extIT) {
	it.t.Log("versions: install an old one, update, refuse a downgrade")
	it.database.Spec.Extensions = append(it.database.Spec.Extensions, postgresv1alpha1.ExtensionSpec{Name: extITHstore,
		Version: extV14, DropOnRemoval: true})
	it.mustReconcile()
	it.spec(extITHstore).Version = extV18
	it.mustReconcile()
	if got := it.extVersion(extITHstore); got != "1.8@public" {
		it.t.Errorf("hstore = %q", got)
	}
	it.spec(extITHstore).Version = "1.5"
	it.wantReason(ReasonExtensionDowngradeNotAllowed)
	it.spec(extITHstore).Version = "9.9"
	it.wantReason(ReasonExtensionVersionNotAvailable)
	it.spec(extITHstore).Version = extV18
}

// extITGrants grants on an extension's functions and schema and revokes.
func extITGrants(it *extIT) {
	t := it.t
	t.Log("grants: EXECUTE only on SQL functions, never on C functions")
	it.database.Spec.Extensions = append(it.database.Spec.Extensions, postgresv1alpha1.ExtensionSpec{Name: extCitext,
		Grants: []postgresv1alpha1.ExtensionGrantSpec{{Role: extApp, Functions: []string{postgres.PrivilegeExecute}}}})
	it.mustReconcile()
	grantedOn := func(lang string) string {
		return it.query(`SELECT count(*) FROM pg_proc p JOIN pg_depend d ON d.objid = p.oid AND d.classid = 'pg_proc'::regclass
  AND d.deptype = 'e' AND d.refobjid = (SELECT oid FROM pg_extension WHERE extname = 'citext')
  JOIN pg_language l ON l.oid = p.prolang, aclexplode(p.proacl) a
WHERE l.lanname = '` + lang + `' AND a.grantee = 'ext_app'::regrole AND a.privilege_type = 'EXECUTE'`)
	}
	if grantedOn("sql") == "0" || grantedOn("c") != "0" {
		t.Errorf("EXECUTE granted on %s sql and %s C functions", grantedOn("sql"), grantedOn("c"))
	}
	i := slices.IndexFunc(it.database.Status.Extensions, func(s postgresv1alpha1.ExtensionStatus) bool { return s.Name == extCitext })
	if i < 0 || it.database.Status.Extensions[i].SkippedObjects == 0 {
		t.Errorf("no skipped C functions counted: %+v", it.database.Status.Extensions)
	}
	if len(it.database.Status.ManagedExtensionGrants) != 1 {
		t.Errorf("ledger = %+v", it.database.Status.ManagedExtensionGrants)
	}

	t.Log("extension schema grants on the extension's own schema")
	it.spec(extITDblink).Grants = []postgresv1alpha1.ExtensionGrantSpec{{Role: extApp, Schema: []string{postgres.PrivilegeUsage}}}
	it.mustReconcile()
	const usage = `SELECT has_schema_privilege('ext_app', 'dblink_s', 'USAGE')`
	if it.query(usage) != "true" {
		t.Error("USAGE on dblink_s not granted")
	}

	t.Log("removing the grants revokes them")
	it.spec(extITDblink).Grants = nil
	it.spec(extCitext).Grants = nil
	it.mustReconcile()
	if grantedOn("sql") != "0" || it.query(usage) != "false" {
		t.Error("grants not revoked")
	}
	if len(it.database.Status.ManagedExtensionGrants) != 0 {
		t.Errorf("ledger = %+v", it.database.Status.ManagedExtensionGrants)
	}
}

// extITRemoval removes extensions from the spec.
func extITRemoval(it *extIT) {
	t := it.t
	t.Log("removal: kept by default, dropped with dropOnRemoval, never with CASCADE")
	it.exec(`CREATE TABLE keep_hstore (h hstore)`)
	it.database.Spec.Extensions = slices.DeleteFunc(it.database.Spec.Extensions, func(e postgresv1alpha1.ExtensionSpec) bool {
		return e.Name == extITHstore || e.Name == polTrgm
	})
	it.wantReason(ReasonExtensionDropBlocked, extITHstore)
	if it.extVersion(extITHstore) == "" || it.extVersion(polTrgm) == "" {
		t.Fatal("an extension was dropped")
	}
	it.exec(`DROP TABLE keep_hstore`)
	it.mustReconcile()
	if it.extVersion(extITHstore) != "" {
		t.Error("hstore not dropped")
	}
	if it.extVersion(polTrgm) == "" {
		t.Error("pg_trgm (no dropOnRemoval) was dropped")
	}
}

// racingSchemaClient runs before right before each CREATE SCHEMA pgop
// issues: it simulates a role creating the schema between pgop's check and
// its CREATE SCHEMA.
type racingSchemaClient struct {
	*postgres.Client
	before func()
}

func (c racingSchemaClient) CreateSchema(ctx context.Context, name, owner string) error {
	c.before()
	return c.Client.CreateSchema(ctx, name, owner)
}

// TestExtensionFixturesIntegration checks the extension-script hijack and
// the per-version trust checks with the fixture extensions in
// testdata/extensions, which must be installed into the server's extension
// directory, for example:
//
//	docker cp internal/controller/testdata/extensions/. <container>:/usr/share/postgresql/18/extension/
//
// It runs only when PGOP_TEST_PGHOST is set and the fixtures are available.
func TestExtensionFixturesIntegration(t *testing.T) {
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
	const db, tenant = "pgop_extfx_test", "extfx_owner"
	open := func(database string) *sql.DB {
		conn, err := sql.Open("postgres", fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			host, port, user, password, database))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	admin := open("postgres")
	var fixtures int
	if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_available_extensions WHERE name IN ('pgop_evs', 'pgop_evt')`).
		Scan(&fixtures); err != nil {
		t.Fatal(err)
	}
	if fixtures != 2 {
		t.Skip("the fixture extensions of testdata/extensions are not installed on the server")
	}
	cleanup := func() {
		_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+db+` WITH (FORCE)`)
		_, _ = admin.ExecContext(ctx, `DROP ROLE IF EXISTS `+tenant)
	}
	cleanup()
	t.Cleanup(cleanup)
	setup := &extIT{t: t, ctx: ctx, conn: admin}
	setup.exec(`CREATE ROLE ` + tenant)
	setup.exec(`CREATE DATABASE ` + db + ` OWNER ` + tenant)
	pg, err := postgres.NewClient(postgres.ConnectionConfig{Host: host, Port: int32(port), User: user, Password: password, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pg.Close() }()
	it := &extIT{t: t, ctx: ctx, conn: open(db), pg: pg, database: &postgresv1alpha1.Database{},
		policy: &postgresv1alpha1.RolePolicySpec{AllowedExtensions: []string{"pgop_evs"}}}
	reconcileWith := func(c extensionInstallClient) func() error {
		return func() error {
			_, err := reconcileExtensions(ctx, c, it.database, it.policy, nil, nil)
			return err
		}
	}

	t.Log("the database owner creates the control file's schema, with a planted f(integer), right before pgop does")
	raced := false
	client := racingSchemaClient{Client: pg, before: func() {
		if !raced {
			raced = true
			it.exec(`SET ROLE ` + tenant + `; CREATE SCHEMA pgop_evs; ` +
				`CREATE FUNCTION pgop_evs.f(integer) RETURNS text LANGUAGE sql AS 'SELECT ''hijacked as '' || current_user'; RESET ROLE`)
		}
	}}
	it.database.Spec.Extensions = []postgresv1alpha1.ExtensionSpec{{Name: "pgop_evs"}}
	it.wantReasonFrom(reconcileWith(client), ReasonExtensionSchemaNotAllowed, "while pgop was creating it")
	if it.extVersion("pgop_evs") != "" {
		t.Fatal("the extension was installed into the raced schema")
	}

	t.Log("without the race, pgop creates the schema for the operator and the script calls its own f")
	it.exec(`DROP SCHEMA pgop_evs CASCADE`)
	if err := reconcileWith(client)(); err != nil {
		t.Fatal(err)
	}
	if got := it.query(`SELECT result FROM pgop_evs.ran`); got != "extension" {
		t.Errorf("the script called %q", got)
	}
	if got := it.query(`SELECT nspowner::regrole::text FROM pg_namespace WHERE nspname = 'pgop_evs'`); got != user {
		t.Errorf("pgop_evs owned by %q", got)
	}

	t.Log("an install or update through the untrusted 1.1 is refused")
	it.database.Spec.Extensions = []postgresv1alpha1.ExtensionSpec{{Name: "pgop_evt"}}
	it.wantReasonFrom(reconcileWith(pg), ReasonExtensionNotAllowed, "version(s) 1.1")
	it.database.Spec.Extensions[0].Version = "1.0"
	if err := reconcileWith(pg)(); err != nil {
		t.Fatal(err)
	}
	it.database.Spec.Extensions[0].Version = "1.2"
	it.wantReasonFrom(reconcileWith(pg), ReasonExtensionNotAllowed, "version(s) 1.1")
	if got := it.extVersion("pgop_evt"); got != "1.0@public" {
		t.Errorf("pgop_evt = %q", got)
	}
}

// wantReasonFrom runs f and checks the reason and message of its error.
func (it *extIT) wantReasonFrom(f func() error, reason string, parts ...string) {
	it.t.Helper()
	err := f()
	if got := extReason(err); got != reason {
		it.t.Fatalf("want reason %s, got %v", reason, err)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			it.t.Errorf("%q not in %v", p, err)
		}
	}
}
