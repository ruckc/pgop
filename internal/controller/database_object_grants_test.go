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
	"errors"
	"fmt"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Names used by the object grant tests.
const (
	ogtSchema   = "sales"
	ogtDBOwner  = "app_owner"
	ogtDeclared = "schema_owner"
	ogtManaged  = "migrator"
	ogtOutsider = "outsider"
	ogtSuper    = "dba"
	ogtOperator = DefaultOperatorUsername
	ogtMarker   = "marker"
	ogtT1       = "sales.t1"
	ogtT1Select = "sales.t1 SELECT"
	ogtFInt     = "sales.f(integer) EXECUTE"
)

// fakeObjectClient models the objects of schemas and their ACLs, and default
// privileges, on top of fakeGrantClient (roles).
type fakeObjectClient struct {
	*fakeGrantClient
	dbOwner  string
	oids     map[string]int64
	objects  map[string][]*postgres.SchemaObject // schema|kind -> objects
	sigs     map[string]int64                    // "schema.name(args)" -> OID
	sigErr   map[string]bool                     // signatures the server cannot parse
	defaults map[string][]string                 // forRole|schema|kind|grantee -> privileges
	listed   int                                 // ListSchemaObjects calls
}

func newFakeObjectClient() *fakeObjectClient {
	f := &fakeObjectClient{
		fakeGrantClient: &fakeGrantClient{
			superusers: map[string]bool{ogtSuper: true, ogtOperator: true},
			comments:   map[string]string{ogtManaged: ogtMarker, grantTestRole: ogtMarker},
		},
		dbOwner: ogtDBOwner,
		oids: map[string]int64{grantTestRole: 100, ogtDBOwner: 101, ogtDeclared: 102, ogtManaged: 103, ogtOutsider: 104,
			ogtSuper: 105, ogtOperator: 10, grantTestOther: 106},
		objects: map[string][]*postgres.SchemaObject{}, sigs: map[string]int64{}, sigErr: map[string]bool{},
		defaults: map[string][]string{},
	}
	return f
}

// add adds an object of kind owned by owner to the schema sales.
func (f *fakeObjectClient) add(kind postgres.SchemaObjectKind, name, identity, owner string, mod ...func(*postgres.SchemaObject)) {
	o := &postgres.SchemaObject{OID: int64(1000 + len(f.objects[ogtSchema+"|"+string(kind)])), Identity: identity, Name: name,
		Owner: owner, OwnerOID: f.oids[owner], OwnerSuperuser: f.superusers[owner], OwnerIsSessionUser: owner == ogtOperator,
		Language: "sql", SubKind: "r"}
	for _, m := range mod {
		m(o)
	}
	k := ogtSchema + "|" + string(kind)
	f.objects[k] = append(f.objects[k], o)
}

func (f *fakeObjectClient) find(kind postgres.SchemaObjectKind, schema, identity string) *postgres.SchemaObject {
	for _, o := range f.objects[schema+"|"+string(kind)] {
		if o.Identity == identity {
			return o
		}
	}
	return nil
}

func (f *fakeObjectClient) CurrentDatabaseOwner(context.Context) (string, error) {
	return f.dbOwner, nil
}

func (f *fakeObjectClient) RoleOIDs(_ context.Context, names []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, n := range names {
		if oid, ok := f.oids[n]; ok && (f.roles == nil || f.roles[n]) {
			out[n] = oid
		}
	}
	return out, nil
}

