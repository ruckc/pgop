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
	"fmt"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Names used by the extension grant tests.
const (
	extPartman   = "pg_partman"
	extPartSch   = "partman"
	fnMaint      = "partman.run_maintenance()"
	fnDefiner    = "partman.check_name_length(text)"
	fnC          = "partman.c_helper(integer)"
	tblConfig    = "partman.part_config"
	viewConfig   = "partman.config_view"
	seqConfig    = "partman.part_config_id_seq"
	pgVersion16  = 160000
	pgVersion17  = 170000
	callGrantRun = "grant EXECUTE on routine partman.run_maintenance() to app"
)

// fakeMember is an extension member in fakeExtGrantClient.
type fakeMember struct {
	identity string
	eligible bool
}

// fakeExtGrantClient models extension members and their ACLs on top of
// fakeGrantClient (which handles the schema grants and roles).
type fakeExtGrantClient struct {
	*fakeGrantClient
	members map[string]map[postgres.MemberKind][]fakeMember
}

func newFakeExtGrantClient() *fakeExtGrantClient {
	return &fakeExtGrantClient{
		fakeGrantClient: &fakeGrantClient{},
		members: map[string]map[postgres.MemberKind][]fakeMember{extPartman: {
			postgres.MemberRoutines:  {{fnMaint, true}, {fnDefiner, false}, {fnC, false}},
			postgres.MemberTables:    {{tblConfig, true}, {viewConfig, false}},
			postgres.MemberSequences: {{seqConfig, true}},
		}},
	}
}

func memberObj(kind postgres.MemberKind, identity string) postgres.PrivilegeObject {
	return postgres.PrivilegeObject{Kind: postgres.ObjectKind(kind), Name: identity}
}

func (f *fakeExtGrantClient) InstalledExtension(_ context.Context, name string) (*postgres.InstalledExtension, error) {
	if _, ok := f.members[name]; !ok {
		return nil, nil
	}
	return &postgres.InstalledExtension{Name: name, Version: extV52, Schema: extPartSch}, nil
}

func (f *fakeExtGrantClient) ExtensionMembers(_ context.Context, ext string, kind postgres.MemberKind, grantee string) ([]postgres.ExtensionMember, error) {
	out := make([]postgres.ExtensionMember, 0, len(f.members[ext][kind]))
	for _, m := range f.members[ext][kind] {
		var held []string
		if e := f.acl[aclKey(memberObj(kind, m.identity), grantee)]; e != nil {
			held = slices.Clone(e.privileges)
		}
		out = append(out, postgres.ExtensionMember{Identity: m.identity, Eligible: m.eligible, Held: held})
	}
	return out, nil
}

func (f *fakeExtGrantClient) GrantOnMembers(_ context.Context, kind postgres.MemberKind, ids, privs []string, grantee string) error {
	for _, id := range ids {
		if err := f.record(fmt.Sprintf("grant %s on %s %s to %s", strings.Join(privs, ","), strings.ToLower(string(kind)), id, grantee)); err != nil {
			return err
		}
		f.hold(memberObj(kind, id), grantee, false, privs...)
	}
	return nil
}

func (f *fakeExtGrantClient) RevokeOnMembers(_ context.Context, kind postgres.MemberKind, ids, privs []string, grantee string) error {
	for _, id := range ids {
		if err := f.record(fmt.Sprintf("revoke %s on %s %s from %s", strings.Join(privs, ","), strings.ToLower(string(kind)), id, grantee)); err != nil {
			return err
		}
		if e := f.acl[aclKey(memberObj(kind, id), grantee)]; e != nil {
			e.privileges = subtract(e.privileges, privs)
		}
	}
	return nil
}

