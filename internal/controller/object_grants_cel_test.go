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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// celSchema is the schema the object grant CEL tests declare.
const celSchema = "sales"

// CRD validation of spec.schemas[].objectGrants and defaultPrivileges
// (issue #24, PR 5).
var _ = Describe("Object grant CRD validation", func() {
	const ns = "default"
	ctx := context.Background()
	var suffix string
	BeforeEach(func() { suffix = fmt.Sprintf("%d", time.Now().UnixNano()) })

	create := func(name string, schema postgresv1alpha1.SchemaSpec) error {
		db := &postgresv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Name: name + "-" + suffix, Namespace: ns},
			Spec: postgresv1alpha1.DatabaseSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: nonexistentCluster},
				Schemas: []postgresv1alpha1.SchemaSpec{schema}}}
		err := k8sClient.Create(ctx, db)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, db) })
		}
		return err
	}
	expectInvalid := func(err error, part string) {
		ExpectWithOffset(1, apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
		ExpectWithOffset(1, err.Error()).To(ContainSubstring(part))
	}
	og := func(kind postgresv1alpha1.ObjectGrantKind, objects []string, privileges ...string) postgresv1alpha1.ObjectGrantSpec {
		return postgresv1alpha1.ObjectGrantSpec{Role: grantTestRole, Kind: kind, Objects: objects, Privileges: privileges}
	}
	all := []string{selectAll}

	It("accepts object grants and default privileges of every kind", func() {
		Expect(create("ok", postgresv1alpha1.SchemaSpec{Name: celSchema,
			ObjectGrants: []postgresv1alpha1.ObjectGrantSpec{
				og(postgresv1alpha1.ObjectGrantTable, all, postgres.PrivilegeSelect, postgres.PrivilegeTrigger, postgres.PrivilegeMaintain),
				og(postgresv1alpha1.ObjectGrantTable, []string{"orders", `We"ird; DROP TABLE x`}, postgres.PrivilegeAll),
				og(postgresv1alpha1.ObjectGrantSequence, all, postgres.PrivilegeUsage, postgres.PrivilegeSelect, postgres.PrivilegeUpdate),
				og(postgresv1alpha1.ObjectGrantFunction, []string{"caller_access(text)", "f"}, postgres.PrivilegeExecute),
				og(postgresv1alpha1.ObjectGrantProcedure, all, postgres.PrivilegeAll),
				og(postgresv1alpha1.ObjectGrantType, all, postgres.PrivilegeUsage),
				{Role: postgres.PublicGrantee, Kind: postgresv1alpha1.ObjectGrantFunction, Objects: all,
					Privileges: []string{postgres.PrivilegeExecute}},
			},
			DefaultPrivileges: []postgresv1alpha1.DefaultPrivilegeSpec{
				{ForRole: "app_migrator", Role: grantTestRole, Kind: postgresv1alpha1.DefaultPrivilegeTable,
					Privileges: []string{postgres.PrivilegeSelect}, WithGrantOption: true},
				{ForRole: "app_migrator", Role: postgres.PublicGrantee, Kind: postgresv1alpha1.DefaultPrivilegeFunction,
					Privileges: []string{postgres.PrivilegeExecute}},
			}})).To(Succeed())
	})

	It("rejects privileges that do not apply to the kind", func() {
		for _, g := range []postgresv1alpha1.ObjectGrantSpec{
			og(postgresv1alpha1.ObjectGrantTable, all, postgres.PrivilegeExecute),
			og(postgresv1alpha1.ObjectGrantSequence, all, postgres.PrivilegeInsert),
			og(postgresv1alpha1.ObjectGrantFunction, all, postgres.PrivilegeUsage),
			og(postgresv1alpha1.ObjectGrantProcedure, all, postgres.PrivilegeSelect),
			og(postgresv1alpha1.ObjectGrantType, all, postgres.PrivilegeExecute),
		} {
			expectInvalid(create("kind", postgresv1alpha1.SchemaSpec{Name: celSchema, ObjectGrants: []postgresv1alpha1.ObjectGrantSpec{g}}),
				"privileges are")
		}
		expectInvalid(create("inj", postgresv1alpha1.SchemaSpec{Name: celSchema, ObjectGrants: []postgresv1alpha1.ObjectGrantSpec{
			og(postgresv1alpha1.ObjectGrantTable, all, "SELECT; DROP TABLE x")}}), "Unsupported value")
		expectInvalid(create("dkind", postgresv1alpha1.SchemaSpec{Name: celSchema, DefaultPrivileges: []postgresv1alpha1.DefaultPrivilegeSpec{
			{ForRole: "m", Role: grantTestRole, Kind: postgresv1alpha1.DefaultPrivilegeType, Privileges: []string{postgres.PrivilegeSelect}}}}),
			"type privileges are USAGE or ALL")
	})

	It("rejects an unknown kind and procedure default privileges", func() {
		expectInvalid(create("k", postgresv1alpha1.SchemaSpec{Name: celSchema, ObjectGrants: []postgresv1alpha1.ObjectGrantSpec{
			og("view", all, postgres.PrivilegeSelect)}}), "Unsupported value")
		expectInvalid(create("dk", postgresv1alpha1.SchemaSpec{Name: celSchema, DefaultPrivileges: []postgresv1alpha1.DefaultPrivilegeSpec{
			{ForRole: "m", Role: grantTestRole, Kind: "procedure", Privileges: []string{postgres.PrivilegeExecute}}}}), "Unsupported value")
	})

	It(`requires "*" to be the only object and objects to be bounded`, func() {
		expectInvalid(create("star", postgresv1alpha1.SchemaSpec{Name: celSchema, ObjectGrants: []postgresv1alpha1.ObjectGrantSpec{
			og(postgresv1alpha1.ObjectGrantTable, []string{selectAll, "t"}, postgres.PrivilegeSelect)}}), "must be the only entry")
		expectInvalid(create("empty", postgresv1alpha1.SchemaSpec{Name: celSchema, ObjectGrants: []postgresv1alpha1.ObjectGrantSpec{
			og(postgresv1alpha1.ObjectGrantTable, []string{}, postgres.PrivilegeSelect)}}), "objects")
		expectInvalid(create("long", postgresv1alpha1.SchemaSpec{Name: celSchema, ObjectGrants: []postgresv1alpha1.ObjectGrantSpec{
			og(postgresv1alpha1.ObjectGrantTable, []string{strings.Repeat("x", 256)}, postgres.PrivilegeSelect)}}), "objects")
		many := make([]postgresv1alpha1.ObjectGrantSpec, 33)
		for i := range many {
			many[i] = og(postgresv1alpha1.ObjectGrantTable, all, postgres.PrivilegeSelect)
		}
		expectInvalid(create("many", postgresv1alpha1.SchemaSpec{Name: celSchema, ObjectGrants: many}), "objectGrants")
	})

	It("applies the grantee rules and refuses the grant option to PUBLIC", func() {
		for _, role := range []string{bootstrapRoleName, reservedPrefixName, polMonitor, grantTestLowerPublic, "nOne"} {
			g := og(postgresv1alpha1.ObjectGrantTable, all, postgres.PrivilegeSelect)
			g.Role = role
			expectInvalid(create("g", postgresv1alpha1.SchemaSpec{Name: celSchema, ObjectGrants: []postgresv1alpha1.ObjectGrantSpec{g}}), "role")
			expectInvalid(create("dg", postgresv1alpha1.SchemaSpec{Name: celSchema, DefaultPrivileges: []postgresv1alpha1.DefaultPrivilegeSpec{
				{ForRole: "m", Role: role, Kind: postgresv1alpha1.DefaultPrivilegeTable, Privileges: []string{postgres.PrivilegeSelect}}}}), "role")
		}
		g := og(postgresv1alpha1.ObjectGrantTable, all, postgres.PrivilegeSelect)
		g.Role, g.WithGrantOption = postgres.PublicGrantee, true
		expectInvalid(create("pub", postgresv1alpha1.SchemaSpec{Name: celSchema, ObjectGrants: []postgresv1alpha1.ObjectGrantSpec{g}}),
			"grant option")
	})

	It("refuses reserved roles and PUBLIC as forRole", func() {
		for _, forRole := range []string{bootstrapRoleName, DefaultOperatorUsername, "pg_read_all_data", postgres.PublicGrantee,
			grantTestLowerPublic, "None"} {
			expectInvalid(create("fr", postgresv1alpha1.SchemaSpec{Name: celSchema, DefaultPrivileges: []postgresv1alpha1.DefaultPrivilegeSpec{
				{ForRole: forRole, Role: grantTestRole, Kind: postgresv1alpha1.DefaultPrivilegeTable,
					Privileges: []string{postgres.PrivilegeSelect}}}}), "forRole")
		}
		By("rejecting the same forRole, role and kind twice")
		d := postgresv1alpha1.DefaultPrivilegeSpec{ForRole: "m", Role: grantTestRole, Kind: postgresv1alpha1.DefaultPrivilegeTable,
			Privileges: []string{postgres.PrivilegeSelect}}
		expectInvalid(create("dup", postgresv1alpha1.SchemaSpec{Name: celSchema,
			DefaultPrivileges: []postgresv1alpha1.DefaultPrivilegeSpec{d, d}}), "Duplicate")
	})
})