func (f *fakeObjectClient) ListSchemaObjects(_ context.Context, schema string, kind postgres.SchemaObjectKind,
	sel postgres.SchemaObjectSelector) ([]postgres.SchemaObject, error) {
	f.listed++
	var out []postgres.SchemaObject
	for _, o := range f.objects[schema+"|"+string(kind)] {
		if sel.All || slices.Contains(sel.Names, o.Name) || slices.Contains(sel.OIDs, o.OID) {
			c := *o
			c.ACL = slices.Clone(o.ACL)
			out = append(out, c)
		}
		if sel.Limit > 0 && len(out) == sel.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeObjectClient) ResolveRoutine(_ context.Context, schema, name, args string) (int64, bool, error) {
	sig := schema + "." + name + "(" + args + ")"
	if f.sigErr[sig] {
		return 0, false, fmt.Errorf("syntax error in %s", sig)
	}
	oid, ok := f.sigs[sig]
	return oid, ok, nil
}

func (f *fakeObjectClient) ResolveSchemaObject(_ context.Context, kind postgres.SchemaObjectKind, schema, identity string) (string, bool, error) {
	if f.find(kind, schema, identity) == nil {
		return "", false, nil
	}
	return identity, true, nil
}

func (f *fakeObjectClient) GrantOnSchemaObject(_ context.Context, kind postgres.SchemaObjectKind, schema, identity string,
	_ int64, grantee string, privileges []string, withGrantOption bool) error {
	if err := f.record(fmt.Sprintf("grant %s on %s %s to %s wgo=%t", strings.Join(privileges, ","),
		strings.ToLower(string(kind)), identity, grantee, withGrantOption)); err != nil {
		return err
	}
	o := f.find(kind, schema, identity)
	if o == nil {
		return postgres.ErrObjectGone
	}
	oid := f.oids[grantee]
	if postgres.IsPublic(grantee) {
		oid = 0
	}
	for _, p := range privileges {
		o.ACL = append(o.ACL, postgres.ACLItem{Grantee: oid, Privilege: p, Grantable: withGrantOption})
	}
	return nil
}

func (f *fakeObjectClient) RevokeOnSchemaObject(_ context.Context, kind postgres.SchemaObjectKind, schema, identity, grantee string,
	privileges []string, mode postgres.RevokeMode) error {
	if err := f.record(fmt.Sprintf("revoke %s on %s %s from %s optionOnly=%t", strings.Join(privileges, ","),
		strings.ToLower(string(kind)), identity, grantee, mode.GrantOptionOnly)); err != nil {
		return err
	}
	o := f.find(kind, schema, identity)
	if o == nil {
		return nil
	}
	oid := f.oids[grantee]
	if postgres.IsPublic(grantee) {
		oid = 0
	}
	o.ACL = slices.DeleteFunc(o.ACL, func(a postgres.ACLItem) bool {
		return a.Grantee == oid && slices.Contains(privileges, a.Privilege) && !mode.GrantOptionOnly
	})
	return nil
}

func defaultKey(t postgres.DefaultPrivilegesTarget, grantee string) string {
	return t.ForRole + "|" + t.Schema + "|" + string(t.Kind) + "|" + grantee
}

func (f *fakeObjectClient) HeldDefaultPrivileges(_ context.Context, t postgres.DefaultPrivilegesTarget, grantee string) ([]string, []string, error) {
	return slices.Clone(f.defaults[defaultKey(t, grantee)]), nil, nil
}

func (f *fakeObjectClient) GrantDefaultPrivileges(_ context.Context, t postgres.DefaultPrivilegesTarget, grantee string,
	privileges []string, withGrantOption bool) error {
	if err := f.record(fmt.Sprintf("default grant %s on %s in %s for %s to %s wgo=%t", strings.Join(privileges, ","),
		strings.ToLower(string(t.Kind)), t.Schema, t.ForRole, grantee, withGrantOption)); err != nil {
		return err
	}
	k := defaultKey(t, grantee)
	f.defaults[k] = union(f.defaults[k], privileges)
	return nil
}

func (f *fakeObjectClient) RevokeDefaultPrivileges(_ context.Context, t postgres.DefaultPrivilegesTarget, grantee string,
	privileges []string, mode postgres.RevokeMode) error {
	if err := f.record(fmt.Sprintf("default revoke %s on %s in %s for %s from %s cascade=%t", strings.Join(privileges, ","),
		strings.ToLower(string(t.Kind)), t.Schema, t.ForRole, grantee, mode.Cascade)); err != nil {
		return err
	}
	k := defaultKey(t, grantee)
	f.defaults[k] = subtract(f.defaults[k], privileges)
	return nil
}

var _ objectGrantClient = (*fakeObjectClient)(nil)

var _ = Describe("Object grants", func() {
	ctx := context.Background()
	schemas := map[string]bool{ogtSchema: true}
	checker := func(f *fakeObjectClient, deleting map[string]bool) *granteeChecker {
		c := allowGrantees(f.fakeGrantClient, deleting, grantTestOther)
		c.managed = managedRoles{ogtManaged: ogtMarker, grantTestRole: ogtMarker}
		return c
	}
	newDB := func(grants ...postgresv1alpha1.ObjectGrantSpec) *postgresv1alpha1.Database {
		return &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Schemas: []postgresv1alpha1.SchemaSpec{
			{Name: ogtSchema, Owner: ogtDeclared, ObjectGrants: grants}}}}
	}
	og := func(kind postgresv1alpha1.ObjectGrantKind, objects []string, privileges ...string) postgresv1alpha1.ObjectGrantSpec {
		return postgresv1alpha1.ObjectGrantSpec{Role: grantTestRole, Kind: kind, Objects: objects, Privileges: privileges}
	}
	all := []string{selectAll}
	reconcile := func(f *fakeObjectClient, db *postgresv1alpha1.Database, version int) error {
		return reconcileObjectGrants(ctx, f, db, schemas, version, checker(f, nil), nil)
	}
	objectsOf := func(db *postgresv1alpha1.Database) []string {
		out := make([]string, 0, len(db.Status.ManagedObjectGrants))
		for _, g := range db.Status.ManagedObjectGrants {
			out = append(out, g.Object+" "+strings.Join(g.Privileges, ","))
		}
		return out
	}
	tables := func(f *fakeObjectClient) {
		f.add(postgres.SchemaTable, "t1", ogtT1, ogtDBOwner)
		f.add(postgres.SchemaTable, "t2", "sales.t2", ogtDeclared)
		f.add(postgres.SchemaTable, "t3", "sales.t3", ogtManaged)
		f.add(postgres.SchemaTable, "out", "sales.out", ogtOutsider)
		f.add(postgres.SchemaTable, "su", "sales.su", ogtSuper)
		f.add(postgres.SchemaTable, "op", "sales.op", ogtOperator)
		f.add(postgres.SchemaTable, "opv", "sales.opv", ogtOperator, func(o *postgres.SchemaObject) { o.SubKind = "v" })
		f.add(postgres.SchemaTable, "extt", "sales.extt", ogtDBOwner, func(o *postgres.SchemaObject) { o.Extension = "pg_trgm" })
	}

	It(`grants "*" only on objects of the trust domain and counts the rest`, func() {
		f := newFakeObjectClient()
		tables(f)
		db := newDB(og(postgresv1alpha1.ObjectGrantTable, all, postgres.PrivilegeSelect, postgres.PrivilegeTrigger))
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())
		Expect(objectsOf(db)).To(Equal([]string{"sales.op SELECT", "sales.t1 SELECT,TRIGGER", "sales.t3 SELECT,TRIGGER"}))
		Expect(f.listed).To(Equal(1))
		Expect(db.Status.ObjectGrants).To(HaveLen(1))
		st := db.Status.ObjectGrants[0]
		Expect(st.Granted).To(BeEquivalentTo(3))
		Expect(st.Skipped).To(BeEquivalentTo(6))
		Expect(st.SkippedExamples).To(HaveLen(maxSkippedExamples))
		Expect(strings.Join(st.SkippedExamples, "\n")).To(And(
			ContainSubstring("sales.extt belongs to the extension pg_trgm"),
			ContainSubstring("sales.op TRIGGER: not granted on a table owned by the superuser pgop_operator"),
			ContainSubstring("sales.opv is a view owned by the superuser"),
			ContainSubstring("sales.out is owned by outsider, which is neither"),
			ContainSubstring("sales.su is owned by the superuser dba")))

		By("setting ObjectGrantsComplete to False without failing the reconcile")
		setObjectGrantsCondition(db)
		Expect(db.Status.Conditions).To(ContainElement(HaveField("Reason", ReasonObjectGrantSkipped)))

		By("doing nothing on the next reconcile")
		f.calls = nil
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())
		Expect(f.calls).To(BeEmpty())
	})

	It("refuses named objects outside the trust domain and reports missing ones", func() {
		f := newFakeObjectClient()
		tables(f)
		db := newDB(og(postgresv1alpha1.ObjectGrantTable, []string{"t1", "t2", "su", "out", "extt", "nope", `x"; DROP TABLE t1; --`},
			postgres.PrivilegeSelect))
		err := reconcile(f, db, pgVersion17)
		Expect(err).To(HaveOccurred())
		ce, ok := errors.AsType[*conditionError](err)
		Expect(ok).To(BeTrue())
		Expect(ce.reason).To(Equal(ReasonObjectGrantSkipped))
		Expect(err.Error()).To(And(ContainSubstring("sales.su is owned by the superuser"), ContainSubstring("sales.out is owned by outsider"),
			ContainSubstring("sales.t2 is owned by schema_owner, which is neither the database owner"),
			ContainSubstring("sales.extt belongs to the extension"), ContainSubstring(`table "nope" does not exist`)))
		Expect(errorReasons(err)).To(ContainElements(ReasonObjectGrantSkipped, ReasonObjectNotFound))
		Expect(objectsOf(db)).To(Equal([]string{ogtT1Select}))
	})

	It("revokes what stops matching and forgets dropped objects", func() {
		f := newFakeObjectClient()
		tables(f)
		db := newDB(og(postgresv1alpha1.ObjectGrantTable, all, postgres.PrivilegeSelect))
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())

		By("dropping t2 and narrowing the selection to t1 and t3")
		f.objects[ogtSchema+"|"+string(postgres.SchemaTable)] = slices.DeleteFunc(f.objects[ogtSchema+"|"+string(postgres.SchemaTable)],
			func(o *postgres.SchemaObject) bool { return o.Name == "t2" })
		db.Spec.Schemas[0].ObjectGrants = []postgresv1alpha1.ObjectGrantSpec{
			og(postgresv1alpha1.ObjectGrantTable, []string{"t1", "t3"}, postgres.PrivilegeSelect)}
		f.calls = nil
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())
		Expect(f.calls).To(Equal([]string{"revoke SELECT on table sales.op from app optionOnly=false"}))
		Expect(objectsOf(db)).To(Equal([]string{ogtT1Select, "sales.t3 SELECT"}))

		By("revoking when an object's owner leaves the trust domain")
		f.find(postgres.SchemaTable, ogtSchema, "sales.t3").Owner = ogtOutsider
		f.calls = nil
		Expect(reconcile(f, db, pgVersion17)).To(HaveOccurred())
		Expect(f.calls).To(Equal([]string{"revoke SELECT on table sales.t3 from app optionOnly=false"}))

		By("revoking everything once the entries are removed")
		db.Spec.Schemas[0].ObjectGrants = nil
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())
		Expect(db.Status.ManagedObjectGrants).To(BeEmpty())
		Expect(db.Status.ObjectGrants).To(BeEmpty())
	})

	It("never records a privilege the grantee already held", func() {
		f := newFakeObjectClient()
		tables(f)
		t1 := f.find(postgres.SchemaTable, ogtSchema, ogtT1)
		t1.ACL = append(t1.ACL, postgres.ACLItem{Grantee: f.oids[grantTestRole], Privilege: postgres.PrivilegeSelect})
		db := newDB(og(postgresv1alpha1.ObjectGrantTable, []string{"t1"}, postgres.PrivilegeSelect, postgres.PrivilegeInsert))
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())
		Expect(objectsOf(db)).To(Equal([]string{"sales.t1 INSERT"}))
		db.Spec.Schemas[0].ObjectGrants = nil
		f.calls = nil
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())
		Expect(f.calls).To(Equal([]string{"revoke INSERT on table sales.t1 from app optionOnly=false"}))
	})

	It("applies the superuser function rule and resolves signatures", func() {
		f := newFakeObjectClient()
		f.add(postgres.SchemaFunction, "f", "sales.f(integer)", ogtDBOwner)
		f.add(postgres.SchemaFunction, "f", "sales.f(text)", ogtDBOwner)
		f.add(postgres.SchemaFunction, "def", "sales.def()", ogtManaged, func(o *postgres.SchemaObject) { o.SecurityDefiner = true })
		f.add(postgres.SchemaFunction, "opdef", "sales.opdef()", ogtOperator, func(o *postgres.SchemaObject) { o.SecurityDefiner = true })
		f.add(postgres.SchemaFunction, "opc", "sales.opc(integer)", ogtOperator, func(o *postgres.SchemaObject) { o.Language = "c" })
		f.add(postgres.SchemaFunction, "opsql", "sales.opsql()", ogtOperator, func(o *postgres.SchemaObject) { o.Language = "plpgsql" })
		fs := f.objects[ogtSchema+"|"+string(postgres.SchemaFunction)]
		f.sigs["sales.f(int4)"] = fs[0].OID
		f.sigErr["sales.f(integer); DROP TABLE t1; --"] = true

		db := newDB(og(postgresv1alpha1.ObjectGrantFunction, []string{"f(int4)"}, postgres.PrivilegeAll))
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())
		Expect(objectsOf(db)).To(Equal([]string{ogtFInt}))

		db.Spec.Schemas[0].ObjectGrants = []postgresv1alpha1.ObjectGrantSpec{
			og(postgresv1alpha1.ObjectGrantFunction, []string{"f(integer); DROP TABLE t1; --)", "g(text)"}, postgres.PrivilegeExecute)}
		err := reconcile(f, db, pgVersion17)
		Expect(errorReasons(err)).To(ConsistOf(ReasonObjectNotFound))
		Expect(strings.Count(err.Error(), "DROP TABLE")).To(Equal(1), "a malformed signature is reported once")

		db.Spec.Schemas[0].ObjectGrants = []postgresv1alpha1.ObjectGrantSpec{og(postgresv1alpha1.ObjectGrantFunction, all, postgres.PrivilegeExecute)}
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())
		Expect(objectsOf(db)).To(Equal([]string{"sales.def() EXECUTE", ogtFInt, "sales.f(text) EXECUTE",
			"sales.opsql() EXECUTE"}))
		Expect(db.Status.ObjectGrants[0].SkippedExamples).To(ConsistOf(
			"sales.opc(integer) is written in c and owned by the superuser pgop_operator",
			"sales.opdef() is a SECURITY DEFINER routine owned by the superuser pgop_operator"))

		By("granting a bare name on every overload")
		db.Spec.Schemas[0].ObjectGrants = []postgresv1alpha1.ObjectGrantSpec{og(postgresv1alpha1.ObjectGrantFunction, []string{"f"}, postgres.PrivilegeExecute)}
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())
		Expect(objectsOf(db)).To(Equal([]string{ogtFInt, "sales.f(text) EXECUTE"}))
	})

	It(`refuses "*" beyond the object cap and keeps the ledger of that group`, func() {
		f := newFakeObjectClient()
		tables(f)
		db := newDB(og(postgresv1alpha1.ObjectGrantTable, all, postgres.PrivilegeSelect))
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())
		before := objectsOf(db)
		for i := range maxObjectsPerKind {
			f.add(postgres.SchemaTable, fmt.Sprintf("n%d", i), fmt.Sprintf("sales.n%d", i), ogtDBOwner)
		}
		f.calls = nil
		err := reconcile(f, db, pgVersion17)
		Expect(errorReasons(err)).To(ConsistOf(ReasonTooManyObjects))
		Expect(f.calls).To(BeEmpty())
		Expect(objectsOf(db)).To(Equal(before))
	})

	It("refuses a change that would overflow the ledger, all or nothing", func() {
		f := newFakeObjectClient()
		for i := range int(objectGrantLedgerLimit) + 1 {
			f.add(postgres.SchemaTable, fmt.Sprintf("n%d", i), fmt.Sprintf("sales.n%d", i), ogtDBOwner)
		}
		db := newDB(og(postgresv1alpha1.ObjectGrantTable, all, postgres.PrivilegeSelect))
		err := reconcile(f, db, pgVersion17)
		Expect(errorReasons(err)).To(ConsistOf(ReasonTooManyGrants))
		Expect(f.calls).To(BeEmpty())
		Expect(db.Status.ManagedObjectGrants).To(BeEmpty())
	})

	It("drops MAINTAIN before PostgreSQL 17 and expands ALL without it", func() {
		f := newFakeObjectClient()
		tables(f)
		db := newDB(og(postgresv1alpha1.ObjectGrantTable, []string{"t1"}, postgres.PrivilegeAll, postgres.PrivilegeMaintain))
		err := reconcile(f, db, pgVersion16)
		Expect(errorReasons(err)).To(ConsistOf(ReasonUnsupportedServerVersion))
		Expect(objectsOf(db)).To(Equal([]string{"sales.t1 DELETE,INSERT,REFERENCES,SELECT,TRIGGER,TRUNCATE,UPDATE"}))
	})

	It("holds back refused, missing and paused grantees and revokes what pgop granted them", func() {
		f := newFakeObjectClient()
		tables(f)
		db := newDB(og(postgresv1alpha1.ObjectGrantTable, []string{"t1"}, postgres.PrivilegeSelect))
		Expect(reconcile(f, db, pgVersion17)).To(Succeed())
		f.calls = nil
		Expect(reconcileObjectGrants(ctx, f, db, schemas, pgVersion17, checker(f, map[string]bool{grantTestRole: true}), nil)).
			To(MatchError(ContainSubstring("paused")))
		Expect(f.calls).To(Equal([]string{"revoke SELECT on table sales.t1 from app optionOnly=false"}))

		g := og(postgresv1alpha1.ObjectGrantTable, []string{"t1"}, postgres.PrivilegeSelect)
		g.Role = ogtOutsider
		db.Spec.Schemas[0].ObjectGrants = []postgresv1alpha1.ObjectGrantSpec{g}
		Expect(errorReasons(reconcile(f, db, pgVersion17))).To(ConsistOf(ReasonGranteeNotAllowed))
	})

	It("does nothing in schemas the Database does not manage", func() {
		f := newFakeObjectClient()
		tables(f)
		db := newDB(og(postgresv1alpha1.ObjectGrantTable, all, postgres.PrivilegeSelect))
		Expect(reconcileObjectGrants(ctx, f, db, map[string]bool{}, pgVersion17, checker(f, nil), nil)).To(Succeed())
		Expect(f.listed).To(BeZero())
		Expect(f.calls).To(BeEmpty())
	})

	It("records the intended grants before granting", func() {
		f := newFakeObjectClient()
		tables(f)
		db := newDB(og(postgresv1alpha1.ObjectGrantTable, []string{"t1"}, postgres.PrivilegeSelect))
		var saved [][]string
		save := func(context.Context) error {
			saved = append(saved, objectsOf(db))
			Expect(f.calls).To(BeEmpty())
			return nil
		}
		Expect(reconcileObjectGrants(ctx, f, db, schemas, pgVersion17, checker(f, nil), save)).To(Succeed())
		Expect(saved).To(Equal([][]string{{ogtT1Select}}))

		By("granting nothing when the record cannot be written")
		db2 := newDB(og(postgresv1alpha1.ObjectGrantTable, []string{"t3"}, postgres.PrivilegeSelect))
		f.calls = nil
		Expect(reconcileObjectGrants(ctx, f, db2, schemas, pgVersion17, checker(f, nil),
			func(context.Context) error { return errors.New("conflict") })).To(HaveOccurred())
		Expect(f.calls).To(BeEmpty())
	})
})

