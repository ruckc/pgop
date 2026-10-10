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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Names used by the role settings tests.
const (
	appDB                = "app_db"
	otherDB              = "other_db"
	goneDB               = "gone_db"
	rsStatementTimeout   = "statement_timeout"
	rsLogConnections     = "log_connections"
	rsNone               = "none"
	rsSessionAuth        = "Session_Authorization"
	rsLoCompatPrivileges = "lo_compat_privileges"
)

// fakeRoleSettingsClient records role settings statements.
type fakeRoleSettingsClient struct {
	fakeGrantClient
	// databases are the existing databases; nil means every database exists.
	databases map[string]bool
}

func (f *fakeRoleSettingsClient) DatabaseExists(_ context.Context, name string) (bool, error) {
	return f.databases == nil || f.databases[name], nil
}

func (f *fakeRoleSettingsClient) SetRoleParameter(_ context.Context, role, db, name, value string) error {
	return f.record(fmt.Sprintf("set %s for %s in %q to %s", name, role, db, value))
}

func (f *fakeRoleSettingsClient) ResetRoleParameter(_ context.Context, role, db, name string) error {
	return f.record(fmt.Sprintf("reset %s for %s in %q", name, role, db))
}

var _ = Describe("Role settings", func() {
	ctx := context.Background()

	newRole := func(settings map[string]string, dbSettings ...postgresv1alpha1.RoleDatabaseSettings) *postgresv1alpha1.Role {
		return &postgresv1alpha1.Role{Spec: postgresv1alpha1.RoleSpec{Settings: settings, DatabaseSettings: dbSettings}}
	}

	It("does nothing without settings or a ledger", func() {
		f := &fakeRoleSettingsClient{}
		pending, err := reconcileRoleSettings(ctx, f, newRole(nil), grantTestRole)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeEmpty())
		Expect(f.calls).To(BeEmpty())
	})

	It("sets role and per-database settings, resets removed managed ones and records them", func() {
		f := &fakeRoleSettingsClient{}
		role := newRole(map[string]string{"Statement_Timeout": "30s", grantTestParam: grantTestTenant},
			postgresv1alpha1.RoleDatabaseSettings{Database: appDB, Settings: map[string]string{testWorkMem: grantTestValue}})
		role.Status.ManagedSettings = []string{"lock_timeout", rsStatementTimeout}
		role.Status.ManagedDatabaseSettings = []postgresv1alpha1.ManagedRoleDatabaseSettings{
			{Database: appDB, Settings: []string{"temp_buffers", testWorkMem}},
			{Database: otherDB, Settings: []string{testWorkMem}},
		}
		pending, err := reconcileRoleSettings(ctx, f, role, grantTestRole)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeEmpty())
		Expect(f.calls).To(Equal([]string{
			`set myapp.tenant for app in "" to acme`,
			`set statement_timeout for app in "" to 30s`,
			`reset lock_timeout for app in ""`,
			`set work_mem for app in "app_db" to 64MB`,
			`reset temp_buffers for app in "app_db"`,
			`reset work_mem for app in "other_db"`,
		}))
		Expect(role.Status.ManagedSettings).To(Equal([]string{grantTestParam, rsStatementTimeout}))
		Expect(role.Status.ManagedDatabaseSettings).To(Equal([]postgresv1alpha1.ManagedRoleDatabaseSettings{
			{Database: appDB, Settings: []string{testWorkMem}},
		}))
	})

	It("refuses denylisted and non-user parameters but applies the rest", func() {
		f := &fakeRoleSettingsClient{fakeGrantClient: fakeGrantClient{contexts: map[string]string{
			testWorkMem: pgContextUser, grantTestSuperuserParam: pgContextSuperuser,
			grantTestPostmasterParam: pgContextPostmaster, rsLogConnections: "superuser-backend",
		}}}
		role := newRole(map[string]string{
			testWorkMem: grantTestValue, grantTestSuperuserParam: rsNone, grantTestPostmasterParam: "1GB",
			rsLogConnections: "off", rsSessionAuth: bootstrapRoleName, grantTestRoleParam: bootstrapRoleName,
			rsLoCompatPrivileges: "on", grantTestAuditParam: rsNone, "shared_preload_libraries": "x",
		}, postgresv1alpha1.RoleDatabaseSettings{Database: appDB, Settings: map[string]string{
			grantTestReplicationRole: "replica", grantTestParam: grantTestTenant,
		}})
		// Settings applied before they became disallowed are reset.
		role.Status.ManagedSettings = []string{grantTestSuperuserParam}
		role.Status.ManagedDatabaseSettings = []postgresv1alpha1.ManagedRoleDatabaseSettings{
			{Database: appDB, Settings: []string{grantTestReplicationRole}},
		}
		pending, err := reconcileRoleSettings(ctx, f, role, grantTestRole)
		Expect(pending).To(BeEmpty())
		ce, ok := errors.AsType[*conditionError](err)
		Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
		Expect(ce.reason).To(Equal(ReasonSettingNotAllowed))
		Expect(err.Error()).To(SatisfyAll(
			ContainSubstring(`settings: log_statement has context "superuser"`),
			ContainSubstring(`settings: shared_buffers has context "postmaster"`),
			ContainSubstring(`settings: log_connections has context "superuser-backend"`),
			ContainSubstring("settings: session_authorization is on pgop's denylist"),
			ContainSubstring("settings: role is on pgop's denylist"),
			ContainSubstring("settings: lo_compat_privileges is on pgop's denylist"),
			ContainSubstring("settings: pgaudit.log is on pgop's denylist"),
			ContainSubstring("settings: shared_preload_libraries is on pgop's denylist"),
			ContainSubstring("databaseSettings[app_db]: session_replication_role is on pgop's denylist"),
		))
		Expect(f.calls).To(Equal([]string{
			`set work_mem for app in "" to 64MB`,
			`reset log_statement for app in ""`,
			`set myapp.tenant for app in "app_db" to acme`,
			`reset session_replication_role for app in "app_db"`,
		}))
		Expect(role.Status.ManagedSettings).To(Equal([]string{testWorkMem}))
		Expect(role.Status.ManagedDatabaseSettings).To(Equal([]postgresv1alpha1.ManagedRoleDatabaseSettings{
			{Database: appDB, Settings: []string{grantTestParam}},
		}))
	})

	It("reports databases that do not exist yet as pending and forgets settings in dropped ones", func() {
		f := &fakeRoleSettingsClient{databases: map[string]bool{appDB: true}}
		role := newRole(nil,
			postgresv1alpha1.RoleDatabaseSettings{Database: appDB, Settings: map[string]string{testWorkMem: grantTestValue}},
			postgresv1alpha1.RoleDatabaseSettings{Database: otherDB, Settings: map[string]string{testWorkMem: grantTestValue}},
		)
		role.Status.ManagedDatabaseSettings = []postgresv1alpha1.ManagedRoleDatabaseSettings{
			{Database: goneDB, Settings: []string{testWorkMem}},
		}
		pending, err := reconcileRoleSettings(ctx, f, role, grantTestRole)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(Equal([]string{otherDB}))
		Expect(f.calls).To(Equal([]string{`set work_mem for app in "app_db" to 64MB`}))
		Expect(role.Status.ManagedDatabaseSettings).To(Equal([]postgresv1alpha1.ManagedRoleDatabaseSettings{
			{Database: appDB, Settings: []string{testWorkMem}},
		}))
	})

	It("keeps a setting tracked when its reset fails and still handles other databases", func() {
		f := &fakeRoleSettingsClient{fakeGrantClient: fakeGrantClient{failOn: `reset work_mem for app in ""`}}
		role := newRole(nil,
			postgresv1alpha1.RoleDatabaseSettings{Database: appDB, Settings: map[string]string{testWorkMem: grantTestValue}})
		role.Status.ManagedSettings = []string{testWorkMem}
		_, err := reconcileRoleSettings(ctx, f, role, grantTestRole)
		Expect(err).To(MatchError(ContainSubstring("boom")))
		Expect(role.Status.ManagedSettings).To(Equal([]string{testWorkMem}))
		Expect(role.Status.ManagedDatabaseSettings).To(Equal([]postgresv1alpha1.ManagedRoleDatabaseSettings{
			{Database: appDB, Settings: []string{testWorkMem}},
		}))
	})

	It("rejects invalid and case-duplicate parameter names before running anything", func() {
		f := &fakeRoleSettingsClient{}
		_, err := reconcileRoleSettings(ctx, f, newRole(map[string]string{"work_mem; ALTER ROLE app SUPERUSER": "1"}), grantTestRole)
		Expect(err).To(MatchError(ContainSubstring("invalid parameter name")))
		_, err = reconcileRoleSettings(ctx, f, newRole(nil, postgresv1alpha1.RoleDatabaseSettings{
			Database: appDB, Settings: map[string]string{grantTestMixedCase: "1", testWorkMem: "2"}}), grantTestRole)
		Expect(err).To(MatchError(ContainSubstring("more than once")))
		_, err = reconcileRoleSettings(ctx, f, newRole(nil,
			postgresv1alpha1.RoleDatabaseSettings{Database: appDB},
			postgresv1alpha1.RoleDatabaseSettings{Database: appDB}), grantTestRole)
		Expect(err).To(MatchError(ContainSubstring("listed more than once")))
		Expect(f.calls).To(BeEmpty())
	})

	It("never touches reserved roles", func() {
		for _, name := range []string{bootstrapRoleName, DefaultOperatorUsername, "pg_monitor"} {
			f := &fakeRoleSettingsClient{}
			_, err := reconcileRoleSettings(ctx, f, newRole(map[string]string{testWorkMem: grantTestValue}), name)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue(), "%s: expected a conditionError, got %v", name, err)
			Expect(ce.reason).To(Equal(ReasonReservedName))
			Expect(f.calls).To(BeEmpty())
		}
	})

	It("forgets a ledger written for another role or Cluster", func() {
		role := newRole(nil)
		role.Status.RoleName = grantTestRole
		role.Status.ClusterUID = "uid-1"
		role.Status.ManagedSettings = []string{testWorkMem}
		role.Status.ManagedDatabaseSettings = []postgresv1alpha1.ManagedRoleDatabaseSettings{{Database: appDB, Settings: []string{testWorkMem}}}
		clearStaleRoleSettings(role, grantTestRole, "uid-1")
		Expect(role.Status.ManagedSettings).NotTo(BeEmpty())
		clearStaleRoleSettings(role, grantTestRole, "uid-2")
		Expect(role.Status.ManagedSettings).To(BeEmpty())
		Expect(role.Status.ManagedDatabaseSettings).To(BeEmpty())
	})
})

