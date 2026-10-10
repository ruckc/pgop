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
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/lib/pq"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Names used by the grant tests.
const (
	grantTestRole            = "app"
	grantTestParam           = "myapp.tenant"
	grantTestMixedCase       = "Work_Mem"
	grantTestAdmin           = "admin"
	grantTestSighupParam     = "archive_command"
	grantTestSearchPath      = "search_path"
	grantTestValue           = "64MB"
	grantTestSuperuserParam  = "log_statement"
	grantTestAuditParam      = "pgaudit.log"
	grantTestReplicationRole = "session_replication_role"
	grantTestOther           = "other"
	grantTestTenant          = "acme"
	grantTestPostmasterParam = "shared_buffers"
	grantTestRoleParam       = "role"
	grantTestLeaving         = "leaving"
	grantTestUsage           = "usage"
	grantTestCreate          = "create"
	grantTestLowerPublic     = "public"
	callGrantConnectApp      = "grant CONNECT on app_db to app wgo=false"
	callGrantConnectDB       = "grant CONNECT on db to app wgo=false"
	declaredSchema           = "declared"
	callRevokeConnectA       = "revoke CONNECT on db from a optionOnly=false cascade=false"
	ownedByDBSchema          = "owned_by_db"
	recordedSchema           = "recorded"
	callRevokeConnectPublic  = "revoke CONNECT on app_db from PUBLIC optionOnly=false cascade=false"
)

// aclEntry is what a grantee holds on an object in fakeGrantClient.
type aclEntry struct {
	privileges, grantable []string
}

// fakeGrantClient records statements issued for database, schema and
// parameter grants, PUBLIC's privileges and database settings, and keeps a
// model of the ACLs those statements change.
type fakeGrantClient struct {
	version int
	roles   map[string]bool // existing roles; nil means every role exists
	// superusers lists existing roles that are superusers; comments holds
	// their COMMENT ON ROLE (ownership markers).
	superusers map[string]bool
	comments   map[string]string
	// schemas lists existing schemas; nil means every schema exists.
	schemas map[string]bool
	// acl maps aclKey(obj, grantee) to what the grantee holds from the
	// object's owner.
	acl map[string]*aclEntry
	// dependents maps aclKeys to the privileges their grantee passed on: a
	// REVOKE without CASCADE naming one of them fails with "dependent
	// privileges exist".
	dependents map[string][]string
	// elsewhere lists "aclKey|privilege" held from a grantor other than the
	// owner (REVOKE as the owner does not remove them).
	elsewhere map[string]bool
	// contexts maps parameter names to their pg_settings context; missing
	// names are unknown (custom placeholders).
	contexts map[string]string
	failOn   string // substring of a recorded call that fails
	calls    []string
}

func aclKey(obj postgres.PrivilegeObject, grantee string) string {
	return string(obj.Kind) + " " + obj.Name + " " + postgres.CanonicalGrantee(grantee)
}

// hold makes grantee hold privileges (and, with grantable, their grant
// option) on obj, as if granted outside pgop or by PostgreSQL's defaults.
func (f *fakeGrantClient) hold(obj postgres.PrivilegeObject, grantee string, grantable bool, privileges ...string) {
	if f.acl == nil {
		f.acl = map[string]*aclEntry{}
	}
	e := f.acl[aclKey(obj, grantee)]
	if e == nil {
		e = &aclEntry{}
		f.acl[aclKey(obj, grantee)] = e
	}
	e.privileges = union(e.privileges, privileges)
	if grantable {
		e.grantable = union(e.grantable, privileges)
	}
}