var _ = Describe("Default privileges", func() {
	ctx := context.Background()
	schemas := map[string]bool{ogtSchema: true}
	checker := func(f *fakeObjectClient, deleting map[string]bool) *granteeChecker {
		c := allowGrantees(f.fakeGrantClient, deleting, grantTestOther)
		c.managed = managedRoles{ogtManaged: ogtMarker, grantTestRole: ogtMarker}
		return c
	}
	dp := func(forRole, role string, kind postgresv1alpha1.DefaultPrivilegeKind, privileges ...string) postgresv1alpha1.DefaultPrivilegeSpec {
		return postgresv1alpha1.DefaultPrivilegeSpec{ForRole: forRole, Role: role, Kind: kind, Privileges: privileges}
	}
	newDB := func(d ...postgresv1alpha1.DefaultPrivilegeSpec) *postgresv1alpha1.Database {
		return &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Schemas: []postgresv1alpha1.SchemaSpec{
			{Name: ogtSchema, DefaultPrivileges: d}}}}
	}

	It("sets, records and removes default privileges for a managed forRole", func() {
		f := newFakeObjectClient()
		d := dp(ogtManaged, grantTestRole, postgresv1alpha1.DefaultPrivilegeTable, postgres.PrivilegeSelect)
		d.WithGrantOption = true
		db := newDB(d, dp(ogtManaged, postgres.PublicGrantee, postgresv1alpha1.DefaultPrivilegeFunction, postgres.PrivilegeAll))
		Expect(reconcileDefaultPrivileges(ctx, f, db, schemas, pgVersion17, checker(f, nil), nil)).To(Succeed())
		Expect(f.calls).To(ConsistOf("default grant SELECT on tables in sales for migrator to app wgo=true",
			"default grant EXECUTE on functions in sales for migrator to PUBLIC wgo=false"))
		Expect(db.Status.ManagedDefaultPrivileges).To(HaveLen(2))

		db.Spec.Schemas[0].DefaultPrivileges = db.Spec.Schemas[0].DefaultPrivileges[1:]
		f.calls = nil
		Expect(reconcileDefaultPrivileges(ctx, f, db, schemas, pgVersion17, checker(f, nil), nil)).To(Succeed())
		Expect(f.calls).To(Equal([]string{"default revoke SELECT on tables in sales for migrator from app cascade=true"}))
	})

	It("refuses a forRole that is not a non-superuser managed role", func() {
		f := newFakeObjectClient()
		f.roles = map[string]bool{ogtOutsider: true, ogtSuper: true, ogtManaged: true, grantTestRole: true}
		db := newDB(dp(ogtOutsider, grantTestRole, postgresv1alpha1.DefaultPrivilegeTable, postgres.PrivilegeSelect),
			dp(ogtSuper, grantTestRole, postgresv1alpha1.DefaultPrivilegeTable, postgres.PrivilegeSelect),
			dp(ogtOperator, grantTestRole, postgresv1alpha1.DefaultPrivilegeTable, postgres.PrivilegeSelect),
			dp("later", grantTestRole, postgresv1alpha1.DefaultPrivilegeTable, postgres.PrivilegeSelect))
		err := reconcileDefaultPrivileges(ctx, f, db, schemas, pgVersion17, checker(f, nil), nil)
		Expect(errorReasons(err)).To(ContainElement(ReasonDefaultPrivilegeNotAllowed))
		Expect(err.Error()).To(And(ContainSubstring("outsider is not managed"), ContainSubstring("dba is a superuser"),
			ContainSubstring("pgop_operator is reserved"), ContainSubstring(`forRole "later" does not exist yet`)))
		Expect(f.calls).To(BeEmpty())
	})

	It("pauses and revokes the default privileges of a Role being deleted", func() {
		f := newFakeObjectClient()
		db := newDB(dp(ogtManaged, grantTestRole, postgresv1alpha1.DefaultPrivilegeSequence, postgres.PrivilegeUsage))
		Expect(reconcileDefaultPrivileges(ctx, f, db, schemas, pgVersion17, checker(f, nil), nil)).To(Succeed())
		f.calls = nil
		err := reconcileDefaultPrivileges(ctx, f, db, schemas, pgVersion17, checker(f, map[string]bool{ogtManaged: true}), nil)
		Expect(err).To(MatchError(ContainSubstring("paused")))
		Expect(f.calls).To(Equal([]string{"default revoke USAGE on sequences in sales for migrator from app cascade=false"}))
		Expect(db.Status.ManagedDefaultPrivileges).To(BeEmpty())
	})
})