// CRD validation of Role spec.settings and spec.databaseSettings.
var _ = Describe("Role settings CRD validation", func() {
	const ns = "default"
	ctx := context.Background()

	// deniedSettings must be rejected by the API server and refused by
	// postgres.DeniedParameter alike.
	deniedSettings := []string{
		grantTestRoleParam, rsSessionAuth, "session_preload_libraries", "local_preload_libraries",
		"shared_preload_libraries", "dynamic_library_path", "jit_provider", "session_replication_role",
		rsLoCompatPrivileges, grantTestAuditParam, "set_user.block", "anon.salt", "sepgsql.permissive",
	}

	create := func(spec postgresv1alpha1.RoleSpec) error {
		spec.ClusterRef = postgresv1alpha1.ClusterReference{Name: nonexistentCluster}
		role := &postgresv1alpha1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("settings-%d", time.Now().UnixNano()), Namespace: ns},
			Spec:       spec,
		}
		err := k8sClient.Create(ctx, role)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, role) })
		}
		return err
	}
	expectInvalid := func(err error) {
		ExpectWithOffset(1, apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
	}
	inDB := func(settings map[string]string) []postgresv1alpha1.RoleDatabaseSettings {
		return []postgresv1alpha1.RoleDatabaseSettings{{Database: appDB, Settings: settings}}
	}

	It("accepts user and custom parameters", func() {
		settings := map[string]string{rsStatementTimeout: "30s", grantTestSearchPath: `"$user", app`, grantTestParam: grantTestTenant}
		Expect(create(postgresv1alpha1.RoleSpec{Settings: settings, DatabaseSettings: inDB(settings)})).To(Succeed())
	})

	for _, name := range deniedSettings {
		It(fmt.Sprintf("rejects the denylisted parameter %q", name), func() {
			Expect(postgres.DeniedParameter(name)).To(BeTrue(), "the CRD and DeniedParameter must agree")
			expectInvalid(create(postgresv1alpha1.RoleSpec{Settings: map[string]string{name: "x"}}))
			expectInvalid(create(postgresv1alpha1.RoleSpec{DatabaseSettings: inDB(map[string]string{name: "x"})}))
		})
	}

	It("rejects invalid keys, long values and duplicate databases", func() {
		for _, settings := range []map[string]string{
			{"work_mem; ALTER ROLE x SUPERUSER": "1"},
			{"a..b": "1"},
			{strings.Repeat("a", 128): "1"},
			{testWorkMem: strings.Repeat("x", 4097)},
		} {
			expectInvalid(create(postgresv1alpha1.RoleSpec{Settings: settings}))
			expectInvalid(create(postgresv1alpha1.RoleSpec{DatabaseSettings: inDB(settings)}))
		}
		expectInvalid(create(postgresv1alpha1.RoleSpec{DatabaseSettings: []postgresv1alpha1.RoleDatabaseSettings{
			{Database: ""}}}))
		expectInvalid(create(postgresv1alpha1.RoleSpec{DatabaseSettings: []postgresv1alpha1.RoleDatabaseSettings{
			{Database: appDB}, {Database: appDB}}}))
	})
})
