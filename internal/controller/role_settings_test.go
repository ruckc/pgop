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

// Calls recorded by the settings tests.
const (
	setWorkMemAppDB     = "set work_mem on app_db to 64MB"
	setWorkMemRoleAppDB = `set work_mem for app in "app_db" to 64MB`
)

// testSettingPolicy lets the settings tests set custom parameters in the
// myapp namespace (grantTestParam).
var testSettingPolicy = &postgresv1alpha1.RolePolicySpec{AllowedSettingPrefixes: []string{"myapp"}}

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
		pending, err := reconcileRoleSettings(ctx, f, newRole(nil), grantTestRole, testSettingPolicy)
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
		pending, err := reconcileRoleSettings(ctx, f, role, grantTestRole, testSettingPolicy)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeEmpty())
		Expect(f.calls).To(Equal([]string{
			`set myapp.tenant for app in "" to acme`,
			`set statement_timeout for app in "" to 30s`,
			`reset lock_timeout for app in ""`,
			setWorkMemRoleAppDB,
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
		pending, err := reconcileRoleSettings(ctx, f, role, grantTestRole, testSettingPolicy)
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
		pending, err := reconcileRoleSettings(ctx, f, role, grantTestRole, testSettingPolicy)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(Equal([]string{otherDB}))
		Expect(f.calls).To(Equal([]string{setWorkMemRoleAppDB}))
		Expect(role.Status.ManagedDatabaseSettings).To(Equal([]postgresv1alpha1.ManagedRoleDatabaseSettings{
			{Database: appDB, Settings: []string{testWorkMem}},
		}))
	})

	It("keeps a setting tracked when its reset fails and still handles other databases", func() {
		f := &fakeRoleSettingsClient{fakeGrantClient: fakeGrantClient{failOn: `reset work_mem for app in ""`}}
		role := newRole(nil,
			postgresv1alpha1.RoleDatabaseSettings{Database: appDB, Settings: map[string]string{testWorkMem: grantTestValue}})
		role.Status.ManagedSettings = []string{testWorkMem}
		_, err := reconcileRoleSettings(ctx, f, role, grantTestRole, testSettingPolicy)
		Expect(err).To(MatchError(ContainSubstring("boom")))
		Expect(role.Status.ManagedSettings).To(Equal([]string{testWorkMem}))
		Expect(role.Status.ManagedDatabaseSettings).To(Equal([]postgresv1alpha1.ManagedRoleDatabaseSettings{
			{Database: appDB, Settings: []string{testWorkMem}},
		}))
	})

	It("rejects invalid and case-duplicate parameter names before running anything", func() {
		f := &fakeRoleSettingsClient{}
		_, err := reconcileRoleSettings(ctx, f, newRole(map[string]string{"work_mem; ALTER ROLE app SUPERUSER": "1"}), grantTestRole, testSettingPolicy)
		Expect(err).To(MatchError(ContainSubstring("invalid parameter name")))
		_, err = reconcileRoleSettings(ctx, f, newRole(nil, postgresv1alpha1.RoleDatabaseSettings{
			Database: appDB, Settings: map[string]string{grantTestMixedCase: "1", testWorkMem: "2"}}), grantTestRole, testSettingPolicy)
		Expect(err).To(MatchError(ContainSubstring("more than once")))
		_, err = reconcileRoleSettings(ctx, f, newRole(nil,
			postgresv1alpha1.RoleDatabaseSettings{Database: appDB},
			postgresv1alpha1.RoleDatabaseSettings{Database: appDB}), grantTestRole, testSettingPolicy)
		Expect(err).To(MatchError(ContainSubstring("listed more than once")))
		Expect(f.calls).To(BeEmpty())
	})

	It("ignores database names longer than PostgreSQL allows and applies the rest", func() {
		f := &fakeRoleSettingsClient{}
		long := strings.Repeat("é", 32) // 32 characters, 64 bytes
		role := newRole(nil,
			postgresv1alpha1.RoleDatabaseSettings{Database: long, Settings: map[string]string{testWorkMem: grantTestValue}},
			postgresv1alpha1.RoleDatabaseSettings{Database: appDB, Settings: map[string]string{testWorkMem: grantTestValue}})
		pending, err := reconcileRoleSettings(ctx, f, role, grantTestRole, testSettingPolicy)
		Expect(err).To(MatchError(ContainSubstring("longer than 63 bytes")))
		Expect(pending).To(BeEmpty())
		Expect(f.calls).To(Equal([]string{setWorkMemRoleAppDB}))
	})

	It("never touches reserved roles", func() {
		for _, name := range []string{bootstrapRoleName, DefaultOperatorUsername, "pg_monitor"} {
			f := &fakeRoleSettingsClient{}
			_, err := reconcileRoleSettings(ctx, f, newRole(map[string]string{testWorkMem: grantTestValue}), name, testSettingPolicy)
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

	It("limits database names to 63 bytes", func() {
		expectInvalid(create(postgresv1alpha1.RoleSpec{DatabaseSettings: []postgresv1alpha1.RoleDatabaseSettings{
			{Database: strings.Repeat("é", 32)}}}))
		Expect(create(postgresv1alpha1.RoleSpec{DatabaseSettings: []postgresv1alpha1.RoleDatabaseSettings{
			{Database: strings.Repeat("é", 31) + "x"}}})).To(Succeed())
	})

	Context("Cluster spec.rolePolicy.allowedSettingPrefixes", func() {
		newCluster := func(prefixes ...string) *postgresv1alpha1.Cluster {
			return &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("prefixes-%d", time.Now().UnixNano()), Namespace: ns},
				Spec: postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage,
					RolePolicy: &postgresv1alpha1.RolePolicySpec{AllowedSettingPrefixes: prefixes}},
			}
		}
		createCluster := func(c *postgresv1alpha1.Cluster) error {
			err := k8sClient.Create(ctx, c)
			if err == nil {
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })
			}
			return err
		}

		It("accepts application namespaces", func() {
			Expect(createCluster(newCluster("myapp", "tenant_cfg", "_x"))).To(Succeed())
		})

		It("rejects every denied namespace", func() {
			for _, p := range postgresv1alpha1.DeniedSettingPrefixes {
				expectInvalid(createCluster(newCluster("myapp", p)))
			}
		})

		It("rejects malformed prefixes and too many entries", func() {
			for _, p := range []string{"MyApp", "myapp.", "my-app", "1app", "", strings.Repeat("a", 64)} {
				expectInvalid(createCluster(newCluster(p)))
			}
			many := make([]string, 33)
			for i := range many {
				many[i] = fmt.Sprintf("p%d", i)
			}
			expectInvalid(createCluster(newCluster(many...)))
		})
	})
})

