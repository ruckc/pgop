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
	"strings"
	"time"

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
	callRevokeConnectPublic  = "revoke CONNECT on app_db from PUBLIC optionOnly=false cascade=false"
)

// fakeGrantClient records statements issued for database, schema and
// parameter grants, PUBLIC's privileges and database settings.
type fakeGrantClient struct {
	version int
	roles   map[string]bool // existing roles; nil means every role exists
	// superusers lists existing roles that are superusers; comments holds
	// their COMMENT ON ROLE (ownership markers).
	superusers map[string]bool
	comments   map[string]string
	// schemas lists existing schemas; nil means every schema exists.
	schemas map[string]bool
	// public maps "DATABASE x" / "SCHEMA x" to the privileges PUBLIC holds;
	// a missing key means the object does not exist.
	public map[string][]string
	// contexts maps parameter names to their pg_settings context; missing
	// names are unknown (custom placeholders).
	contexts map[string]string
	failOn   string // substring of a recorded call that fails
	calls    []string
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
	if postgres.IsPublic(grantee) && f.public != nil {
		key := string(obj.Kind) + " " + obj.Name
		f.public[key] = union(f.public[key], privs)
	}
	return nil
}

func (f *fakeGrantClient) RevokePrivileges(_ context.Context, obj postgres.PrivilegeObject, grantee string, privs []string, mode postgres.RevokeMode) error {
	if err := f.record(fmt.Sprintf("revoke %s on %s from %s optionOnly=%t cascade=%t",
		strings.Join(privs, ","), objectLabel(obj), grantee, mode.GrantOptionOnly, mode.Cascade)); err != nil {
		return err
	}
	if postgres.IsPublic(grantee) && f.public != nil {
		key := string(obj.Kind) + " " + obj.Name
		f.public[key] = subtract(f.public[key], privs)
	}
	return nil
}