func (f *fakeGrantClient) record(call string) error {
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.Contains(call, f.failOn) {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeGrantClient) ServerVersionNum(context.Context) (int, error) { return f.version, nil }

func (f *fakeGrantClient) RoleExists(_ context.Context, name string) (bool, error) {
	return f.roles == nil || f.roles[name], nil
}

func (f *fakeGrantClient) LookupRole(ctx context.Context, name string) (*postgres.ReachableRole, error) {
	if exists, _ := f.RoleExists(ctx, name); !exists {
		return nil, nil
	}
	return &postgres.ReachableRole{Name: name, Superuser: f.superusers[name], Comment: f.comments[name]}, nil
}

func (f *fakeGrantClient) SchemaExists(_ context.Context, name string) (bool, error) {
	return f.schemas == nil || f.schemas[name], nil
}

func (f *fakeGrantClient) HeldPrivileges(ctx context.Context, obj postgres.PrivilegeObject, grantee string) ([]string, []string, bool, error) {
	if obj.Kind == postgres.ObjectSchema {
		if exists, _ := f.SchemaExists(ctx, obj.Name); !exists {
			return nil, nil, false, nil
		}
	}
	e := f.acl[aclKey(obj, grantee)]
	if e == nil {
		return nil, nil, true, nil
	}
	return slices.Clone(e.privileges), slices.Clone(e.grantable), true, nil
}

func (f *fakeGrantClient) HeldFromAnyGrantor(ctx context.Context, obj postgres.PrivilegeObject, grantee, privilege string) (bool, error) {
	if f.elsewhere[aclKey(obj, grantee)+"|"+privilege] {
		return true, nil
	}
	held, _, _, err := f.HeldPrivileges(ctx, obj, grantee)
	return slices.Contains(held, privilege), err
}

// objectLabel renders obj the way the recorded calls name it: a database by
// its name, other kinds prefixed with their kind.
func objectLabel(obj postgres.PrivilegeObject) string {
	if obj.Kind == postgres.ObjectDatabase {
		return obj.Name
	}
	return strings.ToLower(string(obj.Kind)) + " " + obj.Name
}

func (f *fakeGrantClient) GrantPrivileges(_ context.Context, obj postgres.PrivilegeObject, grantee string, privs []string, wgo bool) error {
	if err := f.record(fmt.Sprintf("grant %s on %s to %s wgo=%t", strings.Join(privs, ","), objectLabel(obj), grantee, wgo)); err != nil {
		return err
	}
	f.hold(obj, grantee, wgo, privs...)
	return nil
}

func (f *fakeGrantClient) RevokePrivileges(_ context.Context, obj postgres.PrivilegeObject, grantee string, privs []string, mode postgres.RevokeMode) error {
	if err := f.record(fmt.Sprintf("revoke %s on %s from %s optionOnly=%t cascade=%t",
		strings.Join(privs, ","), objectLabel(obj), grantee, mode.GrantOptionOnly, mode.Cascade)); err != nil {
		return err
	}
	k := aclKey(obj, grantee)
	if len(intersect(f.dependents[k], privs)) > 0 && !mode.Cascade {
		return &pq.Error{Code: "2BP01", Message: "dependent privileges exist"}
	}
	if e := f.acl[k]; e != nil {
		e.grantable = subtract(e.grantable, privs)
		if !mode.GrantOptionOnly {
			e.privileges = subtract(e.privileges, privs)
		}
	}
	return nil
}

func (f *fakeGrantClient) ParameterContext(_ context.Context, name string) (string, bool, error) {
	c, ok := f.contexts[name]
	return c, ok, nil
}

func (f *fakeGrantClient) SetDatabaseParameter(_ context.Context, db, name, value string) error {
	return f.record(fmt.Sprintf("set %s on %s to %s", name, db, value))
}

func (f *fakeGrantClient) ResetDatabaseParameter(_ context.Context, db, name string) error {
	return f.record(fmt.Sprintf("reset %s on %s", name, db))
}

// Objects used by the tests.
var (
	testDBObj     = postgres.PrivilegeObject{Kind: postgres.ObjectDatabase, Name: "app_db"}
	testSchemaObj = postgres.PrivilegeObject{Kind: postgres.ObjectSchema, Name: grantTestRole}
	testPublicObj = postgres.PrivilegeObject{Kind: postgres.ObjectSchema, Name: publicSchemaName}
)

// tgt is a database-grant target used by the engine tests.
func tgt(grantee string) grantTarget {
	return grantTarget{Kind: postgres.ObjectDatabase, Name: "db", Grantee: grantee}
}

// allowGrantees returns a grantee checker for f whose Cluster lists roles in
// rolePolicy.allowedExistingRoles; deleting lists Roles being deleted.
func allowGrantees(f *fakeGrantClient, deleting map[string]bool, roles ...string) *granteeChecker {
	return &granteeChecker{
		pg:       f,
		policy:   &postgresv1alpha1.RolePolicySpec{AllowedExistingRoles: roles},
		deleting: deleting,
	}
}

// testChecker allows every grantee the database grant tests use.
func testChecker(f *fakeGrantClient, deleting map[string]bool) *granteeChecker {
	return allowGrantees(f, deleting, grantTestRole, grantTestAdmin, grantTestOther, grantTestLeaving, "old", "later")
}

var _ = Describe("Privilege grants", func() {
	type pg = privilegeGrant
	ctx := context.Background()
	connect := []string{postgres.PrivilegeConnect}
	usage := []string{postgres.PrivilegeUsage}

	Describe("applyPrivilegeGrants", func() {
		It("grants what is missing and records exactly that", func() {
			f := &fakeGrantClient{}
			f.hold(postgres.PrivilegeObject{Kind: postgres.ObjectDatabase, Name: "db"}, grantTestRole, false, postgres.PrivilegeConnect)
			desired := []pg{desiredGrant(tgt(grantTestRole), []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}, false)}
			ledger, err := applyPrivilegeGrants(ctx, desired, nil, executorOps(f, nil), 0, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{"grant CREATE on db to app wgo=false"}))
			Expect(ledger).To(Equal([]pg{{Target: tgt(grantTestRole), Privileges: []string{postgres.PrivilegeCreate}}}))

			By("doing nothing once everything is held")
			f.calls = nil
			ledger, err = applyPrivilegeGrants(ctx, desired, ledger, executorOps(f, nil), 0, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(BeEmpty())
			Expect(ledger).To(HaveLen(1))
		})

		It("records only the grant option when the privilege was held without it", func() {
			f := &fakeGrantClient{}
			f.hold(postgres.PrivilegeObject{Kind: postgres.ObjectDatabase, Name: "db"}, grantTestRole, false, postgres.PrivilegeConnect)
			ledger, err := applyPrivilegeGrants(ctx, []pg{desiredGrant(tgt(grantTestRole), connect, true)}, nil, executorOps(f, nil), 0, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(ledger).To(Equal([]pg{{Target: tgt(grantTestRole), GrantOptions: connect}}))

			By("revoking only the grant option once the privilege leaves the spec")
			f.calls = nil
			ledger, err = applyPrivilegeGrants(ctx, nil, ledger, executorOps(f, nil), 0, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{"revoke CONNECT on db from app optionOnly=true cascade=true"}))
			Expect(ledger).To(BeNil())
			held, _, _, _ := f.HeldPrivileges(ctx, postgres.PrivilegeObject{Kind: postgres.ObjectDatabase, Name: "db"}, grantTestRole)
			Expect(held).To(Equal(connect), "the pre-existing privilege stays")
		})

		It("never revokes privileges it did not add", func() {
			f := &fakeGrantClient{}
			f.hold(postgres.PrivilegeObject{Kind: postgres.ObjectDatabase, Name: "db"}, postgres.PublicGrantee, false, postgres.PrivilegeConnect)
			ledger, err := applyPrivilegeGrants(ctx, []pg{desiredGrant(tgt(postgres.PublicGrantee), connect, false)}, nil, executorOps(f, nil), 0, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(ledger).To(BeNil())
			ledger, err = applyPrivilegeGrants(ctx, nil, ledger, executorOps(f, nil), 0, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(ledger).To(BeNil())
			Expect(f.calls).To(BeEmpty())
		})

		It("revokes what it added, cascading where it added the grant option", func() {
			f := &fakeGrantClient{}
			managed := []pg{
				{Target: tgt("a"), Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}, GrantOptions: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}},
				{Target: tgt("b"), Privileges: connect, GrantOptions: connect},
				{Target: tgt("c"), Privileges: connect},
			}
			f.hold(postgres.PrivilegeObject{Kind: postgres.ObjectDatabase, Name: "db"}, "a", true, postgres.PrivilegeConnect, postgres.PrivilegeCreate)
			ledger, err := applyPrivilegeGrants(ctx, []pg{desiredGrant(tgt("a"), connect, false)}, managed, executorOps(f, nil), 0, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{
				"revoke CONNECT on db from a optionOnly=true cascade=true", // CONNECT stays, its grant option goes
				"revoke CREATE on db from a optionOnly=false cascade=true",
				"revoke CONNECT on db from b optionOnly=false cascade=true",
				"revoke CONNECT on db from c optionOnly=false cascade=false",
			}))
			Expect(ledger).To(Equal([]pg{{Target: tgt("a"), Privileges: connect}}))
		})

		It("restores a privilege it added that was revoked outside pgop", func() {
			f := &fakeGrantClient{}
			managed := []pg{{Target: tgt(grantTestRole), Privileges: connect}}
			ledger, err := applyPrivilegeGrants(ctx, []pg{desiredGrant(tgt(grantTestRole), connect, false)}, managed, executorOps(f, nil), 0, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{callGrantConnectDB}))
			Expect(ledger).To(Equal(managed))
		})

		It("skips a plain revoke blocked by dependents it did not enable, and keeps going", func() {
			f := &fakeGrantClient{dependents: map[string][]string{
				aclKey(postgres.PrivilegeObject{Kind: postgres.ObjectDatabase, Name: "db"}, "a"): connect,
			}}
			managed := []pg{{Target: tgt("a"), Privileges: connect}, {Target: tgt("b"), Privileges: connect}}
			ledger, err := applyPrivilegeGrants(ctx, nil, managed, executorOps(f, nil), 0, nil)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
			Expect(ce.reason).To(Equal(ReasonRevokeSkipped))
			Expect(err.Error()).To(ContainSubstring(`CONNECT on database "db" to a`))
			Expect(f.calls).To(Equal([]string{
				callRevokeConnectA,
				callRevokeConnectA, // retried alone
				"revoke CONNECT on db from b optionOnly=false cascade=false",
			}))
			Expect(ledger).To(BeNil(), "the skipped revoke leaves the ledger, so it does not fail forever")
		})

		It("revokes the privileges without dependents when one of a REVOKE's privileges has them", func() {
			obj := postgres.PrivilegeObject{Kind: postgres.ObjectDatabase, Name: "db"}
			f := &fakeGrantClient{dependents: map[string][]string{aclKey(obj, "a"): connect}}
			f.hold(obj, "a", false, postgres.PrivilegeConnect, postgres.PrivilegeCreate)
			managed := []pg{{Target: tgt("a"), Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}}}
			ledger, err := applyPrivilegeGrants(ctx, nil, managed, executorOps(f, nil), 0, nil)
			Expect(err).To(MatchError(ContainSubstring(`CONNECT on database "db" to a`)))
			Expect(err.Error()).NotTo(ContainSubstring("CREATE"))
			Expect(f.calls).To(Equal([]string{
				"revoke CONNECT,CREATE on db from a optionOnly=false cascade=false",
				callRevokeConnectA,
				"revoke CREATE on db from a optionOnly=false cascade=false",
			}))
			held, _, _, _ := f.HeldPrivileges(ctx, obj, "a")
			Expect(held).To(Equal(connect), "CREATE is revoked, CONNECT (with dependents) is left")
			Expect(ledger).To(BeNil())
		})

		It("records what it is about to grant before granting, and grants nothing when that fails", func() {
			f := &fakeGrantClient{}
			var persisted []pg
			persist := func(_ context.Context, ledger []pg) error {
				Expect(f.calls).To(BeEmpty(), "the ledger is written before any GRANT")
				persisted = ledger
				return nil
			}
			desired := []pg{desiredGrant(tgt(grantTestRole), connect, false)}
			_, err := applyPrivilegeGrants(ctx, desired, nil, executorOps(f, nil), 0, persist)
			Expect(err).NotTo(HaveOccurred())
			Expect(persisted).To(Equal([]pg{{Target: tgt(grantTestRole), Privileges: connect}}))

			f2 := &fakeGrantClient{}
			ledger, err := applyPrivilegeGrants(ctx, desired, nil, executorOps(f2, nil), 0,
				func(context.Context, []pg) error { return errors.New("conflict") })
			Expect(err).To(MatchError(ContainSubstring("nothing was granted")))
			Expect(f2.calls).To(BeEmpty())
			Expect(ledger).To(BeNil())
		})

		It("keeps going after a failing statement and keeps the failed entry tracked", func() {
			f := &fakeGrantClient{failOn: "from a"}
			managed := []pg{{Target: tgt("a"), Privileges: connect}, {Target: tgt("b"), Privileges: connect}}
			ledger, err := applyPrivilegeGrants(ctx, []pg{desiredGrant(tgt("c"), connect, false)}, managed, executorOps(f, nil), 0, nil)
			Expect(err).To(MatchError(ContainSubstring("boom")))
			Expect(f.calls).To(Equal([]string{
				"grant CONNECT on db to c wgo=false",
				callRevokeConnectA,
				"revoke CONNECT on db from b optionOnly=false cascade=false",
			}))
			Expect(ledger).To(Equal([]pg{{Target: tgt("a"), Privileges: connect}, {Target: tgt("c"), Privileges: connect}}))
		})

		It("refuses to grow the ledger past its limit without changing anything", func() {
			f := &fakeGrantClient{}
			managed := []pg{{Target: tgt("a"), Privileges: connect}, {Target: tgt("b"), Privileges: connect}}
			desired := []pg{desiredGrant(tgt("c"), connect, false), desiredGrant(tgt("d"), connect, false)}
			ledger, err := applyPrivilegeGrants(ctx, desired, managed, executorOps(f, nil), 3, nil)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
			Expect(ce.reason).To(Equal(ReasonTooManyGrants))
			Expect(f.calls).To(BeEmpty())
			Expect(ledger).To(Equal(managed))

			By("applying once the union fits")
			ledger, err = applyPrivilegeGrants(ctx, desired, managed, executorOps(f, nil), 4, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(ledger).To(HaveLen(2))
		})
	})

	Describe("reconcileDatabaseGrants", func() {
		newDB := func(grants []postgresv1alpha1.DatabaseGrantSpec, managed ...postgresv1alpha1.ManagedDatabaseGrant) *postgresv1alpha1.Database {
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: grants}}
			db.Status.ManagedGrants = managed
			return db
		}

		It("is a no-op without grants", func() {
			f := &fakeGrantClient{}
			db := newDB(nil)
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedGrants).To(BeNil())
		})

		It("grants normalized privileges and records them", func() {
			f := &fakeGrantClient{}
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeTemp}},
				{Role: grantTestAdmin, Privileges: []string{postgres.PrivilegeAll}, WithGrantOption: true},
			})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), nil)).To(Succeed())
			all := []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate, postgres.PrivilegeTemporary}
			Expect(f.calls).To(Equal([]string{
				"grant CONNECT,TEMPORARY on app_db to app wgo=false",
				"grant CONNECT,CREATE,TEMPORARY on app_db to admin wgo=true",
			}))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{
				{Role: grantTestAdmin, Privileges: all, GrantOptions: all},
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeTemporary}},
			}))
		})

		It("revokes removed grants and privileges it added", func() {
			f := &fakeGrantClient{}
			f.hold(testDBObj, grantTestRole, true, postgres.PrivilegeConnect, postgres.PrivilegeCreate)
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{{Role: grantTestRole, Privileges: connect}},
				postgresv1alpha1.ManagedDatabaseGrant{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate},
					GrantOptions: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}},
				postgresv1alpha1.ManagedDatabaseGrant{Role: "old", Privileges: connect})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				"revoke CONNECT on app_db from app optionOnly=true cascade=true",
				"revoke CREATE on app_db from app optionOnly=false cascade=true",
				"revoke CONNECT on app_db from old optionOnly=false cascade=false",
			}))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{
				{Role: grantTestRole, Privileges: connect},
			}))
		})

		It("keeps PostgreSQL's default PUBLIC privileges when a declared PUBLIC grant is removed", func() {
			f := &fakeGrantClient{}
			f.hold(testDBObj, postgres.PublicGrantee, false, postgres.PrivilegeConnect, postgres.PrivilegeTemporary)
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{{Role: postgres.PublicGrantee, Privileges: connect}})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedGrants).To(BeNil())
			db.Spec.Grants = nil
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
		})

		It("reports a grantee that does not exist, keeping the others", func() {
			f := &fakeGrantClient{roles: map[string]bool{grantTestRole: true}}
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: "later", Privileges: connect},
				{Role: grantTestRole, Privileges: connect},
			})
			err := reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), nil)
			Expect(err).To(MatchError(ContainSubstring(`PostgreSQL role "later" does not exist yet`)))
			Expect(f.calls).To(Equal([]string{callGrantConnectApp}))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{{Role: grantTestRole, Privileges: connect}}))
		})

		It("forgets a managed grant whose role was dropped without revoking", func() {
			f := &fakeGrantClient{roles: map[string]bool{}}
			db := newDB(nil, postgresv1alpha1.ManagedDatabaseGrant{Role: "dropped", Privileges: connect})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedGrants).To(BeNil())
		})

		It("keeps a pending revoke tracked when it fails", func() {
			f := &fakeGrantClient{failOn: "from old"}
			db := newDB(nil, postgresv1alpha1.ManagedDatabaseGrant{Role: "old", Privileges: connect})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), nil)).NotTo(Succeed())
			Expect(db.Status.ManagedGrants).To(HaveLen(1))
		})

		It("rejects privileges outside the allow-list before issuing SQL", func() {
			f := &fakeGrantClient{}
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: []string{"CONNECT; DROP DATABASE app_db"}},
			})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), nil)).To(MatchError(ContainSubstring("invalid database privilege")))
			Expect(f.calls).To(BeEmpty())
		})

		It("pauses grants to roles that are being deleted", func() {
			f := &fakeGrantClient{}
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: connect},
				{Role: grantTestLeaving, Privileges: connect},
			}, postgresv1alpha1.ManagedDatabaseGrant{Role: grantTestLeaving, Privileges: connect})
			err := reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, map[string]bool{grantTestLeaving: true}), nil)
			Expect(err).To(MatchError(ContainSubstring("grants to leaving are paused")))
			Expect(f.calls).To(Equal([]string{
				callGrantConnectApp,
				"revoke CONNECT on app_db from leaving optionOnly=false cascade=false",
			}))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{
				{Role: grantTestRole, Privileges: connect},
			}))
		})
	})

	Describe("ledger durability and migration", func() {
		It("revokes a grant whose status write after the GRANT was lost", func() {
			f := &fakeGrantClient{}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: []postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: connect},
			}}}
			var persisted []postgresv1alpha1.ManagedDatabaseGrant
			save := func(context.Context) error {
				persisted = slices.Clone(db.Status.ManagedGrants)
				return nil
			}
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), save)).To(Succeed())
			Expect(f.calls).To(Equal([]string{callGrantConnectApp}))

			By("losing the status write that follows the GRANT (a conflict, a restart)")
			db.Status.ManagedGrants = persisted // all that reached the API server
			Expect(persisted).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{{Role: grantTestRole, Privileges: connect}}))

			By("removing the grant from the spec")
			db.Spec.Grants = nil
			f.calls = nil
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(Equal([]string{"revoke CONNECT on app_db from app optionOnly=false cascade=false"}))
			held, _, _, _ := f.HeldPrivileges(ctx, testDBObj, grantTestRole)
			Expect(held).To(BeEmpty())
		})

		It("grants nothing when the intended additions cannot be recorded", func() {
			f := &fakeGrantClient{}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: []postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: connect},
			}}}
			err := reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), func(context.Context) error {
				return errors.New("the object has been modified")
			})
			Expect(err).To(MatchError(ContainSubstring("nothing was granted")))
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedGrants).To(BeNil())
		})

		It("reads a v0.15 ledger's withGrantOption as grant options and stops writing it", func() {
			f := &fakeGrantClient{}
			f.hold(testDBObj, grantTestRole, true, postgres.PrivilegeConnect, postgres.PrivilegeCreate)
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: []postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: connect}, // the grant option is turned off, CREATE removed
			}}}
			db.Status.ManagedGrants = []postgresv1alpha1.ManagedDatabaseGrant{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}, WithGrantOption: true},
			}
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				"revoke CONNECT on app_db from app optionOnly=true cascade=true",
				"revoke CREATE on app_db from app optionOnly=false cascade=true",
			}))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{{Role: grantTestRole, Privileges: connect}}))

			By("reading old schema and parameter ledgers the same way")
			Expect(schemaLedger([]postgresv1alpha1.ManagedSchemaGrant{{Schema: grantTestRole, Role: grantTestRole, Privileges: usage, WithGrantOption: true}})).
				To(Equal([]pg{{Target: schemaTarget(grantTestRole, grantTestRole), Privileges: usage, GrantOptions: usage}}))
			set := []string{postgres.PrivilegeSet}
			Expect(parameterLedger(grantTestRole, []postgresv1alpha1.ManagedParameterGrant{{Parameter: testWorkMem, Privileges: set, WithGrantOption: true}})).
				To(Equal([]pg{{Target: parameterTarget(testWorkMem, grantTestRole), Privileges: set, GrantOptions: set}}))
		})

		It("merges ledgers entry by entry, never dropping one", func() {
			ours := postgresv1alpha1.DatabaseStatus{
				ManagedGrants:           []postgresv1alpha1.ManagedDatabaseGrant{{Role: grantTestRole, Privileges: connect}},
				RevokedPublicPrivileges: []postgresv1alpha1.PublicPrivilege{postgresv1alpha1.PublicPrivilegeTemporary},
			}
			theirs := postgresv1alpha1.DatabaseStatus{
				ManagedGrants: []postgresv1alpha1.ManagedDatabaseGrant{
					{Role: grantTestRole, Privileges: []string{postgres.PrivilegeCreate}, WithGrantOption: true},
					{Role: grantTestOther, Privileges: connect},
				},
				ManagedSchemaGrants:     []postgresv1alpha1.ManagedSchemaGrant{{Schema: grantTestRole, Role: grantTestRole, Privileges: usage}},
				RevokedPublicPrivileges: []postgresv1alpha1.PublicPrivilege{postgresv1alpha1.PublicPrivilegeConnect},
			}
			mergeDatabaseLedgers(&ours, &theirs)
			Expect(ours.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}, GrantOptions: []string{postgres.PrivilegeCreate}},
				{Role: grantTestOther, Privileges: connect},
			}))
			Expect(ours.ManagedSchemaGrants).To(HaveLen(1))
			Expect(ours.RevokedPublicPrivileges).To(Equal([]postgresv1alpha1.PublicPrivilege{
				postgresv1alpha1.PublicPrivilegeConnect, postgresv1alpha1.PublicPrivilegeTemporary}))
		})
	})

	Describe("grantee policy", func() {
		It("grants to PUBLIC with the bare keyword and records it canonically", func() {
			f := &fakeGrantClient{roles: map[string]bool{}}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: []postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestLowerPublic, Privileges: []string{postgres.PrivilegeCreate}},
			}}}
			// No role lookup is needed for PUBLIC, and no policy allows it.
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", &granteeChecker{pg: f}, nil)).To(Succeed())
			Expect(f.calls).To(Equal([]string{"grant CREATE on app_db to PUBLIC wgo=false"}))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{
				{Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeCreate}},
			}))

			By("revoking it from PUBLIC once removed")
			db.Spec.Grants = nil
			f.calls = nil
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", &granteeChecker{pg: f}, nil)).To(Succeed())
			Expect(f.calls).To(Equal([]string{"revoke CREATE on app_db from PUBLIC optionOnly=false cascade=false"}))
			Expect(db.Status.ManagedGrants).To(BeNil())
		})

		It("rejects PUBLIC listed twice and the grant option for PUBLIC", func() {
			_, err := desiredDatabaseGrants([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: postgres.PublicGrantee, Privileges: connect},
				{Role: grantTestLowerPublic, Privileges: []string{postgres.PrivilegeTemp}},
			}, "d")
			Expect(err).To(MatchError(ContainSubstring("listed more than once")))
			_, err = desiredDatabaseGrants([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: postgres.PublicGrantee, Privileges: connect, WithGrantOption: true},
			}, "d")
			Expect(err).To(MatchError(ContainSubstring("grant option cannot be granted to PUBLIC")))
		})

		It("refuses grantees outside the policy, applies the rest and revokes managed refused grants", func() {
			managedMarker := "pgop:v2:Role/app:x"
			f := &fakeGrantClient{superusers: map[string]bool{polDBA: true}, comments: map[string]string{grantTestRole: managedMarker}}
			checker := &granteeChecker{
				pg:      f,
				policy:  &postgresv1alpha1.RolePolicySpec{AllowedExistingRoles: []string{"legacy", polDBA}},
				managed: managedRoles{grantTestRole: managedMarker, "forged": "pgop:v2:Role/forged:y"},
			}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: []postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: connect},  // managed by a Role
				{Role: "legacy", Privileges: connect},       // allowlisted
				{Role: polDBA, Privileges: connect},         // allowlisted but superuser
				{Role: "forged", Privileges: connect},       // marker does not match
				{Role: grantTestOther, Privileges: connect}, // unmanaged
				{Role: bootstrapRoleName, Privileges: connect},
				{Role: "pgop_replicator", Privileges: connect},
				{Role: "pg_monitor", Privileges: connect},
			}}}
			// A grant pgop made earlier to a grantee that is no longer allowed.
			db.Status.ManagedGrants = []postgresv1alpha1.ManagedDatabaseGrant{{Role: grantTestOther, Privileges: connect}}
			err := reconcileDatabaseGrants(ctx, f, db, "app_db", checker, nil)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
			Expect(ce.reason).To(Equal(ReasonGranteeNotAllowed))
			Expect(err.Error()).To(SatisfyAll(
				ContainSubstring("dba is a superuser"),
				ContainSubstring("forged is not managed by a Role of this Cluster"),
				ContainSubstring("other is not managed"),
				ContainSubstring("postgres is reserved"),
				ContainSubstring("pgop_replicator is reserved for the operator"),
				ContainSubstring("pg_monitor is a predefined role"),
			))
			Expect(f.calls).To(Equal([]string{
				callGrantConnectApp,
				"grant CONNECT on app_db to legacy wgo=false",
				"revoke CONNECT on app_db from other optionOnly=false cascade=false",
			}))
			Expect(db.Status.ManagedGrants).To(HaveLen(2))
		})

		It("applies the same policy to schema grants", func() {
			f := &fakeGrantClient{}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Schemas: []postgresv1alpha1.SchemaSpec{{
				Name: grantTestRole,
				Grants: []postgresv1alpha1.GrantSpec{
					{Role: grantTestRole, Privileges: []string{grantTestUsage}},
					{Role: grantTestOther, Privileges: []string{grantTestUsage}},
				},
			}}}}
			err := reconcileSchemaGrants(ctx, f, db, setOf([]string{grantTestRole}), allowGrantees(f, nil, grantTestRole), nil)
			Expect(err).To(MatchError(ContainSubstring("schemas[app].grants: other is not managed")))
			Expect(f.calls).To(Equal([]string{"grant USAGE on schema app to app wgo=false"}))
		})
	})

	Describe("reconcileSchemaGrants", func() {
		newDB := func(schemas ...postgresv1alpha1.SchemaSpec) *postgresv1alpha1.Database {
			return &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Schemas: schemas}}
		}
		managedApp := setOf([]string{grantTestRole, publicSchemaName})

		It("grants normalized schema privileges and records them", func() {
			f := &fakeGrantClient{}
			db := newDB(postgresv1alpha1.SchemaSpec{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
				{Role: grantTestRole, Privileges: []string{"ALL PRIVILEGES"}, WithGrantOption: true},
				{Role: "Public", Privileges: []string{grantTestUsage}},
			}}, postgresv1alpha1.SchemaSpec{Name: "unmanaged", Grants: []postgresv1alpha1.GrantSpec{
				{Role: grantTestRole, Privileges: []string{grantTestCreate}}, // not managed: never granted
			}})
			Expect(reconcileSchemaGrants(ctx, f, db, managedApp, testChecker(f, nil), nil)).To(Succeed())
			both := []string{postgres.PrivilegeCreate, postgres.PrivilegeUsage}
			Expect(f.calls).To(Equal([]string{
				"grant CREATE,USAGE on schema app to app wgo=true",
				"grant USAGE on schema app to PUBLIC wgo=false",
			}))
			Expect(db.Status.ManagedSchemaGrants).To(Equal([]postgresv1alpha1.ManagedSchemaGrant{
				{Schema: grantTestRole, Role: postgres.PublicGrantee, Privileges: usage},
				{Schema: grantTestRole, Role: grantTestRole, Privileges: both, GrantOptions: both},
			}))
		})

		It("revokes removed privileges, grants and schemas it added, with CASCADE after the grant option", func() {
			f := &fakeGrantClient{}
			both := []string{postgres.PrivilegeCreate, postgres.PrivilegeUsage}
			f.hold(testSchemaObj, grantTestRole, true, both...)
			db := newDB(postgresv1alpha1.SchemaSpec{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
				{Role: grantTestRole, Privileges: []string{grantTestUsage}},
			}})
			db.Status.ManagedSchemaGrants = []postgresv1alpha1.ManagedSchemaGrant{
				{Schema: grantTestRole, Role: grantTestRole, Privileges: both, GrantOptions: both},
				{Schema: grantTestRole, Role: postgres.PublicGrantee, Privileges: usage},
				{Schema: "gone_entry", Role: grantTestOther, Privileges: usage},
			}
			Expect(reconcileSchemaGrants(ctx, f, db, managedApp, testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				"revoke USAGE on schema app from PUBLIC optionOnly=false cascade=false",
				"revoke USAGE on schema app from app optionOnly=true cascade=true",
				"revoke CREATE on schema app from app optionOnly=false cascade=true",
				"revoke USAGE on schema gone_entry from other optionOnly=false cascade=false",
			}))
			Expect(db.Status.ManagedSchemaGrants).To(Equal([]postgresv1alpha1.ManagedSchemaGrant{
				{Schema: grantTestRole, Role: grantTestRole, Privileges: usage},
			}))
		})

		It("keeps the default PUBLIC USAGE on public and the owner's privileges when declared grants are removed", func() {
			f := &fakeGrantClient{}
			f.hold(testPublicObj, postgres.PublicGrantee, false, postgres.PrivilegeUsage)
			f.hold(testSchemaObj, grantTestAdmin, true, postgres.PrivilegeCreate, postgres.PrivilegeUsage) // the owner
			db := newDB(
				postgresv1alpha1.SchemaSpec{Name: publicSchemaName, Grants: []postgresv1alpha1.GrantSpec{
					{Role: postgres.PublicGrantee, Privileges: []string{grantTestUsage}}}},
				postgresv1alpha1.SchemaSpec{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
					{Role: grantTestAdmin, Privileges: []string{"all"}, WithGrantOption: true}}},
			)
			Expect(reconcileSchemaGrants(ctx, f, db, managedApp, testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedSchemaGrants).To(BeNil())
			db.Spec.Schemas = nil
			Expect(reconcileSchemaGrants(ctx, f, db, managedApp, testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
		})

		It("forgets grants on schemas or to roles that no longer exist without revoking", func() {
			f := &fakeGrantClient{schemas: map[string]bool{grantTestRole: true}, roles: map[string]bool{}}
			db := newDB()
			db.Status.ManagedSchemaGrants = []postgresv1alpha1.ManagedSchemaGrant{
				{Schema: "gone_schema", Role: postgres.PublicGrantee, Privileges: usage},
				{Schema: grantTestRole, Role: "dropped_role", Privileges: usage},
			}
			Expect(reconcileSchemaGrants(ctx, f, db, managedApp, testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedSchemaGrants).To(BeNil())
		})

		It("rejects invalid privileges and duplicates before issuing SQL", func() {
			f := &fakeGrantClient{}
			db := newDB(postgresv1alpha1.SchemaSpec{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
				{Role: grantTestRole, Privileges: []string{"USAGE; DROP SCHEMA app"}},
			}})
			Expect(reconcileSchemaGrants(ctx, f, db, managedApp, testChecker(f, nil), nil)).To(MatchError(ContainSubstring("invalid schema privilege")))
			db = newDB(postgresv1alpha1.SchemaSpec{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
				{Role: postgres.PublicGrantee, Privileges: []string{grantTestUsage}}, {Role: grantTestLowerPublic, Privileges: []string{grantTestCreate}},
			}})
			Expect(reconcileSchemaGrants(ctx, f, db, managedApp, testChecker(f, nil), nil)).To(MatchError(ContainSubstring("listed more than once")))
			db = newDB(postgresv1alpha1.SchemaSpec{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
				{Role: postgres.PublicGrantee, Privileges: []string{grantTestUsage}, WithGrantOption: true},
			}})
			Expect(reconcileSchemaGrants(ctx, f, db, managedApp, testChecker(f, nil), nil)).To(MatchError(ContainSubstring("grant option cannot be granted to PUBLIC")))
			Expect(f.calls).To(BeEmpty())
		})
	})

	Describe("two resources granting the same privilege", func() {
		It("keeps each Database's ledger to its own database", func() {
			f := &fakeGrantClient{}
			grant := []postgresv1alpha1.DatabaseGrantSpec{{Role: grantTestRole, Privileges: connect}}
			a := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: grant}}
			b := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: grant}}
			Expect(reconcileDatabaseGrants(ctx, f, a, "db_a", testChecker(f, nil), nil)).To(Succeed())
			Expect(reconcileDatabaseGrants(ctx, f, b, "db_b", testChecker(f, nil), nil)).To(Succeed())

			By("removing the grant from one of them")
			a.Spec.Grants = nil
			f.calls = nil
			Expect(reconcileDatabaseGrants(ctx, f, a, "db_a", testChecker(f, nil), nil)).To(Succeed())
			Expect(reconcileDatabaseGrants(ctx, f, b, "db_b", testChecker(f, nil), nil)).To(Succeed())
			Expect(f.calls).To(Equal([]string{"revoke CONNECT on db_a from app optionOnly=false cascade=false"}))
			held, _, _, _ := f.HeldPrivileges(ctx, postgres.PrivilegeObject{Kind: postgres.ObjectDatabase, Name: "db_b"}, grantTestRole)
			Expect(held).To(Equal(connect))
		})

		It("lets the resource that still declares a grant restore it on its next reconcile", func() {
			// Should two ledgers ever hold the same key, the one that still
			// declares the privilege re-grants it: grant wins.
			f := &fakeGrantClient{}
			desired := []pg{desiredGrant(tgt(grantTestRole), connect, false)}
			ledgerA, err := applyPrivilegeGrants(ctx, desired, nil, executorOps(f, nil), 0, nil)
			Expect(err).NotTo(HaveOccurred())
			_, err = applyPrivilegeGrants(ctx, nil, ledgerA, executorOps(f, nil), 0, nil) // A removed it: revoke
			Expect(err).NotTo(HaveOccurred())
			_, err = applyPrivilegeGrants(ctx, desired, ledgerA, executorOps(f, nil), 0, nil) // B still declares it: grant
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{
				callGrantConnectDB,
				"revoke CONNECT on db from app optionOnly=false cascade=false",
				callGrantConnectDB,
			}))
		})
	})

	Describe("reconcilePublicPrivileges", func() {
		newClient := func() *fakeGrantClient {
			c := &fakeGrantClient{}
			c.hold(testDBObj, postgres.PublicGrantee, false, postgres.PrivilegeConnect, postgres.PrivilegeTemporary)
			c.hold(testPublicObj, postgres.PublicGrantee, false, postgres.PrivilegeUsage)
			return c
		}
		revoke := new(false)

		It("is a no-op when nothing is requested or recorded", func() {
			c := newClient()
			db := &postgresv1alpha1.Database{}
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase, nil)).To(Succeed())
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema, nil)).To(Succeed())
			Expect(c.calls).To(BeEmpty())
		})

		It("revokes only what PUBLIC holds, records it and grants exactly that back", func() {
			c := newClient()
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{PublicPrivileges: &postgresv1alpha1.PublicPrivilegesSpec{
				Connect: revoke, Temporary: revoke, PublicSchemaUsage: revoke, PublicSchemaCreate: revoke,
			}}}
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase, nil)).To(Succeed())
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema, nil)).To(Succeed())
			Expect(c.calls).To(Equal([]string{
				callRevokeConnectPublic,
				"revoke TEMPORARY on app_db from PUBLIC optionOnly=false cascade=false",
				"revoke USAGE on schema public from PUBLIC optionOnly=false cascade=false",
			}))
			// CREATE on public was not PUBLIC's (PostgreSQL 15+): not recorded.
			Expect(db.Status.RevokedPublicPrivileges).To(Equal([]postgresv1alpha1.PublicPrivilege{
				postgresv1alpha1.PublicPrivilegeConnect, postgresv1alpha1.PublicPrivilegeTemporary,
				postgresv1alpha1.PublicPrivilegePublicSchemaUsage,
			}))

			By("being idempotent")
			c.calls = nil
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase, nil)).To(Succeed())
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema, nil)).To(Succeed())
			Expect(c.calls).To(BeEmpty())

			By("revoking again what was granted back outside pgop")
			c.hold(testDBObj, postgres.PublicGrantee, false, postgres.PrivilegeConnect)
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase, nil)).To(Succeed())
			Expect(c.calls).To(Equal([]string{callRevokeConnectPublic}))

			By("granting back what it revoked once the spec stops asking")
			c.calls = nil
			db.Spec.PublicPrivileges = &postgresv1alpha1.PublicPrivilegesSpec{Temporary: revoke, PublicSchemaUsage: new(true)}
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase, nil)).To(Succeed())
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema, nil)).To(Succeed())
			Expect(c.calls).To(Equal([]string{
				"grant CONNECT on app_db to PUBLIC wgo=false",
				"grant USAGE on schema public to PUBLIC wgo=false",
			}))
			Expect(db.Status.RevokedPublicPrivileges).To(Equal([]postgresv1alpha1.PublicPrivilege{postgresv1alpha1.PublicPrivilegeTemporary}))
		})

		It("records a PUBLIC privilege before revoking it", func() {
			c := newClient()
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{PublicPrivileges: &postgresv1alpha1.PublicPrivilegesSpec{Connect: revoke}}}
			var persisted []postgresv1alpha1.PublicPrivilege
			save := func(context.Context) error {
				Expect(c.calls).To(BeEmpty(), "the record is written before the REVOKE")
				persisted = slices.Clone(db.Status.RevokedPublicPrivileges)
				return nil
			}
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase, save)).To(Succeed())
			Expect(persisted).To(Equal([]postgresv1alpha1.PublicPrivilege{postgresv1alpha1.PublicPrivilegeConnect}))
			Expect(c.calls).To(Equal([]string{callRevokeConnectPublic}))

			c2 := newClient()
			db2 := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{PublicPrivileges: &postgresv1alpha1.PublicPrivilegesSpec{Connect: revoke}}}
			err := reconcilePublicPrivileges(ctx, c2, db2, "app_db", postgres.ObjectDatabase, func(context.Context) error { return errors.New("conflict") })
			Expect(err).To(MatchError(ContainSubstring("nothing was revoked")))
			Expect(c2.calls).To(BeEmpty())
			Expect(db2.Status.RevokedPublicPrivileges).To(BeNil())
		})

		It("reports a PUBLIC privilege still held from another grantor", func() {
			c := newClient()
			c.elsewhere = map[string]bool{aclKey(testDBObj, postgres.PublicGrantee) + "|" + postgres.PrivilegeConnect: true}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{PublicPrivileges: &postgresv1alpha1.PublicPrivilegesSpec{Connect: revoke}}}
			err := reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase, nil)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
			Expect(ce.reason).To(Equal(ReasonPublicPrivilegeStillHeld))
			Expect(c.calls).To(Equal([]string{callRevokeConnectPublic}))
		})

		It("forgets a recorded revoke on a schema public that was dropped", func() {
			c := &fakeGrantClient{schemas: map[string]bool{}}
			db := &postgresv1alpha1.Database{}
			db.Status.RevokedPublicPrivileges = []postgresv1alpha1.PublicPrivilege{postgresv1alpha1.PublicPrivilegePublicSchemaUsage}
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema, nil)).To(Succeed())
			Expect(c.calls).To(BeEmpty())
			Expect(db.Status.RevokedPublicPrivileges).To(BeNil())
		})

		It("leaves a privilege alone that the spec also grants to PUBLIC", func() {
			c := newClient()
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{
				PublicPrivileges: &postgresv1alpha1.PublicPrivilegesSpec{Connect: revoke, Temporary: revoke, PublicSchemaUsage: revoke},
				Grants:           []postgresv1alpha1.DatabaseGrantSpec{{Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeAll}}},
				Schemas: []postgresv1alpha1.SchemaSpec{{Name: publicSchemaName, Grants: []postgresv1alpha1.GrantSpec{
					{Role: postgres.PublicGrantee, Privileges: []string{grantTestUsage}},
				}}},
			}}
			err := reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase, nil)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
			Expect(ce.reason).To(Equal(ReasonPublicPrivilegeConflict))
			Expect(err.Error()).To(ContainSubstring("connect, temporary"))
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema, nil)).To(MatchError(ContainSubstring("publicSchemaUsage")))
			Expect(c.calls).To(BeEmpty())
		})
	})

	Describe("reconcileSchemas", func() {
		It("creates missing schemas, manages owned ones and leaves others alone", func() {
			s := &fakeSchemaClient{
				dbOwner: grantTestAdmin,
				owners: map[string]string{
					ownedByDBSchema: grantTestAdmin, declaredSchema: grantTestRole, publicSchemaName: pgDatabaseOwnerRole,
					"ext": DefaultOperatorUsername, grantTestOther: grantTestOther, recordedSchema: DefaultOperatorUsername, "su_owned": polDBA,
				},
				superusers: map[string]bool{DefaultOperatorUsername: true, polDBA: true},
			}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Schemas: []postgresv1alpha1.SchemaSpec{
				{Name: "fresh"}, {Name: "fresh_owned", Owner: grantTestRole},
				{Name: ownedByDBSchema, Owner: grantTestRole}, {Name: declaredSchema, Owner: grantTestRole}, {Name: publicSchemaName},
				{Name: "ext"}, {Name: grantTestOther}, {Name: recordedSchema}, {Name: "su_owned", Owner: polDBA}, {Name: "pg_catalog"},
			}}}
			db.Status.CreatedSchemas = []string{recordedSchema}
			managed, refused, err := reconcileSchemas(ctx, s, db)
			Expect(err).NotTo(HaveOccurred())
			Expect(managed).To(Equal([]string{"fresh", "fresh_owned", ownedByDBSchema, declaredSchema, publicSchemaName, recordedSchema}))
			Expect(s.calls).To(Equal([]string{
				"create fresh owner admin", "create fresh_owned owner app", "alter owned_by_db owner app",
			}))
			Expect(refused).To(MatchError(SatisfyAll(
				ContainSubstring("ext (owned by pgop_operator)"), ContainSubstring("other (owned by other)"),
				ContainSubstring("su_owned (owned by dba)"), ContainSubstring("pg_catalog"))))
			reasons := map[string]bool{}
			for _, e := range refused.(interface{ Unwrap() []error }).Unwrap() {
				ce, ok := errors.AsType[*conditionError](e)
				Expect(ok).To(BeTrue())
				reasons[ce.reason] = true
			}
			Expect(reasons).To(Equal(map[string]bool{ReasonSchemaNotAllowed: true, ReasonSchemaNotManaged: true}))
		})

		It("manages a schema public owned by the bootstrap superuser for grants only", func() {
			s := &fakeSchemaClient{
				dbOwner:    grantTestAdmin,
				owners:     map[string]string{publicSchemaName: DefaultOperatorUsername},
				superusers: map[string]bool{DefaultOperatorUsername: true},
				bootstrap:  DefaultOperatorUsername,
			}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Schemas: []postgresv1alpha1.SchemaSpec{
				{Name: publicSchemaName, Owner: grantTestRole},
			}}}
			db.Status.CreatedSchemas = []string{publicSchemaName}
			managed, refused, err := reconcileSchemas(ctx, s, db)
			Expect(err).NotTo(HaveOccurred())
			Expect(refused).NotTo(HaveOccurred())
			Expect(managed).To(Equal([]string{publicSchemaName}))
			Expect(s.calls).To(BeEmpty(), "its owner is never changed")
		})
	})
})

