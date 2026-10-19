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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// ownerRoleClient is a granteeClient over a fixed set of roles.
type ownerRoleClient struct {
	roles map[string]*postgres.ReachableRole
}

func (c *ownerRoleClient) LookupRole(_ context.Context, name string) (*postgres.ReachableRole, error) {
	return c.roles[name], nil
}

var _ = Describe("Schema owner policy", func() {
	ctx := context.Background()
	const (
		managedOwner = "team_app"
		allowedOwner = "legacy_app"
		strayOwner   = "stray"
		suOwner      = "boss"
		marker       = "pgop-marker"
	)
	newChecker := func(deleting map[string]bool) *granteeChecker {
		return &granteeChecker{
			pg: &ownerRoleClient{roles: map[string]*postgres.ReachableRole{
				managedOwner: {Name: managedOwner, Comment: marker},
				allowedOwner: {Name: allowedOwner},
				strayOwner:   {Name: strayOwner},
				suOwner:      {Name: suOwner, Superuser: true},
			}},
			policy:   &postgresv1alpha1.RolePolicySpec{AllowedExistingRoles: []string{allowedOwner, suOwner}},
			managed:  managedRoles{managedOwner: marker},
			deleting: deleting,
		}
	}
	dbWith := func(schemas ...postgresv1alpha1.SchemaSpec) *postgresv1alpha1.Database {
		return &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Schemas: schemas}}
	}
	refusedReason := func(err error) string {
		ce, ok := errors.AsType[*conditionError](err)
		Expect(ok).To(BeTrue(), "expected a conditionError in %v", err)
		return ce.reason
	}

	It("refuses unmanaged, superuser, reserved and PUBLIC owners without touching the schema", func() {
		for _, owner := range []string{strayOwner, suOwner, "postgres", "pgop_operator", "pg_monitor", "PUBLIC", "none"} {
			s := &fakeSchemaClient{dbOwner: grantTestAdmin}
			managed, refused, err := reconcileSchemas(ctx, s, dbWith(postgresv1alpha1.SchemaSpec{Name: "app", Owner: owner}),
				"", newChecker(nil))
			Expect(err).NotTo(HaveOccurred(), owner)
			Expect(managed).To(BeEmpty(), owner)
			Expect(s.calls).To(BeEmpty(), owner)
			Expect(refusedReason(refused)).To(Equal(ReasonGranteeNotAllowed), owner)
			Expect(refused).To(MatchError(ContainSubstring("schemas[app].owner: " + owner)))
		}
	})

	It("does not re-own an existing managed schema to a refused owner, and keeps it recorded", func() {
		s := &fakeSchemaClient{dbOwner: grantTestAdmin, owners: map[string]string{"app": grantTestRole}}
		db := dbWith(postgresv1alpha1.SchemaSpec{Name: "app", Owner: strayOwner})
		db.Status.CreatedSchemas = []string{"app"}
		managed, refused, err := reconcileSchemas(ctx, s, db, "", newChecker(nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(managed).To(Equal([]string{"app"}))
		Expect(s.calls).To(BeEmpty())
		Expect(refusedReason(refused)).To(Equal(ReasonGranteeNotAllowed))
	})

	It("allows managed and allowedExistingRoles owners", func() {
		s := &fakeSchemaClient{dbOwner: grantTestAdmin, owners: map[string]string{"old": grantTestRole}}
		db := dbWith(
			postgresv1alpha1.SchemaSpec{Name: "fresh", Owner: managedOwner},
			postgresv1alpha1.SchemaSpec{Name: "fresh2", Owner: allowedOwner},
			postgresv1alpha1.SchemaSpec{Name: "old", Owner: managedOwner})
		db.Status.CreatedSchemas = []string{"old"}
		managed, refused, err := reconcileSchemas(ctx, s, db, "", newChecker(nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(refused).NotTo(HaveOccurred())
		Expect(managed).To(Equal([]string{"fresh", "fresh2", "old"}))
		Expect(s.calls).To(Equal([]string{
			"create fresh owner team_app", "create fresh2 owner legacy_app", "alter old owner team_app"}))
	})

	It("retries a missing owner and pauses one being deleted, as plain errors", func() {
		s := &fakeSchemaClient{dbOwner: grantTestAdmin}
		db := dbWith(
			postgresv1alpha1.SchemaSpec{Name: "a", Owner: "nobody"},
			postgresv1alpha1.SchemaSpec{Name: "b", Owner: managedOwner})
		_, refused, err := reconcileSchemas(ctx, s, db, "", newChecker(map[string]bool{managedOwner: true}))
		Expect(err).NotTo(HaveOccurred())
		Expect(refused).To(MatchError(SatisfyAll(
			ContainSubstring(`schemas[a].owner: PostgreSQL role "nobody" does not exist yet`),
			ContainSubstring("schemas[b].owner: team_app: the Role is being deleted"))))
		_, isCond := errors.AsType[*conditionError](refused)
		Expect(isCond).To(BeFalse())
		Expect(s.calls).To(BeEmpty())
	})

	It("does not let one refused schema block the others, and skips the check with a nil checker", func() {
		s := &fakeSchemaClient{dbOwner: grantTestAdmin}
		db := dbWith(
			postgresv1alpha1.SchemaSpec{Name: "bad", Owner: strayOwner},
			postgresv1alpha1.SchemaSpec{Name: "good", Owner: managedOwner})
		managed, refused, err := reconcileSchemas(ctx, s, db, "", newChecker(nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(refused).To(HaveOccurred())
		Expect(managed).To(Equal([]string{"good"}))
		Expect(s.calls).To(Equal([]string{"create good owner team_app"}))

		s = &fakeSchemaClient{dbOwner: grantTestAdmin}
		managed, refused, err = reconcileSchemas(ctx, s, db, "", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(refused).NotTo(HaveOccurred())
		Expect(managed).To(Equal([]string{"bad", "good"}))
	})

	It("reports a refused schema grant grantee as GranteeNotAllowed", func() {
		f := &grantFilter{checker: newChecker(nil)}
		out, err := f.filter(ctx, "schemas[app].grants", []privilegeGrant{{Target: grantTarget{Grantee: strayOwner}}})
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(BeEmpty())
		Expect(refusedReason(f.err())).To(Equal(ReasonGranteeNotAllowed))
	})
})
