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
	grantTestRole       = "app"
	grantTestParam      = "myapp.tenant"
	grantTestMixedCase  = "Work_Mem"
	grantTestAdmin      = "admin"
	grantTestSearchPath = "search_path"
)

// fakeGrantClient records statements issued for database grants, database
// settings and parameter grants.
type fakeGrantClient struct {
	version int
	roles   map[string]bool // existing roles; nil means every role exists
	failOn  string          // substring of a recorded call that fails
	calls   []string
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

func (f *fakeGrantClient) GrantDatabasePrivileges(_ context.Context, db, role string, privs []string, wgo bool) error {
	return f.record(fmt.Sprintf("grant %s on %s to %s wgo=%t", strings.Join(privs, ","), db, role, wgo))
}

func (f *fakeGrantClient) RevokeDatabasePrivileges(_ context.Context, db, role string, privs []string, gOnly bool) error {
	return f.record(fmt.Sprintf("revoke %s on %s from %s optionOnly=%t", strings.Join(privs, ","), db, role, gOnly))
}

func (f *fakeGrantClient) SetDatabaseParameter(_ context.Context, db, name, value string) error {
	return f.record(fmt.Sprintf("set %s on %s to %s", name, db, value))
}

func (f *fakeGrantClient) ResetDatabaseParameter(_ context.Context, db, name string) error {
	return f.record(fmt.Sprintf("reset %s on %s", name, db))
}

func (f *fakeGrantClient) GrantParameterPrivileges(_ context.Context, param, role string, privs []string, wgo bool) error {
	return f.record(fmt.Sprintf("grant %s on parameter %s to %s wgo=%t", strings.Join(privs, ","), param, role, wgo))
}

func (f *fakeGrantClient) RevokeParameterPrivileges(_ context.Context, param, role string, privs []string, gOnly bool) error {
	return f.record(fmt.Sprintf("revoke %s on parameter %s from %s optionOnly=%t", strings.Join(privs, ","), param, role, gOnly))
}

var _ = Describe("Privilege grants", func() {
	type pg = privilegeGrant
	ctx := context.Background()

	Describe("diffPrivilegeGrants", func() {
		It("grants every desired grant", func() {
			desired := []pg{{Key: "a", Privileges: []string{postgres.PrivilegeConnect}}}
			plan := diffPrivilegeGrants(desired, nil)
			Expect(plan.Grant).To(Equal(desired))
			Expect(plan.Revoke).To(BeEmpty())
			Expect(plan.RevokeGrantOption).To(BeEmpty())
		})

		It("revokes managed grants removed from the spec", func() {
			managed := []pg{{Key: "gone", Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}}}
			plan := diffPrivilegeGrants(nil, managed)
			Expect(plan.Revoke).To(Equal(managed))
		})

		It("revokes only managed privileges removed from a grant", func() {
			plan := diffPrivilegeGrants(
				[]pg{{Key: "a", Privileges: []string{postgres.PrivilegeConnect}}},
				[]pg{{Key: "a", Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}}})
			Expect(plan.Revoke).To(Equal([]pg{{Key: "a", Privileges: []string{postgres.PrivilegeCreate}}}))
		})

		It("revokes the grant option it granted once it is turned off", func() {
			plan := diffPrivilegeGrants(
				[]pg{{Key: "a", Privileges: []string{postgres.PrivilegeConnect}}},
				[]pg{{Key: "a", Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeCreate}, WithGrantOption: true}})
			Expect(plan.RevokeGrantOption).To(Equal([]pg{{Key: "a", Privileges: []string{postgres.PrivilegeConnect}}}))
			Expect(plan.Revoke).To(Equal([]pg{{Key: "a", Privileges: []string{postgres.PrivilegeCreate}}}))
		})

		It("never revokes privileges it did not grant", func() {
			plan := diffPrivilegeGrants([]pg{{Key: "a", Privileges: []string{postgres.PrivilegeConnect}, WithGrantOption: false}}, nil)
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
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db")).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedGrants).To(BeNil())
		})

		It("grants normalized privileges and records them", func() {
			f := &fakeGrantClient{}
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect, postgres.PrivilegeTemp}},
				{Role: grantTestAdmin, Privileges: []string{postgres.PrivilegeAll}, WithGrantOption: true},
			})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db")).To(Succeed())
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
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db")).To(Succeed())
			Expect(f.calls).To(Equal([]string{
				"grant CONNECT on app_db to app wgo=false",
				"revoke CONNECT on app_db from app optionOnly=true",
				"revoke CREATE on app_db from app optionOnly=false",
				"revoke CONNECT on app_db from old optionOnly=false",
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
			err := reconcileDatabaseGrants(ctx, f, db, "app_db")
			Expect(err).To(MatchError(ContainSubstring(`PostgreSQL role "later" does not exist yet`)))
			Expect(db.Status.ManagedGrants).To(Equal([]postgresv1alpha1.ManagedDatabaseGrant{
				{Role: grantTestRole, Privileges: []string{postgres.PrivilegeConnect}},
			}))
		})

		It("forgets a managed grant whose role was dropped without revoking", func() {
			f := &fakeGrantClient{roles: map[string]bool{}}
			db := newDB(nil, postgresv1alpha1.ManagedDatabaseGrant{Role: "dropped", Privileges: []string{postgres.PrivilegeConnect}})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db")).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedGrants).To(BeNil())
		})

		It("keeps a pending revoke tracked when it fails", func() {
			f := &fakeGrantClient{failOn: "from old"}
			db := newDB(nil, postgresv1alpha1.ManagedDatabaseGrant{Role: "old", Privileges: []string{postgres.PrivilegeConnect}})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db")).NotTo(Succeed())
			Expect(db.Status.ManagedGrants).To(HaveLen(1))
		})

		It("rejects privileges outside the allow-list before issuing SQL", func() {
			f := &fakeGrantClient{}
			db := newDB([]postgresv1alpha1.DatabaseGrantSpec{
				{Role: grantTestRole, Privileges: []string{"CONNECT; DROP DATABASE app_db"}},
			})
			Expect(reconcileDatabaseGrants(ctx, f, db, "app_db")).To(MatchError(ContainSubstring("invalid database privilege")))
			Expect(f.calls).To(BeEmpty())
		})
	})

	Describe("reconcileDatabaseSettings", func() {
		It("sets desired settings, resets removed managed ones and records them", func() {
			f := &fakeGrantClient{}
			db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{
				Settings: map[string]string{testWorkMem: "64MB", "MyApp.Tenant": "acme"},
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
				"revoke SET on parameter work_mem from app optionOnly=false",
			}))
			Expect(role.Status.ManagedParameterGrants).To(Equal([]postgresv1alpha1.ManagedParameterGrant{
				{Parameter: "log_statement", Privileges: []string{postgres.PrivilegeSet}},
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

		It("revokes every managed grant before the role is dropped", func() {
			f := &fakeGrantClient{version: 180001}
			role := newRole([]postgresv1alpha1.ParameterGrantSpec{{Parameter: testWorkMem}},
				postgresv1alpha1.ManagedParameterGrant{Parameter: testWorkMem, Privileges: []string{postgres.PrivilegeSet}})
			Expect(revokeManagedParameterGrants(ctx, f, role, grantTestRole)).To(Succeed())
			Expect(f.calls).To(Equal([]string{"revoke SET on parameter work_mem from app optionOnly=false"}))
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
			Settings: map[string]string{testWorkMem: "64MB", grantTestParam: "acme", grantTestSearchPath: `"$user", app, public`},
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
			{Parameter: "log_statement"},
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
	)
})