// fakeSchemaClient models the schemas of one database for reconcileSchemas.
type fakeSchemaClient struct {
	dbOwner    string
	owners     map[string]string // existing schemas and their owners
	superusers map[string]bool
	bootstrap  string // the bootstrap superuser's name
	calls      []string
}

func (s *fakeSchemaClient) SchemaOwner(_ context.Context, name string) (postgres.SchemaInfo, bool, error) {
	o, ok := s.owners[name]
	return postgres.SchemaInfo{Owner: o, OwnerIsBootstrap: ok && o == s.bootstrap}, ok, nil
}

func (s *fakeSchemaClient) CurrentDatabaseOwner(context.Context) (string, error) {
	return s.dbOwner, nil
}

func (s *fakeSchemaClient) CreateSchema(_ context.Context, name, owner string) error {
	s.calls = append(s.calls, "create "+name+" owner "+owner)
	return nil
}

func (s *fakeSchemaClient) AlterSchemaOwner(_ context.Context, name, owner string) error {
	s.calls = append(s.calls, "alter "+name+" owner "+owner)
	return nil
}

func (s *fakeSchemaClient) LookupRole(_ context.Context, name string) (*postgres.ReachableRole, error) {
	return &postgres.ReachableRole{Name: name, Superuser: s.superusers[name]}, nil
}