var _ = Describe("Extension grants", func() {
	ctx := context.Background()
	installed := func(schema string) installedExtensions {
		return installedExtensions{extPartman: {Name: extPartman, Version: extV52, Schema: schema}}
	}
	newDB := func(grants ...postgresv1alpha1.ExtensionGrantSpec) *postgresv1alpha1.Database {
		return &postgresv1alpha1.Database{
			Spec: postgresv1alpha1.DatabaseSpec{Extensions: []postgresv1alpha1.ExtensionSpec{{Name: extPartman, Grants: grants}}},
			Status: postgresv1alpha1.DatabaseStatus{Extensions: []postgresv1alpha1.ExtensionStatus{
				{Name: extPartman, Version: extV52, Schema: extPartSch}}},
		}
	}
	reconcile := func(f *fakeExtGrantClient, db *postgresv1alpha1.Database, eligible installedExtensions, version int) error {
		return reconcileExtensionGrants(ctx, f, db, extensionStates{eligible: eligible}, version, testChecker(f.fakeGrantClient, nil), nil)
	}

	It("grants EXECUTE only on SQL/PL/pgSQL invoker functions and table privileges only on tables", func() {
		f := newFakeExtGrantClient()
		db := newDB(postgresv1alpha1.ExtensionGrantSpec{Role: grantTestRole, Schema: []string{postgres.PrivilegeUsage, postgres.PrivilegeCreate},
			Tables: []string{postgres.PrivilegeAll}, Sequences: []string{postgres.PrivilegeUsage}, Functions: []string{postgres.PrivilegeExecute}})
		Expect(reconcile(f, db, installed(extPartSch), pgVersion17)).To(Succeed())
		Expect(f.calls).To(ContainElements(
			"grant CREATE,USAGE on schema partman to app wgo=false",
			callGrantRun,
			"grant SELECT on table partman.part_config to app",
			"grant USAGE on sequence partman.part_config_id_seq to app",
		))
		joined := strings.Join(f.calls, "\n")
		for _, skipped := range []string{fnDefiner, fnC, viewConfig, extTestTrigger, postgres.PrivilegeMaintain} {
			Expect(joined).NotTo(ContainSubstring(skipped))
		}
		Expect(db.Status.ManagedExtensionGrants).To(ConsistOf(
			postgresv1alpha1.ManagedExtensionGrant{Extension: extPartman, Role: grantTestRole, Kind: postgresv1alpha1.ExtensionObjectSchema,
				Schema: extPartSch, Privileges: []string{postgres.PrivilegeCreate, postgres.PrivilegeUsage}},
			postgresv1alpha1.ManagedExtensionGrant{Extension: extPartman, Role: grantTestRole, Kind: postgresv1alpha1.ExtensionObjectTables,
				Privileges: []string{postgres.PrivilegeDelete, "INSERT", "REFERENCES", postgres.PrivilegeSelect, "TRUNCATE", "UPDATE"}},
			postgresv1alpha1.ManagedExtensionGrant{Extension: extPartman, Role: grantTestRole, Kind: postgresv1alpha1.ExtensionObjectSequences,
				Privileges: []string{postgres.PrivilegeUsage}},
			postgresv1alpha1.ManagedExtensionGrant{Extension: extPartman, Role: grantTestRole, Kind: postgresv1alpha1.ExtensionObjectFunctions,
				Privileges: []string{postgres.PrivilegeExecute}},
		))
		Expect(db.Status.Extensions[0].SkippedObjects).To(BeEquivalentTo(3), "two functions and a view")

		By("granting nothing again on the next reconcile")
		f.calls = nil
		Expect(reconcile(f, db, installed(extPartSch), pgVersion17)).To(Succeed())
		Expect(f.calls).To(BeEmpty())
	})

	It("picks up objects an update adds and revokes EXECUTE from a function that stopped qualifying", func() {
		f := newFakeExtGrantClient()
		db := newDB(postgresv1alpha1.ExtensionGrantSpec{Role: grantTestRole, Functions: []string{postgres.PrivilegeAll}})
		Expect(reconcile(f, db, installed(extPartSch), pgVersion17)).To(Succeed())

		f.calls = nil
		f.members[extPartman][postgres.MemberRoutines] = []fakeMember{
			{fnMaint, false}, // rewritten as SECURITY DEFINER by an update
			{"partman.new_fn()", true},
		}
		Expect(reconcile(f, db, installed(extPartSch), pgVersion17)).To(Succeed())
		Expect(f.calls).To(ConsistOf(
			"grant EXECUTE on routine partman.new_fn() to app",
			"revoke EXECUTE on routine partman.run_maintenance() from app",
		))
	})

	It("revokes what it granted once the grant is removed, on every object of the kind", func() {
		f := newFakeExtGrantClient()
		db := newDB(postgresv1alpha1.ExtensionGrantSpec{Role: grantTestRole, Schema: []string{postgres.PrivilegeUsage}, Functions: []string{postgres.PrivilegeExecute}})
		Expect(reconcile(f, db, installed(extPartSch), pgVersion17)).To(Succeed())
		f.hold(memberObj(postgres.MemberRoutines, fnC), grantTestRole, false, postgres.PrivilegeExecute) // by hand

		f.calls = nil
		db.Spec.Extensions[0].Grants = nil
		Expect(reconcile(f, db, installed(extPartSch), pgVersion17)).To(Succeed())
		Expect(f.calls).To(ConsistOf(
			"revoke USAGE on schema partman from app optionOnly=false cascade=false",
			"revoke EXECUTE on routine partman.run_maintenance() from app",
			"revoke EXECUTE on routine partman.c_helper(integer) from app",
		))
		Expect(db.Status.ManagedExtensionGrants).To(BeEmpty())
	})

	It("forgets grants on an extension that was dropped and keeps a schema another extension still grants on", func() {
		f := newFakeExtGrantClient()
		db := newDB(postgresv1alpha1.ExtensionGrantSpec{Role: grantTestRole, Schema: []string{postgres.PrivilegeUsage}, Functions: []string{postgres.PrivilegeExecute}})
		db.Spec.Extensions = append(db.Spec.Extensions, postgresv1alpha1.ExtensionSpec{Name: "other_ext",
			Grants: []postgresv1alpha1.ExtensionGrantSpec{{Role: grantTestRole, Schema: []string{postgres.PrivilegeUsage}}}})
		f.members["other_ext"] = map[postgres.MemberKind][]fakeMember{}
		eligible := installed(extPartSch)
		eligible["other_ext"] = &postgres.InstalledExtension{Name: "other_ext", Schema: extPartSch}
		Expect(reconcile(f, db, eligible, pgVersion17)).To(Succeed())
		Expect(db.Status.ManagedExtensionGrants).To(HaveLen(3))

		By("dropping pg_partman and removing it from the spec")
		delete(f.members, extPartman)
		db.Spec.Extensions = db.Spec.Extensions[1:]
		delete(eligible, extPartman)
		f.calls = nil
		Expect(reconcile(f, db, eligible, pgVersion17)).To(Succeed())
		Expect(f.calls).To(BeEmpty(), "objects are gone, and other_ext still grants USAGE on the schema")
		Expect(db.Status.ManagedExtensionGrants).To(ConsistOf(HaveField("Extension", "other_ext")))
	})

	It("refuses schema grants on public, system schemas and schemas the spec lists", func() {
		for _, schema := range []string{publicSchemaName, extTestPgCatalog, grantTestRole} {
			f := newFakeExtGrantClient()
			db := newDB(postgresv1alpha1.ExtensionGrantSpec{Role: grantTestRole, Schema: []string{postgres.PrivilegeUsage}, Functions: []string{postgres.PrivilegeExecute}})
			db.Spec.Schemas = []postgresv1alpha1.SchemaSpec{{Name: grantTestRole}}
			err := reconcile(f, db, installed(schema), pgVersion17)
			Expect(extReason(err)).To(Equal(ReasonExtensionGrantNotAllowed), schema)
			Expect(f.calls).To(Equal([]string{callGrantRun}), schema)
		}
	})

	It("applies the grantee policy", func() {
		f := newFakeExtGrantClient()
		f.superusers = map[string]bool{grantTestAdmin: true}
		db := newDB(
			postgresv1alpha1.ExtensionGrantSpec{Role: grantTestAdmin, Functions: []string{postgres.PrivilegeExecute}},
			postgresv1alpha1.ExtensionGrantSpec{Role: "unknown_role", Functions: []string{postgres.PrivilegeExecute}},
			postgresv1alpha1.ExtensionGrantSpec{Role: postgres.PublicGrantee, Functions: []string{postgres.PrivilegeExecute}},
		)
		err := reconcile(f, db, installed(extPartSch), pgVersion17)
		Expect(extReason(err)).To(Equal(ReasonGranteeNotAllowed))
		Expect(err.Error()).To(ContainSubstring("superuser"))
		Expect(f.calls).To(Equal([]string{"grant EXECUTE on routine partman.run_maintenance() to PUBLIC"}))
	})

	It("needs PostgreSQL 17 for MAINTAIN", func() {
		f := newFakeExtGrantClient()
		db := newDB(postgresv1alpha1.ExtensionGrantSpec{Role: grantTestRole, Tables: []string{postgres.PrivilegeSelect, postgres.PrivilegeMaintain}})
		err := reconcile(f, db, installed(extPartSch), pgVersion16)
		Expect(extReason(err)).To(Equal(ReasonUnsupportedServerVersion))
		Expect(f.calls).To(Equal([]string{"grant SELECT on table partman.part_config to app"}))

		f = newFakeExtGrantClient()
		Expect(reconcile(f, db, installed(extPartSch), pgVersion17)).To(Succeed())
		Expect(f.calls).To(ConsistOf("grant MAINTAIN on table partman.part_config to app",
			"grant SELECT on table partman.part_config to app"))
	})

	It("waits for an extension that is not installed or not allowed, and revokes what it granted on it", func() {
		f := newFakeExtGrantClient()
		db := newDB(postgresv1alpha1.ExtensionGrantSpec{Role: grantTestRole, Functions: []string{postgres.PrivilegeExecute}})
		Expect(reconcile(f, db, installed(extPartSch), pgVersion17)).To(Succeed())
		f.calls = nil
		Expect(reconcile(f, db, installedExtensions{}, pgVersion17)).To(Succeed())
		Expect(f.calls).To(Equal([]string{"revoke EXECUTE on routine partman.run_maintenance() from app"}))
	})

	It("leaves the tracked grants of an extension whose state could not be read alone", func() {
		f := newFakeExtGrantClient()
		db := newDB(postgresv1alpha1.ExtensionGrantSpec{Role: grantTestRole, Functions: []string{postgres.PrivilegeExecute}})
		Expect(reconcile(f, db, installed(extPartSch), pgVersion17)).To(Succeed())
		ledger := slices.Clone(db.Status.ManagedExtensionGrants)
		f.calls = nil
		for _, states := range []extensionStates{
			{eligible: installedExtensions{}, unknown: map[string]bool{extPartman: true}},
			{allUnknown: true},
		} {
			Expect(reconcileExtensionGrants(ctx, f, db, states, pgVersion17, testChecker(f.fakeGrantClient, nil), nil)).To(Succeed())
			Expect(f.calls).To(BeEmpty())
			Expect(db.Status.ManagedExtensionGrants).To(Equal(ledger))
		}
	})

	It("rejects invalid privileges before issuing SQL", func() {
		for _, g := range []postgresv1alpha1.ExtensionGrantSpec{
			{Role: grantTestRole, Functions: []string{"EXECUTE; DROP FUNCTION x"}},
			{Role: grantTestRole, Tables: []string{extTestTrigger}},
			{Role: grantTestRole, Sequences: []string{postgres.PrivilegeDelete}},
			{Role: grantTestRole, Schema: []string{postgres.PrivilegeExecute}},
		} {
			f := newFakeExtGrantClient()
			Expect(reconcile(f, newDB(g), installed(extPartSch), pgVersion17)).To(MatchError(ContainSubstring("invalid")))
			Expect(f.calls).To(BeEmpty())
		}
		f := newFakeExtGrantClient()
		db := newDB(postgresv1alpha1.ExtensionGrantSpec{Role: postgres.PublicGrantee, Functions: []string{postgres.PrivilegeExecute}},
			postgresv1alpha1.ExtensionGrantSpec{Role: grantTestLowerPublic, Functions: []string{postgres.PrivilegeExecute}})
		Expect(reconcile(f, db, installed(extPartSch), pgVersion17)).To(MatchError(ContainSubstring("listed more than once")))
	})

	It("only grants read-only privileges on an extension the server does not trust, and revokes the others", func() {
		f := newFakeExtGrantClient()
		g := postgresv1alpha1.ExtensionGrantSpec{Role: grantTestRole, Schema: []string{postgres.PrivilegeAll},
			Tables: []string{postgres.PrivilegeAll}, Sequences: []string{postgres.PrivilegeAll}, Functions: []string{postgres.PrivilegeExecute}}
		db := newDB(g)
		By("granting everything while it is trusted")
		Expect(reconcile(f, db, installed(extPartSch), pgVersion17)).To(Succeed())
		Expect(f.calls).To(ContainElement("grant CREATE,USAGE on schema partman to app wgo=false"))

		By("treating it as allowed only by the Cluster")
		f.calls = nil
		states := extensionStates{eligible: installed(extPartSch), untrusted: map[string]bool{extPartman: true}}
		err := reconcileExtensionGrants(ctx, f, db, states, pgVersion17, testChecker(f.fakeGrantClient, nil), nil)
		Expect(extReason(err)).To(Equal(ReasonExtensionGrantNotAllowed))
		Expect(err.Error()).To(ContainSubstring("does not trust"))
		Expect(f.calls).To(ConsistOf(
			"revoke CREATE on schema partman from app optionOnly=false cascade=false",
			"revoke DELETE on table partman.part_config from app",
			"revoke INSERT on table partman.part_config from app",
			"revoke REFERENCES on table partman.part_config from app",
			"revoke TRUNCATE on table partman.part_config from app",
			"revoke UPDATE on table partman.part_config from app",
			"revoke UPDATE on sequence partman.part_config_id_seq from app",
			"revoke USAGE on sequence partman.part_config_id_seq from app",
		))
		Expect(f.acl[aclKey(memberObj(postgres.MemberTables, tblConfig), grantTestRole)].privileges).To(Equal([]string{postgres.PrivilegeSelect}))
		Expect(f.acl[aclKey(memberObj(postgres.MemberRoutines, fnMaint), grantTestRole)].privileges).To(Equal([]string{postgres.PrivilegeExecute}))
	})
})