var _ = Describe("Setting policy", func() {
	ctx := context.Background()
	f := &fakeGrantClient{contexts: map[string]string{
		testWorkMem: pgContextUser, grantTestSuperuserParam: pgContextSuperuser,
		// Extension parameters the operator's session knows (loaded in
		// every session) are checked by their context.
		"pg_trgm.similarity_threshold": pgContextUser, "auto_explain.log_min_duration": pgContextSuperuser,
	}}
	policy := &postgresv1alpha1.RolePolicySpec{AllowedSettingPrefixes: []string{"myapp", "plperl", "auto_explain"}}

	DescribeTable("decides per parameter",
		func(policy *postgresv1alpha1.RolePolicySpec, name, want string) {
			norm, err := postgres.NormalizeParameterName(name)
			Expect(err).NotTo(HaveOccurred())
			reason, err := settingAllowed(ctx, f, policy, norm)
			Expect(err).NotTo(HaveOccurred())
			if want == "" {
				Expect(reason).To(BeEmpty())
			} else {
				Expect(reason).To(ContainSubstring(want))
			}
		},
		Entry("user-context built-in", nil, testWorkMem, ""),
		Entry("superuser built-in", policy, grantTestSuperuserParam, `context "superuser"`),
		Entry("denylisted", policy, "Session_Replication_Role", "denylist"),
		Entry("unknown name without a dot (typo, PostgreSQL rejects it)", nil, "work_mme", ""),
		Entry("known user-context extension parameter", nil, "pg_trgm.similarity_threshold", ""),
		Entry("known superuser extension parameter", policy, "auto_explain.log_min_duration", `context "superuser"`),
		Entry("placeholder without a policy", nil, grantTestParam, "allowedSettingPrefixes"),
		Entry("placeholder with an empty policy", &postgresv1alpha1.RolePolicySpec{}, grantTestParam, "allowedSettingPrefixes"),
		Entry("placeholder in an allowed namespace", policy, grantTestParam, ""),
		Entry("placeholder in an allowed namespace, mixed case", policy, "MyApp.Tenant", ""),
		Entry("placeholder with many dots", policy, "myapp.a.b.c.d", ""),
		Entry("prefix match is per namespace, not per string", policy, "myapp2.tenant", "allowedSettingPrefixes"),
		Entry("allowed namespace only as a later part", policy, "other.myapp.tenant", "allowedSettingPrefixes"),
		Entry("denied namespace even when listed", policy, "plperl.on_plperl_init", "never sets"),
		Entry("denied namespace, mixed case", policy, "PlPerl.on_plperl_init", "never sets"),
		Entry("denied namespace, unloaded auto_explain", policy, "auto_explain.log_analyze", "never sets"),
		Entry("plpgsql (variable_conflict is superuser-only)", policy, "plpgsql.variable_conflict", "never sets"),
		Entry("postgis file access", policy, "postgis.gdal_enabled_drivers", "never sets"),
		Entry("pgaudit (static denylist)", policy, "pgaudit.log", "denylist"),
	)

	It("rejects names that are not ASCII identifiers before deciding", func() {
		for _, name := range []string{"plperl .on_init", " myapp.tenant", "myapp.tenänt", "ｍyapp.x", "myapp..x", "myapp."} {
			_, err := postgres.NormalizeParameterName(name)
			Expect(err).To(HaveOccurred(), name)
		}
	})

	It("never allows a denied namespace through the API helper", func() {
		for _, p := range postgresv1alpha1.DeniedSettingPrefixes {
			Expect((&postgresv1alpha1.RolePolicySpec{AllowedSettingPrefixes: []string{p}}).AllowsSettingPrefix(p)).To(BeFalse(), p)
		}
	})

	It("resets a managed placeholder once its namespace is no longer allowed", func() {
		g := &fakeGrantClient{}
		db := &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{
			Settings: map[string]string{grantTestParam: grantTestTenant, testWorkMem: grantTestValue}}}
		db.Status.ManagedSettings = []string{grantTestParam}
		err := reconcileDatabaseSettings(ctx, g, db, "app_db", nil)
		ce, ok := errors.AsType[*conditionError](err)
		Expect(ok).To(BeTrue(), "expected a conditionError, got %v", err)
		Expect(ce.reason).To(Equal(ReasonSettingNotAllowed))
		Expect(err.Error()).To(ContainSubstring(grantTestParam))
		Expect(g.calls).To(Equal([]string{setWorkMemAppDB, "reset myapp.tenant on app_db"}))
		Expect(db.Status.ManagedSettings).To(Equal([]string{testWorkMem}))

		r := &fakeRoleSettingsClient{}
		role := &postgresv1alpha1.Role{Spec: postgresv1alpha1.RoleSpec{Settings: map[string]string{grantTestParam: grantTestTenant}}}
		role.Status.ManagedSettings = []string{grantTestParam}
		_, err = reconcileRoleSettings(ctx, r, role, grantTestRole, &postgresv1alpha1.RolePolicySpec{})
		Expect(err).To(MatchError(ContainSubstring("allowedSettingPrefixes")))
		Expect(r.calls).To(Equal([]string{`reset myapp.tenant for app in ""`}))
		Expect(role.Status.ManagedSettings).To(BeEmpty())
	})
})

var _ = Describe("Pending databaseSettings backoff", func() {
	It("doubles from 30s up to 5m with at most 20% jitter", func() {
		want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
		for i, w := range want {
			Expect(backoffDelay(i, 0)).To(Equal(w))
			Expect(backoffDelay(i, 0.999)).To(BeNumerically("<", w+w/5))
		}
		Expect(backoffDelay(100, 0)).To(Equal(pendingSettingsMaxRequeue))
	})

	It("counts attempts per Role and resets", func() {
		var b requeueBackoff
		Expect(b.next("a")).To(BeNumerically("<", 36*time.Second))
		Expect(b.next("a")).To(BeNumerically(">=", time.Minute))
		Expect(b.next("b")).To(BeNumerically("<", 36*time.Second))
		b.reset("a")
		Expect(b.next("a")).To(BeNumerically("<", 36*time.Second))
	})
})