var _ = Describe("Settings and parameter grants", func() {
	ctx := context.Background()

	Describe("reconcileDatabaseSettings", func() {
		It("sets desired settings, resets removed managed ones and records them", func() {
			f := &fakeGrantClient{}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{
				Settings: map[string]string{testWorkMem: grantTestValue, "MyApp.Tenant": grantTestTenant},
			}}
			db.Status.ManagedSettings = []string{"statement_timeout", testWorkMem}
			Expect(reconcileDatabaseSettings(ctx, f, db, "app_db")).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				"set myapp.tenant on app_db to acme",
				"set work_mem on app_db to 64MB",
				"reset statement_timeout on app_db",
			}))
			Expect(db.Status.ManagedSettings).To(Equal([]string{grantTestParam, testWorkMem}))
		})

		It("keeps a setting tracked when its reset fails", func() {
			f := &fakeGrantClient{failOn: "reset"}
			db := &postgresv1alpha1.Database{}
			db.Status.ManagedSettings = []string{testWorkMem}
			Expect(reconcileDatabaseSettings(ctx, f, db, "app_db")).NotTo(Succeed())
			Expect(db.Status.ManagedSettings).To(Equal([]string{testWorkMem}))
		})

		It("rejects invalid and case-duplicate parameter names", func() {
			f := &fakeGrantClient{}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{
				Settings: map[string]string{"work_mem; DROP DATABASE x": "1"},
			}}
			Expect(reconcileDatabaseSettings(ctx, f, db, "app_db")).To(MatchError(ContainSubstring("invalid parameter name")))
			db.Spec.Settings = map[string]string{grantTestMixedCase: "1", testWorkMem: "2"}
			Expect(reconcileDatabaseSettings(ctx, f, db, "app_db")).To(MatchError(ContainSubstring("more than once")))
			Expect(f.calls).To(BeEmpty())
		})

		It("refuses denylisted and superuser-only parameters but applies the rest", func() {
			f := &fakeGrantClient{contexts: map[string]string{
				testWorkMem: "user", grantTestSuperuserParam: "superuser", grantTestPostmasterParam: "postmaster",
			}}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Settings: map[string]string{
				testWorkMem: grantTestValue, grantTestSuperuserParam: "none", grantTestPostmasterParam: "1GB",
				"Session_Replication_Role": "replica", grantTestRoleParam: "postgres", grantTestAuditParam: "none",
				grantTestParam: grantTestTenant,
			}}}
			// A setting applied before it became disallowed is reset.
			db.Status.ManagedSettings = []string{grantTestReplicationRole}
			err := reconcileDatabaseSettings(ctx, f, db, "app_db")
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
			Expect(ce.reason).To(Equal(ReasonSettingNotAllowed))
			for _, name := range []string{grantTestSuperuserParam, grantTestPostmasterParam, grantTestReplicationRole, grantTestRoleParam, grantTestAuditParam} {
				Expect(err.Error()).To(ContainSubstring(name))
			}
			Expect(f.calls).To(Equal([]string{
				"set myapp.tenant on app_db to acme",
				"set work_mem on app_db to 64MB",
				"reset session_replication_role on app_db",
			}))
			Expect(db.Status.ManagedSettings).To(Equal([]string{grantTestParam, testWorkMem}))
		})
	})

	Describe("reconcileParameterGrants", func() {
		newRole := func(grants []postgresv1alpha1.ParameterGrantSpec, managed ...postgresv1alpha1.ManagedParameterGrant) *postgresv1alpha1.Role {
			r := &postgresv1alpha1.Role{Spec: postgresv1alpha1.RoleSpec{ParameterGrants: grants}}
			r.Status.ManagedParameterGrants = managed
			return r
		}

		It("is a no-op without parameter grants", func() {
			f := &fakeGrantClient{version: 140000}
			Expect(reconcileParameterGrants(ctx, f, newRole(nil), grantTestRole, nil)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
		})

		It("grants SET by default, revokes removed grants and records them", func() {
			f := &fakeGrantClient{version: 180001}
			role := newRole([]postgresv1alpha1.ParameterGrantSpec{
				{Parameter: "Log_Statement"},
				{Parameter: grantTestParam, Privileges: []string{postgres.PrivilegeSet}, WithGrantOption: true},
			}, postgresv1alpha1.ManagedParameterGrant{Parameter: testWorkMem, Privileges: []string{postgres.PrivilegeSet}})
			Expect(reconcileParameterGrants(ctx, f, role, grantTestRole, nil)).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				"grant SET on parameter log_statement to app wgo=false",
				"grant SET on parameter myapp.tenant to app wgo=true",
				"revoke SET on parameter work_mem from app optionOnly=false cascade=false",
			}))
			Expect(role.Status.ManagedParameterGrants).To(Equal([]postgresv1alpha1.ManagedParameterGrant{
				{Parameter: grantTestSuperuserParam, Privileges: []string{postgres.PrivilegeSet}},
				{Parameter: grantTestParam, Privileges: []string{postgres.PrivilegeSet}, GrantOptions: []string{postgres.PrivilegeSet}},
			}))
		})

		It("reports UnsupportedServerVersion before PostgreSQL 15", func() {
			f := &fakeGrantClient{version: 140010}
			role := newRole([]postgresv1alpha1.ParameterGrantSpec{{Parameter: testWorkMem}})
			err := reconcileParameterGrants(ctx, f, role, grantTestRole, nil)
			Expect(err).To(MatchError(ContainSubstring("require PostgreSQL 15 or later")))
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue())
			Expect(ce.reason).To(Equal(ReasonUnsupportedServerVersion))
			Expect(f.calls).To(BeEmpty())
		})

		It("rejects ALTER SYSTEM", func() {
			f := &fakeGrantClient{version: 180001}
			role := newRole([]postgresv1alpha1.ParameterGrantSpec{{Parameter: testWorkMem, Privileges: []string{"ALTER SYSTEM"}}})
			Expect(reconcileParameterGrants(ctx, f, role, grantTestRole, nil)).To(MatchError(ContainSubstring("invalid parameter privilege")))
			Expect(f.calls).To(BeEmpty())
		})

		It("refuses denylisted parameters, grants the rest and revokes a managed denied grant", func() {
			f := &fakeGrantClient{version: 180001}
			role := newRole([]postgresv1alpha1.ParameterGrantSpec{
				{Parameter: grantTestRoleParam}, {Parameter: "Session_Authorization"}, {Parameter: testWorkMem},
			}, postgresv1alpha1.ManagedParameterGrant{Parameter: grantTestReplicationRole, Privileges: []string{postgres.PrivilegeSet}})
			err := reconcileParameterGrants(ctx, f, role, grantTestRole, nil)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
			Expect(ce.reason).To(Equal(ReasonParameterNotAllowed))
			Expect(err.Error()).To(ContainSubstring("role, session_authorization"))
			Expect(f.calls).To(Equal([]string{
				"grant SET on parameter work_mem to app wgo=false",
				"revoke SET on parameter session_replication_role from app optionOnly=false cascade=false",
			}))
			Expect(role.Status.ManagedParameterGrants).To(Equal([]postgresv1alpha1.ManagedParameterGrant{
				{Parameter: testWorkMem, Privileges: []string{postgres.PrivilegeSet}},
			}))
		})

		It("only grants user and superuser parameters and custom placeholders", func() {
			f := &fakeGrantClient{version: 180001, contexts: map[string]string{
				grantTestSuperuserParam: pgContextSuperuser, testWorkMem: pgContextUser, grantTestSighupParam: "sighup",
				"log_connections": "superuser-backend", "shared_buffers": "postmaster",
			}}
			role := newRole([]postgresv1alpha1.ParameterGrantSpec{
				{Parameter: "log_statement"}, {Parameter: testWorkMem}, {Parameter: grantTestSighupParam},
				{Parameter: "log_connections"}, {Parameter: "shared_buffers"}, {Parameter: grantTestParam},
				{Parameter: "lo_compat_privileges"},
			}, postgresv1alpha1.ManagedParameterGrant{Parameter: grantTestSighupParam, Privileges: []string{postgres.PrivilegeSet}})
			err := reconcileParameterGrants(ctx, f, role, grantTestRole, nil)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
			Expect(ce.reason).To(Equal(ReasonParameterNotAllowed))
			Expect(err.Error()).To(SatisfyAll(
				ContainSubstring(grantTestSighupParam+` (context "sighup")`),
				ContainSubstring(`log_connections (context "superuser-backend")`),
				ContainSubstring(`shared_buffers (context "postmaster")`),
				ContainSubstring("lo_compat_privileges"),
			))
			Expect(f.calls).To(Equal([]string{
				"grant SET on parameter log_statement to app wgo=false",
				"grant SET on parameter work_mem to app wgo=false",
				"grant SET on parameter myapp.tenant to app wgo=false",
				"revoke SET on parameter " + grantTestSighupParam + " from app optionOnly=false cascade=false",
			}))
		})

		It("revokes every managed grant before the role is dropped", func() {
			f := &fakeGrantClient{version: 180001}
			role := newRole([]postgresv1alpha1.ParameterGrantSpec{{Parameter: testWorkMem}},
				postgresv1alpha1.ManagedParameterGrant{Parameter: testWorkMem, Privileges: []string{postgres.PrivilegeSet}})
			Expect(revokeManagedParameterGrants(ctx, f, role, grantTestRole)).To(Succeed())
			Expect(f.calls).To(Equal([]string{"revoke SET on parameter work_mem from app optionOnly=false cascade=false"}))
			Expect(role.Status.ManagedParameterGrants).To(BeNil())
		})
	})
})