func (f *fakeGrantClient) PublicPrivileges(_ context.Context, obj postgres.PrivilegeObject) ([]string, bool, error) {
	privs, ok := f.public[string(obj.Kind)+" "+obj.Name]
	return privs, ok, nil
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

	Describe("diffPrivilegeGrants", func() {
		It("grants every desired grant", func() {
			desired := []pg{{Target: tgt("a"), Privileges: []string{postgres.PrivilegeConnect}}}
			plan := diffPrivilegeGrants(desired, nil)
			Expect(plan.Grant).To(Equal(desired))
			Expect(plan.Revoke).To(BeEmpty())
			Expect(plan.RevokeGrantOption).To(BeEmpty())
		})

		It("revokes managed grants removed from the spec", func() {
			managed := []pg{{Target: tgt("gone"), Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}}}
			plan := diffPrivilegeGrants(nil, managed)
			Expect(plan.Revoke).To(Equal(managed))
		})

		It("revokes only managed privileges removed from a grant", func() {
			plan := diffPrivilegeGrants(
				[]pg{{Target: tgt("a"), Privileges: []string{postgres.PrivilegeConnect}}},
				[]pg{{Target: tgt("a"), Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}}})
			Expect(plan.Revoke).To(Equal([]pg{{Target: tgt("a"), Privileges: []string{postgres.PrivilegeCreate}}}))
		})

		It("revokes the grant option it granted once it is turned off", func() {
			plan := diffPrivilegeGrants(
				[]pg{{Target: tgt("a"), Privileges: []string{postgres.PrivilegeConnect}}},
				[]pg{{Target: tgt("a"), Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}, WithGrantOption: true}})
			Expect(plan.RevokeGrantOption).To(Equal([]pg{{Target: tgt("a"), Privileges: []string{postgres.PrivilegeConnect}}}))
			// Granted with the grant option: the revoke must cascade.
			Expect(plan.Revoke).To(Equal([]pg{{Target: tgt("a"), Privileges: []string{postgres.PrivilegeCreate}, WithGrantOption: true}}))
		})

		It("cascades every revoke of privileges granted with the grant option", func() {
			var modes []postgres.RevokeMode
			ops := privilegeOps{
				grant: func(context.Context, privilegeGrant) error { return nil },
				revoke: func(_ context.Context, _ privilegeGrant, mode postgres.RevokeMode) error {
					modes = append(modes, mode)
					return nil
				},
			}
			_, err := applyPrivilegeGrants(ctx,
				[]pg{{Target: tgt("a"), Privileges: []string{postgres.PrivilegeConnect}}},
				[]pg{
					{Target: tgt("a"), Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}, WithGrantOption: true},
					{Target: tgt("b"), Privileges: []string{postgres.PrivilegeConnect}, WithGrantOption: true},
					{Target: tgt("c"), Privileges: []string{postgres.PrivilegeConnect}},
				}, ops)
			Expect(err).NotTo(HaveOccurred())
			Expect(modes).To(Equal([]postgres.RevokeMode{
				{GrantOptionOnly: true, Cascade: true}, // a: CONNECT keeps, grant option goes
				{Cascade: true},                        // a: CREATE
				{Cascade: true},                        // b
				{},                                     // c: never had the grant option
			}))
		})

		It("never revokes privileges it did not grant", func() {
			plan := diffPrivilegeGrants([]pg{{Target: tgt("a"), Privileges: []string{postgres.PrivilegeConnect}, WithGrantOption: false}}, nil)
			Expect(plan.Revoke).To(BeEmpty())
			Expect(plan.RevokeGrantOption).To(BeEmpty())
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
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil))).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedGrants).To(BeNil())
		})

		It("grants normalized privileges and records them", func() {
			f := &fakeGrantClient{}
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeTemp}},
				{Role: grantTestAdmin, Privileges: []string{postgres.PrivilegeAll}, WithGrantOption: true},
			})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil))).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				"grant CONNECT,TEMPORARY on app_db to app wgo=false",
				"grant CONNECT,CREATE,TEMPORARY on app_db to admin wgo=true",
			}))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{
				{Role: grantTestAdmin, Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate, postgres.PrivilegeTemporary}, WithGrantOption: true},
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeTemporary}},
			}))
		})

		It("revokes removed grants and privileges", func() {
			f := &fakeGrantClient{}
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}}},
				postgresv1alpha1.ManagedDatabaseGrant{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}, WithGrantOption: true},
				postgresv1alpha1.ManagedDatabaseGrant{Role: "old", Privileges: []string{postgres.PrivilegeConnect}})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil))).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				callGrantConnectApp,
				"revoke CONNECT on app_db from app optionOnly=true cascade=true",
				"revoke CREATE on app_db from app optionOnly=false cascade=true",
				"revoke CONNECT on app_db from old optionOnly=false cascade=false",
			}))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}},
			}))
		})

		It("fails clearly while a grantee does not exist, keeping earlier grants tracked", func() {
			f := &fakeGrantClient{roles: map[string]bool{grantTestRole: true}}
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}},
				{Role: "later", Privileges: []string{postgres.PrivilegeConnect}},
			})
			err := reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil))
			Expect(err).To(MatchError(ContainSubstring(`PostgreSQL role "later" does not exist yet`)))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}},
			}))
		})

		It("forgets a managed grant whose role was dropped without revoking", func() {
			f := &fakeGrantClient{roles: map[string]bool{}}
			db := newDB(nil, postgresv1alpha1.ManagedDatabaseGrant{Role: "dropped", Privileges: []string{postgres.PrivilegeConnect}})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil))).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedGrants).To(BeNil())
		})

		It("keeps a pending revoke tracked when it fails", func() {
			f := &fakeGrantClient{failOn: "from old"}
			db := newDB(nil, postgresv1alpha1.ManagedDatabaseGrant{Role: "old", Privileges: []string{postgres.PrivilegeConnect}})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil))).NotTo(Succeed())
			Expect(db.Status.ManagedGrants).To(HaveLen(1))
		})

		It("rejects privileges outside the allow-list before issuing SQL", func() {
			f := &fakeGrantClient{}
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: []string{"CONNECT; DROP DATABASE app_db"}},
			})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil))).To(MatchError(ContainSubstring("invalid database privilege")))
			Expect(f.calls).To(BeEmpty())
		})

		It("pauses grants to roles that are being deleted", func() {
			f := &fakeGrantClient{}
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}},
				{Role: grantTestLeaving, Privileges: []string{postgres.PrivilegeConnect}},
			}, postgresv1alpha1.ManagedDatabaseGrant{Role: grantTestLeaving, Privileges: []string{postgres.PrivilegeConnect}})
			err := reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, map[string]bool{grantTestLeaving: true}))
			Expect(err).To(MatchError(ContainSubstring("grants to leaving are paused")))
			Expect(f.calls).To(Equal([]string{
				callGrantConnectApp,
				"revoke CONNECT on app_db from leaving optionOnly=false cascade=false",
			}))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}},
			}))
		})
	})

	Describe("grantee policy", func() {
		It("grants to PUBLIC with the bare keyword and records it canonically", func() {
			f := &fakeGrantClient{roles: map[string]bool{}}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: []postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestLowerPublic, Privileges: []string{postgres.PrivilegeConnect}},
			}}}
			// No role lookup is needed for PUBLIC, and no policy allows it.
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", &granteeChecker{pg: f})).To(Succeed())
			Expect(f.calls).To(Equal([]string{"grant CONNECT on app_db to PUBLIC wgo=false"}))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{
				{Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeConnect}},
			}))

			By("revoking it from PUBLIC once removed")
			db.Spec.Grants = nil
			f.calls = nil
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db", &granteeChecker{pg: f})).To(Succeed())
			Expect(f.calls).To(Equal([]string{callRevokeConnectPublic}))
			Expect(db.Status.ManagedGrants).To(BeNil())
		})

		It("rejects PUBLIC listed twice and the grant option for PUBLIC", func() {
			_, err := desiredDatabaseGrants([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeConnect}},
				{Role: grantTestLowerPublic, Privileges: []string{postgres.PrivilegeTemp}},
			}, "d")
			Expect(err).To(MatchError(ContainSubstring("listed more than once")))
			_, err = desiredDatabaseGrants([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeConnect}, WithGrantOption: true},
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
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}},  // managed by a Role
				{Role: "legacy", Privileges: []string{postgres.PrivilegeConnect}},       // allowlisted
				{Role: polDBA, Privileges: []string{postgres.PrivilegeConnect}},         // allowlisted but superuser
				{Role: "forged", Privileges: []string{postgres.PrivilegeConnect}},       // marker does not match
				{Role: grantTestOther, Privileges: []string{postgres.PrivilegeConnect}}, // unmanaged
				{Role: bootstrapRoleName, Privileges: []string{postgres.PrivilegeConnect}},
				{Role: "pgop_replicator", Privileges: []string{postgres.PrivilegeConnect}},
				{Role: "pg_monitor", Privileges: []string{postgres.PrivilegeConnect}},
			}}}
			// A grant pgop made earlier to a grantee that is no longer allowed.
			db.Status.ManagedGrants = []postgresv1alpha1.ManagedDatabaseGrant{
				{Role: grantTestOther, Privileges: []string{postgres.PrivilegeConnect}},
			}
			err := reconcileDatabaseGrants(ctx, f, db, "app_db", checker)
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
			err := reconcileSchemaGrants(ctx, f, db, allowGrantees(f, nil, grantTestRole))
			Expect(err).To(MatchError(ContainSubstring("schemas[app].grants: other is not managed")))
			Expect(f.calls).To(Equal([]string{"grant USAGE on schema app to app wgo=false"}))
		})

		It("reports a grantee that does not exist without holding up the others", func() {
			f := &fakeGrantClient{roles: map[string]bool{grantTestRole: true}}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: []postgresv1alpha1.DatabaseGrantSpec{
				{Role: "later", Privileges: []string{postgres.PrivilegeConnect}},
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}},
			}}}
			err := reconcileDatabaseGrants(ctx, f, db, "app_db", testChecker(f, nil))
			Expect(err).To(MatchError(ContainSubstring(`PostgreSQL role "later" does not exist yet`)))
			Expect(f.calls).To(Equal([]string{callGrantConnectApp}))
		})
	})

	Describe("reconcileSchemaGrants", func() {
		newDB := func(schemas ...postgresv1alpha1.SchemaSpec) *postgresv1alpha1.Database {
			return &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Schemas: schemas}}
		}

		It("grants normalized schema privileges and records them", func() {
			f := &fakeGrantClient{}
			db := newDB(postgresv1alpha1.SchemaSpec{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
				{Role: grantTestRole, Privileges: []string{"ALL PRIVILEGES"}, WithGrantOption: true},
				{Role: "Public", Privileges: []string{grantTestUsage}},
			}}, postgresv1alpha1.SchemaSpec{Name: "pg_catalog", Grants: []postgresv1alpha1.GrantSpec{
				{Role: grantTestRole, Privileges: []string{grantTestCreate}}, // system schema: never granted
			}})
			Expect(reconcileSchemaGrants(ctx, f, db, testChecker(f, nil))).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				"grant CREATE,USAGE on schema app to app wgo=true",
				"grant USAGE on schema app to PUBLIC wgo=false",
			}))
			Expect(db.Status.ManagedSchemaGrants).To(Equal([]postgresv1alpha1.ManagedSchemaGrant{
				{Schema: grantTestRole, Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeUsage}},
				{Schema: grantTestRole, Role: grantTestRole, Privileges: []string{postgres.PrivilegeCreate, postgres.PrivilegeUsage}, WithGrantOption: true},
			}))
		})

		It("revokes removed privileges, grants and schemas it granted, with CASCADE after the grant option", func() {
			f := &fakeGrantClient{}
			db := newDB(postgresv1alpha1.SchemaSpec{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
				{Role: grantTestRole, Privileges: []string{grantTestUsage}},
			}})
			db.Status.ManagedSchemaGrants = []postgresv1alpha1.ManagedSchemaGrant{
				{Schema: grantTestRole, Role: grantTestRole, Privileges: []string{postgres.PrivilegeCreate, postgres.PrivilegeUsage}, WithGrantOption: true},
				{Schema: grantTestRole, Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeUsage}},
				{Schema: "gone_entry", Role: grantTestOther, Privileges: []string{postgres.PrivilegeUsage}},
			}
			Expect(reconcileSchemaGrants(ctx, f, db, testChecker(f, nil))).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				"grant USAGE on schema app to app wgo=false",
				"revoke USAGE on schema app from app optionOnly=true cascade=true",
				"revoke CREATE on schema app from app optionOnly=false cascade=true",
				"revoke USAGE on schema app from PUBLIC optionOnly=false cascade=false",
				"revoke USAGE on schema gone_entry from other optionOnly=false cascade=false",
			}))
			Expect(db.Status.ManagedSchemaGrants).To(Equal([]postgresv1alpha1.ManagedSchemaGrant{
				{Schema: grantTestRole, Role: grantTestRole, Privileges: []string{postgres.PrivilegeUsage}},
			}))
		})

		It("never revokes schema privileges it did not record", func() {
			f := &fakeGrantClient{}
			db := newDB(postgresv1alpha1.SchemaSpec{Name: grantTestRole})
			Expect(reconcileSchemaGrants(ctx, f, db, testChecker(f, nil))).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedSchemaGrants).To(BeNil())
		})

		It("forgets grants on schemas or to roles that no longer exist without revoking", func() {
			f := &fakeGrantClient{schemas: map[string]bool{grantTestRole: true}, roles: map[string]bool{}}
			db := newDB()
			db.Status.ManagedSchemaGrants = []postgresv1alpha1.ManagedSchemaGrant{
				{Schema: "gone_schema", Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeUsage}},
				{Schema: grantTestRole, Role: "dropped_role", Privileges: []string{postgres.PrivilegeUsage}},
			}
			Expect(reconcileSchemaGrants(ctx, f, db, testChecker(f, nil))).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedSchemaGrants).To(BeNil())
		})

		It("keeps pending revokes tracked when one fails", func() {
			f := &fakeGrantClient{failOn: "revoke USAGE on schema app from other"}
			db := newDB()
			db.Status.ManagedSchemaGrants = []postgresv1alpha1.ManagedSchemaGrant{
				{Schema: grantTestRole, Role: grantTestOther, Privileges: []string{postgres.PrivilegeUsage}},
			}
			Expect(reconcileSchemaGrants(ctx, f, db, testChecker(f, nil))).NotTo(Succeed())
			Expect(db.Status.ManagedSchemaGrants).To(HaveLen(1))
		})

		It("rejects invalid privileges and duplicates before issuing SQL", func() {
			f := &fakeGrantClient{}
			db := newDB(postgresv1alpha1.SchemaSpec{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
				{Role: grantTestRole, Privileges: []string{"USAGE; DROP SCHEMA app"}},
			}})
			Expect(reconcileSchemaGrants(ctx, f, db, testChecker(f, nil))).To(MatchError(ContainSubstring("invalid schema privilege")))
			db = newDB(postgresv1alpha1.SchemaSpec{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
				{Role: postgres.PublicGrantee, Privileges: []string{grantTestUsage}}, {Role: grantTestLowerPublic, Privileges: []string{grantTestCreate}},
			}})
			Expect(reconcileSchemaGrants(ctx, f, db, testChecker(f, nil))).To(MatchError(ContainSubstring("listed more than once")))
			db = newDB(postgresv1alpha1.SchemaSpec{Name: grantTestRole, Grants: []postgresv1alpha1.GrantSpec{
				{Role: postgres.PublicGrantee, Privileges: []string{grantTestUsage}, WithGrantOption: true},
			}})
			Expect(reconcileSchemaGrants(ctx, f, db, testChecker(f, nil))).To(MatchError(ContainSubstring("grant option cannot be granted to PUBLIC")))
			Expect(f.calls).To(BeEmpty())
		})

		It("refuses more schema grants than it tracks", func() {
			schemas := make([]postgresv1alpha1.SchemaSpec, 0, maxSchemaGrants/16+1)
			for i := range maxSchemaGrants/16 + 1 {
				s := postgresv1alpha1.SchemaSpec{Name: fmt.Sprintf("s%d", i)}
				for j := range 16 {
					s.Grants = append(s.Grants, postgresv1alpha1.GrantSpec{Role: fmt.Sprintf("r%d", j), Privileges: []string{grantTestUsage}})
				}
				schemas = append(schemas, s)
			}
			f := &fakeGrantClient{}
			err := reconcileSchemaGrants(ctx, f, newDB(schemas...), testChecker(f, nil))
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
			Expect(ce.reason).To(Equal(ReasonTooManyGrants))
			Expect(f.calls).To(BeEmpty())
		})
	})

	Describe("two resources granting the same privilege", func() {
		It("keeps each Database's ledger to its own database", func() {
			f := &fakeGrantClient{}
			grant := []postgresv1alpha1.DatabaseGrantSpec{{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}}}
			a := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: grant}}
			b := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Grants: grant}}
			Expect(reconcileDatabaseGrants(ctx, f, a, "db_a", testChecker(f, nil))).To(Succeed())
			Expect(reconcileDatabaseGrants(ctx, f, b, "db_b", testChecker(f, nil))).To(Succeed())

			By("removing the grant from one of them")
			a.Spec.Grants = nil
			f.calls = nil
			Expect(reconcileDatabaseGrants(ctx, f, a, "db_a", testChecker(f, nil))).To(Succeed())
			Expect(reconcileDatabaseGrants(ctx, f, b, "db_b", testChecker(f, nil))).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				"revoke CONNECT on db_a from app optionOnly=false cascade=false",
				"grant CONNECT on db_b to app wgo=false",
			}))
		})

		It("lets the resource that still declares a grant restore it on its next reconcile", func() {
			// Should two ledgers ever hold the same key, the one that still
			// declares the privilege re-grants it: grant wins.
			f := &fakeGrantClient{}
			desired := []privilegeGrant{{Target: tgt(grantTestRole), Privileges: []string{postgres.PrivilegeConnect}}}
			ops := executorOps(f, nil)
			_, err := applyPrivilegeGrants(ctx, nil, desired, ops) // A removed it: revoke
			Expect(err).NotTo(HaveOccurred())
			_, err = applyPrivilegeGrants(ctx, desired, desired, ops) // B still declares it: grant
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{
				"revoke CONNECT on db from app optionOnly=false cascade=false",
				"grant CONNECT on db to app wgo=false",
			}))
		})
	})

	Describe("reconcilePublicPrivileges", func() {
		f := func() *fakeGrantClient {
			return &fakeGrantClient{public: map[string][]string{
				"DATABASE app_db": {postgres.PrivilegeConnect, postgres.PrivilegeTemporary},
				"SCHEMA public":   {postgres.PrivilegeUsage},
			}}
		}
		revoke := new(false)

		It("is a no-op when nothing is requested or recorded", func() {
			c := f()
			db := &postgresv1alpha1.Database{}
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase)).To(Succeed())
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema)).To(Succeed())
			Expect(c.calls).To(BeEmpty())
		})

		It("revokes only what PUBLIC holds, records it and grants exactly that back", func() {
			c := f()
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{PublicPrivileges: &postgresv1alpha1.PublicPrivilegesSpec{
				Connect: revoke, Temporary: revoke, PublicSchemaUsage: revoke, PublicSchemaCreate: revoke,
			}}}
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase)).To(Succeed())
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema)).To(Succeed())
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
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase)).To(Succeed())
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema)).To(Succeed())
			Expect(c.calls).To(BeEmpty())

			By("revoking again what was granted back outside pgop")
			c.public["DATABASE app_db"] = []string{postgres.PrivilegeConnect}
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase)).To(Succeed())
			Expect(c.calls).To(Equal([]string{callRevokeConnectPublic}))

			By("granting back what it revoked once the spec stops asking")
			c.calls = nil
			db.Spec.PublicPrivileges = &postgresv1alpha1.PublicPrivilegesSpec{Temporary: revoke, PublicSchemaUsage: new(true)}
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase)).To(Succeed())
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema)).To(Succeed())
			Expect(c.calls).To(Equal([]string{
				"grant CONNECT on app_db to PUBLIC wgo=false",
				"grant USAGE on schema public to PUBLIC wgo=false",
			}))
			Expect(db.Status.RevokedPublicPrivileges).To(Equal([]postgresv1alpha1.PublicPrivilege{postgresv1alpha1.PublicPrivilegeTemporary}))
		})

		It("forgets a recorded revoke on a schema public that was dropped", func() {
			c := &fakeGrantClient{public: map[string][]string{}}
			db := &postgresv1alpha1.Database{}
			db.Status.RevokedPublicPrivileges = []postgresv1alpha1.PublicPrivilege{postgresv1alpha1.PublicPrivilegePublicSchemaUsage}
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema)).To(Succeed())
			Expect(c.calls).To(BeEmpty())
			Expect(db.Status.RevokedPublicPrivileges).To(BeNil())
		})

		It("leaves a privilege alone that the spec also grants to PUBLIC", func() {
			c := f()
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{
				PublicPrivileges: &postgresv1alpha1.PublicPrivilegesSpec{Connect: revoke, Temporary: revoke, PublicSchemaUsage: revoke},
				Grants:           []postgresv1alpha1.DatabaseGrantSpec{{Role: postgres.PublicGrantee, Privileges: []string{postgres.PrivilegeAll}}},
				Schemas: []postgresv1alpha1.SchemaSpec{{Name: publicSchemaName, Grants: []postgresv1alpha1.GrantSpec{
					{Role: postgres.PublicGrantee, Privileges: []string{grantTestUsage}},
				}}},
			}}
			err := reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectDatabase)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
			Expect(ce.reason).To(Equal(ReasonPublicPrivilegeConflict))
			Expect(err.Error()).To(ContainSubstring("connect, temporary"))
			Expect(reconcilePublicPrivileges(ctx, c, db, "app_db", postgres.ObjectSchema)).To(MatchError(ContainSubstring("publicSchemaUsage")))
			Expect(c.calls).To(BeEmpty())
		})
	})

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
			Expect(reconcileParameterGrants(ctx, f, newRole(nil), grantTestRole)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
		})

		It("grants SET by default, revokes removed grants and records them", func() {
			f := &fakeGrantClient{version: 180001}
			role := newRole([]postgresv1alpha1.ParameterGrantSpec{
				{Parameter: "Log_Statement"},
				{Parameter: grantTestParam, Privileges: []string{postgres.PrivilegeSet}, WithGrantOption: true},
			}, postgresv1alpha1.ManagedParameterGrant{Parameter: testWorkMem, Privileges: []string{postgres.PrivilegeSet}})
			Expect(reconcileParameterGrants(ctx, f, role, grantTestRole)).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				"grant SET on parameter log_statement to app wgo=false",
				"grant SET on parameter myapp.tenant to app wgo=true",
				"revoke SET on parameter work_mem from app optionOnly=false cascade=false",
			}))
			Expect(role.Status.ManagedParameterGrants).To(Equal([]postgresv1alpha1.ManagedParameterGrant{
				{Parameter: grantTestSuperuserParam, Privileges: []string{postgres.PrivilegeSet}},
				{Parameter: grantTestParam, Privileges: []string{postgres.PrivilegeSet}, WithGrantOption: true},
			}))
		})

		It("reports UnsupportedServerVersion before PostgreSQL 15", func() {
			f := &fakeGrantClient{version: 140010}
			role := newRole([]postgresv1alpha1.ParameterGrantSpec{{Parameter: testWorkMem}})
			err := reconcileParameterGrants(ctx, f, role, grantTestRole)
			Expect(err).To(MatchError(ContainSubstring("require PostgreSQL 15 or later")))
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue())
			Expect(ce.reason).To(Equal(ReasonUnsupportedServerVersion))
			Expect(f.calls).To(BeEmpty())
		})

		It("rejects ALTER SYSTEM", func() {
			f := &fakeGrantClient{version: 180001}
			role := newRole([]postgresv1alpha1.ParameterGrantSpec{{Parameter: testWorkMem, Privileges: []string{"ALTER SYSTEM"}}})
			Expect(reconcileParameterGrants(ctx, f, role, grantTestRole)).To(MatchError(ContainSubstring("invalid parameter privilege")))
			Expect(f.calls).To(BeEmpty())
		})

		It("refuses denylisted parameters, grants the rest and revokes a managed denied grant", func() {
			f := &fakeGrantClient{version: 180001}
			role := newRole([]postgresv1alpha1.ParameterGrantSpec{
				{Parameter: grantTestRoleParam}, {Parameter: "Session_Authorization"}, {Parameter: testWorkMem},
			}, postgresv1alpha1.ManagedParameterGrant{Parameter: grantTestReplicationRole, Privileges: []string{postgres.PrivilegeSet}})
			err := reconcileParameterGrants(ctx, f, role, grantTestRole)
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
			err := reconcileParameterGrants(ctx, f, role, grantTestRole)
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
		Entry("operator role", "pgop_operator"),
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