var _ = Describe("Object grant helpers", func() {
	It("parses routine names", func() {
		type parsed struct {
			name, args string
			sig        bool
		}
		for in, want := range map[string]parsed{
			"f(integer, text)": {"f", "integer, text", true},
			"f()":              {"f", "", true},
			"f":                {"f", "", false},
			"(x)":              {"(x)", "", false},
			`q"uote(int)`:      {`q"uote`, "int", true},
			"f(int":            {"f(int", "", false},
		} {
			name, args, sig := parseRoutineName(in)
			Expect(parsed{name, args, sig}).To(Equal(want), in)
		}
	})

	It("normalizes privileges per kind", func() {
		Expect(normalizeObjectPrivileges(postgres.SchemaSequence, []string{"All"})).To(Equal(
			[]string{postgres.PrivilegeSelect, postgres.PrivilegeUpdate, postgres.PrivilegeUsage}))
		Expect(normalizeObjectPrivileges(postgres.SchemaType, []string{allPrivilegesKeyword})).To(Equal([]string{postgres.PrivilegeUsage}))
		_, err := normalizeObjectPrivileges(postgres.SchemaFunction, []string{postgres.PrivilegeSelect})
		Expect(err).To(HaveOccurred())
		_, err = normalizeObjectPrivileges(postgres.SchemaTable, nil)
		Expect(err).To(HaveOccurred())
	})

	It("round-trips the ledgers through the status", func() {
		obj := []postgresv1alpha1.ManagedObjectGrant{{Schema: ogtSchema, Kind: postgresv1alpha1.ObjectGrantProcedure, Object: "sales.p()",
			Role: grantTestRole, Privileges: []string{postgres.PrivilegeExecute}, GrantOptions: []string{postgres.PrivilegeExecute}}}
		Expect(objectLedgerStatus(objectLedger(obj))).To(Equal(obj))
		def := []postgresv1alpha1.ManagedDefaultPrivilege{{Schema: ogtSchema, ForRole: ogtManaged, Kind: postgresv1alpha1.DefaultPrivilegeType,
			Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeUsage}}}
		Expect(defaultLedgerStatus(defaultLedger(def))).To(Equal(def))
		Expect(defaultLedger(def)[0].key()).NotTo(Equal(defaultTarget(defaultKindInfos[3], ogtSchema, "other", postgres.PublicGrantee).key()))
	})

	It("maps Roles named by object grants and default privileges", func() {
		db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Schemas: []postgresv1alpha1.SchemaSpec{{Name: ogtSchema,
			ObjectGrants:      []postgresv1alpha1.ObjectGrantSpec{{Role: "ro"}},
			DefaultPrivileges: []postgresv1alpha1.DefaultPrivilegeSpec{{ForRole: "mig", Role: "rw"}}}}}}
		for _, name := range []string{"ro", "mig", "rw"} {
			Expect(objectAccessNames(db, name)).To(BeTrue(), name)
		}
		Expect(objectAccessNames(db, "x")).To(BeFalse())
	})

	It("revokes exactly what the ledgers record before a role is dropped", func() {
		f := newFakeObjectClient()
		f.add(postgres.SchemaTable, "t1", ogtT1, ogtDBOwner)
		f.add(postgres.SchemaTable, "m1", "sales.m1", ogtManaged, func(o *postgres.SchemaObject) {
			o.ACL = []postgres.ACLItem{{Grantee: 100, Privilege: postgres.PrivilegeSelect}, {Grantee: 100, Privilege: postgres.PrivilegeInsert}}
		})
		f.add(postgres.SchemaTable, "x1", "sales.x1", ogtDBOwner, func(o *postgres.SchemaObject) {
			o.ACL = []postgres.ACLItem{{Grantee: 100, Privilege: postgres.PrivilegeSelect}}
		})
		rec := &recordedObjectAccess{
			objects: []postgresv1alpha1.ManagedObjectGrant{{Schema: ogtSchema, Kind: postgresv1alpha1.ObjectGrantTable, Object: ogtT1,
				Role: grantTestRole, Privileges: []string{postgres.PrivilegeSelect}, GrantOptions: []string{postgres.PrivilegeUpdate}}},
			defaults: []postgresv1alpha1.ManagedDefaultPrivilege{{Schema: ogtSchema, ForRole: ogtManaged,
				Kind: postgresv1alpha1.DefaultPrivilegeTable, Role: grantTestRole, Privileges: []string{postgres.PrivilegeSelect}}},
		}
		Expect(revokeRecordedObjectAccess(context.Background(), f, grantTestRole, rec)).To(Succeed())
		Expect(f.calls).To(Equal([]string{
			"revoke SELECT on table sales.t1 from app optionOnly=false",
			"revoke UPDATE on table sales.t1 from app optionOnly=true",
			"default revoke SELECT on tables in sales for migrator from app cascade=true",
			"revoke SELECT on table sales.m1 from app optionOnly=false",
		}))
	})
})

// errorReasons returns the condition reasons in err.
func errorReasons(err error) []string {
	var out []string
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if ce, ok := e.(*conditionError); ok {
			out = append(out, ce.reason)
			return
		}
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
			return
		}
		walk(errors.Unwrap(e))
	}
	walk(err)
	return out
}