var _ = Describe("Grant and settings CRD validation", func() {
	ctx := context.Background()
	name := func(prefix string) string { return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()) }

	newDatabase := func(spec postgresv1alpha1.DatabaseSpec) *postgresv1alpha1.Database {
		spec.ClusterRef = postgresv1alpha1.ClusterReference{Name: nonexistentCluster}
		return &postgresv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: name("grant-db"), Namespace: "default"},
			Spec:       spec,
		}
	}
	newRole := func(grants []postgresv1alpha1.ParameterGrantSpec) *postgresv1alpha1.Role {
		return &postgresv1alpha1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name("param-role"), Namespace: "default"},
			Spec: postgresv1alpha1.RoleSpec{
				ClusterRef:      postgresv1alpha1.ClusterReference{Name: nonexistentCluster},
				ParameterGrants: grants,
			},
		}
	}
	expectInvalid := func(obj client.Object) {
		err := k8sClient.Create(ctx, obj)
		if err == nil {
			_ = k8sClient.Delete(ctx, obj)
		}
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
	}

	It("accepts valid database grants, settings and schema grants", func() {
		db := newDatabase(postgresv1alpha1.DatabaseSpec{
			Grants: []postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeTemp}},
				{Role: grantTestAdmin, Privileges: []string{postgres.PrivilegeAll}, WithGrantOption: true},
			},
			Settings: map[string]string{testWorkMem: grantTestValue, grantTestParam: grantTestTenant, grantTestSearchPath: `"$user", app, public`},
			Schemas: []postgresv1alpha1.SchemaSpec{{
				Name:   grantTestRole,
				Grants: []postgresv1alpha1.GrantSpec{{Role: grantTestRole, Privileges: []string{postgres.PrivilegeUsage, postgres.PrivilegeCreate}}},
			}},
		})
		Expect(k8sClient.Create(ctx, db)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, db) }()
		got := &postgresv1alpha1.Database{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(db), got)).To(Succeed())
		Expect(got.Spec.Grants).To(Equal(db.Spec.Grants))
		Expect(maps.Equal(got.Spec.Settings, db.Spec.Settings)).To(BeTrue())
	})

	DescribeTable("rejects invalid database privileges",
		func(privs []string) {
			expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
				Grants: []postgresv1alpha1.DatabaseGrantSpec{{Role: grantTestRole, Privileges: privs}},
			}))
		},
		Entry("injection", []string{"CONNECT ON DATABASE postgres TO public; --"}),
		Entry("schema privilege", []string{postgres.PrivilegeUsage}),
		Entry("lowercase", []string{"connect"}),
		Entry("empty list", []string{}),
	)

	It("rejects a database grant without a role and duplicate grant roles", func() {
		expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
			Grants: []postgresv1alpha1.DatabaseGrantSpec{{Role: "", Privileges: []string{postgres.PrivilegeConnect}}},
		}))
		expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
			Grants: []postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}},
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeCreate}},
			},
		}))
	})

	DescribeTable("rejects invalid schema privileges (closes the GRANT injection)",
		func(priv string) {
			expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
				Schemas: []postgresv1alpha1.SchemaSpec{{
					Name:   grantTestRole,
					Grants: []postgresv1alpha1.GrantSpec{{Role: grantTestRole, Privileges: []string{priv}}},
				}},
			}))
		},
		Entry("injection", "USAGE ON SCHEMA public TO public; DROP DATABASE postgres; --"),
		Entry("database privilege", postgres.PrivilegeConnect),
		Entry("table privilege", "SELECT"),
	)

	DescribeTable("rejects invalid settings keys",
		func(key string) {
			expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{Settings: map[string]string{key: "1"}}))
		},
		Entry("injection", "work_mem TO 1; DROP DATABASE postgres; --"),
		Entry("space", "work mem"),
		Entry("leading digit", "1abc"),
		Entry("trailing dot", "myapp."),
		Entry("quote", `work_mem"`),
		Entry("too long", strings.Repeat("a", 128)),
	)

	It("rejects an oversized settings value", func() {
		expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
			Settings: map[string]string{"myapp.blob": strings.Repeat("x", 4097)},
		}))
	})

	It("accepts parameter grants and defaults privileges to SET", func() {
		role := newRole([]postgresv1alpha1.ParameterGrantSpec{
			{Parameter: grantTestSuperuserParam},
			{Parameter: "myapp.tenant_id", Privileges: []string{postgres.PrivilegeSet}, WithGrantOption: true},
		})
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, role) }()
		got := &postgresv1alpha1.Role{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(role), got)).To(Succeed())
		Expect(got.Spec.ParameterGrants[0].Privileges).To(Equal([]string{postgres.PrivilegeSet}))
		Expect(got.Spec.ParameterGrants[1].WithGrantOption).To(BeTrue())
	})

	DescribeTable("rejects invalid parameter grants",
		func(g postgresv1alpha1.ParameterGrantSpec) {
			expectInvalid(newRole([]postgresv1alpha1.ParameterGrantSpec{g}))
		},
		Entry("ALTER SYSTEM", postgresv1alpha1.ParameterGrantSpec{Parameter: testWorkMem, Privileges: []string{"ALTER SYSTEM"}}),
		Entry("ALL privilege", postgresv1alpha1.ParameterGrantSpec{Parameter: testWorkMem, Privileges: []string{postgres.PrivilegeAll}}),
		Entry("injection in privilege", postgresv1alpha1.ParameterGrantSpec{Parameter: testWorkMem, Privileges: []string{"SET; DROP ROLE x"}}),
		Entry("injection in parameter", postgresv1alpha1.ParameterGrantSpec{Parameter: "work_mem TO x; --"}),
		Entry("empty parameter", postgresv1alpha1.ParameterGrantSpec{Parameter: ""}),
		Entry(grantTestRoleParam, postgresv1alpha1.ParameterGrantSpec{Parameter: grantTestRoleParam}),
		Entry("session_authorization", postgresv1alpha1.ParameterGrantSpec{Parameter: "Session_Authorization"}),
		Entry(grantTestReplicationRole, postgresv1alpha1.ParameterGrantSpec{Parameter: grantTestReplicationRole}),
		Entry("preload libraries", postgresv1alpha1.ParameterGrantSpec{Parameter: "session_preload_libraries"}),
		Entry("pgaudit", postgresv1alpha1.ParameterGrantSpec{Parameter: grantTestAuditParam}),
	)

	It("accepts schema privileges in any case and ALL PRIVILEGES", func() {
		db := newDatabase(postgresv1alpha1.DatabaseSpec{
			Schemas: []postgresv1alpha1.SchemaSpec{{
				Name: grantTestRole,
				Grants: []postgresv1alpha1.GrantSpec{
					{Role: grantTestRole, Privileges: []string{grantTestUsage, "Create"}},
					{Role: grantTestAdmin, Privileges: []string{"ALL PRIVILEGES"}},
					{Role: grantTestOther, Privileges: []string{"all"}},
				},
			}},
		})
		Expect(k8sClient.Create(ctx, db)).To(Succeed())
		_ = k8sClient.Delete(ctx, db)
	})

	DescribeTable("rejects denylisted settings keys",
		func(key string) {
			expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{Settings: map[string]string{key: "x"}}))
		},
		Entry(grantTestRoleParam, grantTestRoleParam),
		Entry("session_authorization", "SESSION_AUTHORIZATION"),
		Entry("session_preload_libraries", "session_preload_libraries"),
		Entry("local_preload_libraries", "local_preload_libraries"),
		Entry("dynamic_library_path", "dynamic_library_path"),
		Entry(grantTestReplicationRole, grantTestReplicationRole),
		Entry("pgaudit", grantTestAuditParam),
	)

	It("merges the grant ledgers when a status write conflicts", func() {
		db := newDatabase(postgresv1alpha1.DatabaseSpec{})
		Expect(k8sClient.Create(ctx, db)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, db) }()
		stale := db.DeepCopy()

		By("recording a grant through one copy")
		db.Status.ManagedGrants = []postgresv1alpha1.ManagedDatabaseGrant{{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}}}
		Expect(k8sClient.Status().Update(ctx, db)).To(Succeed())

		By("writing another ledger entry through a stale copy")
		stale.Status.ManagedSchemaGrants = []postgresv1alpha1.ManagedSchemaGrant{
			{Schema: grantTestRole, Role: grantTestOther, Privileges: []string{postgres.PrivilegeUsage}},
		}
		r := &DatabaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(r.saveStatus(ctx, stale)).To(Succeed())

		got := &postgresv1alpha1.Database{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(db), got)).To(Succeed())
		Expect(got.Status.ManagedGrants).To(HaveLen(1), "the entry recorded through the other copy is kept")
		Expect(got.Status.ManagedSchemaGrants).To(HaveLen(1))
	})

	It("accepts PUBLIC grantees and publicPrivileges", func() {
		db := newDatabase(postgresv1alpha1.DatabaseSpec{
			Grants: []postgresv1alpha1.DatabaseGrantSpec{{Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeTemp}}},
			Schemas: []postgresv1alpha1.SchemaSpec{{
				Name: publicSchemaName,
				Grants: []postgresv1alpha1.GrantSpec{
					{Role: postgres.PublicGrantee, Privileges: []string{grantTestCreate}},
					{Role: grantTestRole, Privileges: []string{grantTestUsage}, WithGrantOption: true},
				},
			}},
			PublicPrivileges: &postgresv1alpha1.PublicPrivilegesSpec{
				Connect: new(false), PublicSchemaUsage: new(false), PublicSchemaCreate: new(true),
			},
		})
		Expect(k8sClient.Create(ctx, db)).To(Succeed())
		_ = k8sClient.Delete(ctx, db)
	})

	DescribeTable("rejects reserved and mis-spelled grantees",
		func(role string) {
			expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
				Grants: []postgresv1alpha1.DatabaseGrantSpec{{Role: role, Privileges: []string{postgres.PrivilegeConnect}}},
			}))
			expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
				Schemas: []postgresv1alpha1.SchemaSpec{{
					Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{{Role: role, Privileges: []string{grantTestUsage}}},
				}},
			}))
		},
		Entry("lower-case public", grantTestLowerPublic),
		Entry("mixed-case public", "Public"),
		Entry(bootstrapRoleName, bootstrapRoleName),
		Entry("operator role", DefaultOperatorUsername),
		Entry("predefined role", "pg_read_server_files"),
		Entry("none", "NONE"),
		Entry("empty", ""),
		Entry("too long", strings.Repeat("r", 64)),
	)

	It("rejects the grant option for PUBLIC", func() {
		expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
			Grants: []postgresv1alpha1.DatabaseGrantSpec{{Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeConnect}, WithGrantOption: true}},
		}))
		expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
			Schemas: []postgresv1alpha1.SchemaSpec{{
				Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{{Role: postgres.PublicGrantee, Privileges: []string{grantTestUsage}, WithGrantOption: true}},
			}},
		}))
	})

	It("rejects granting PUBLIC what publicPrivileges revokes", func() {
		expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
			Grants:           []postgresv1alpha1.DatabaseGrantSpec{{Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeAll}}},
			PublicPrivileges: &postgresv1alpha1.PublicPrivilegesSpec{Temporary: new(false)},
		}))
		expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
			Schemas: []postgresv1alpha1.SchemaSpec{{
				Name: publicSchemaName, Grants: []postgresv1alpha1.GrantSpec{{Role: postgres.PublicGrantee, Privileges: []string{"All Privileges"}}},
			}},
			PublicPrivileges: &postgresv1alpha1.PublicPrivilegesSpec{PublicSchemaCreate: new(false)},
		}))
	})

	It("rejects duplicate schemas, duplicate schema grantees and too many schema grants", func() {
		expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
			Schemas: []postgresv1alpha1.SchemaSpec{{Name: grantTestRole}, {Name: grantTestRole}},
		}))
		expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
			Schemas: []postgresv1alpha1.SchemaSpec{{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
				{Role: grantTestRole, Privileges: []string{grantTestUsage}}, {Role: grantTestRole, Privileges: []string{grantTestCreate}},
			}}},
		}))
		many := make([]postgresv1alpha1.GrantSpec, 0, 17)
		for i := range 17 {
			many = append(many, postgresv1alpha1.GrantSpec{Role: fmt.Sprintf("r%d", i), Privileges: []string{grantTestUsage}})
		}
		expectInvalid(newDatabase(postgresv1alpha1.DatabaseSpec{
			Schemas: []postgresv1alpha1.SchemaSpec{{Name: grantTestRole, Grants: many}},
		}))
	})
})
