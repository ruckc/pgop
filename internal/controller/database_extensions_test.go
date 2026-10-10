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

	"github.com/lib/pq"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Names used by the extension tests.
const (
	extHstore   = "hstore"
	extEarth    = "earthdistance"
	extCube     = "cube"
	extDblink   = "dblink"
	extChild    = "child"
	extParentU  = "parent_u"
	extCtl      = "ctl_ext"
	extDBOwner  = "app_owner"
	extSchema   = "ext"
	extEvil     = "evil"
	extV14      = "1.4"
	extV18      = "1.8"
	extOperator = "operator"
	extV16      = "1.6"
	extV52      = "5.2"
	extCitext   = "citext"
	extApp      = "ext_app"

	extTestTrigger    = "TRIGGER"
	extTestPgCatalog  = "pg_catalog"
	extTestInfoSchema = "information_schema"
)

// fakeExtClient models the extensions of one database for
// reconcileExtensions.
type fakeExtClient struct {
	// available maps "name@version" to what the server offers; defaults maps
	// names to their default version.
	available map[string]postgres.ExtensionVersionInfo
	defaults  map[string]string
	installed map[string]*postgres.InstalledExtension
	// paths lists "name|from|to" update paths.
	paths      map[string]bool
	schemas    map[string]postgres.SchemaWriters
	dbOwner    string
	superusers map[string]bool
	// dependents lists extensions DROP EXTENSION refuses (2BP01).
	dependents map[string]bool
	// raceOn makes CREATE EXTENSION of that name find it already created.
	raceOn string
	calls  []string
}

func newFakeExtClient() *fakeExtClient {
	f := &fakeExtClient{
		available: map[string]postgres.ExtensionVersionInfo{}, defaults: map[string]string{},
		installed: map[string]*postgres.InstalledExtension{}, paths: map[string]bool{},
		schemas: map[string]postgres.SchemaWriters{
			publicSchemaName: {Exists: true, Owner: pgDatabaseOwnerRole},
		},
		dbOwner:    bootstrapRoleName,
		superusers: map[string]bool{bootstrapRoleName: true, extOperator: true},
		dependents: map[string]bool{},
	}
	return f
}

// offer makes version of name available; the last offered version is the
// default.
func (f *fakeExtClient) offer(name, version string, trusted bool, requires ...string) *fakeExtClient {
	f.available[name+"@"+version] = postgres.ExtensionVersionInfo{Name: name, Version: version, Trusted: trusted,
		Relocatable: true, Requires: requires}
	f.defaults[name] = version
	return f
}

func (f *fakeExtClient) install(name, version, schema string) {
	f.installed[name] = &postgres.InstalledExtension{Name: name, Version: version, Schema: schema}
}

func (f *fakeExtClient) ExtensionVersion(_ context.Context, name, version string) (postgres.ExtensionVersionInfo, bool, error) {
	if version == "" {
		version = f.defaults[name]
	}
	info, ok := f.available[name+"@"+version]
	return info, ok, nil
}

func (f *fakeExtClient) InstalledExtension(_ context.Context, name string) (*postgres.InstalledExtension, error) {
	if e := f.installed[name]; e != nil {
		c := *e
		return &c, nil
	}
	return nil, nil
}

func (f *fakeExtClient) ExtensionUpdatePathExists(_ context.Context, name, from, to string) (bool, error) {
	return f.paths[name+"|"+from+"|"+to], nil
}

func (f *fakeExtClient) SchemaWriters(_ context.Context, name string) (postgres.SchemaWriters, error) {
	return f.schemas[name], nil
}

func (f *fakeExtClient) CurrentDatabaseOwner(context.Context) (string, error) { return f.dbOwner, nil }

func (f *fakeExtClient) LookupRole(_ context.Context, name string) (*postgres.ReachableRole, error) {
	return &postgres.ReachableRole{Name: name, Superuser: f.superusers[name]}, nil
}

func (f *fakeExtClient) CreateSchema(_ context.Context, name, owner string) error {
	f.calls = append(f.calls, fmt.Sprintf("create schema %s owner=%q", name, owner))
	f.schemas[name] = postgres.SchemaWriters{Exists: true, Owner: extOperator, OwnerSuperuser: true}
	return nil
}

func (f *fakeExtClient) CreateExtension(ctx context.Context, name, schema, version string, cascade bool) error {
	f.calls = append(f.calls, fmt.Sprintf("create extension %s schema=%q version=%q cascade=%t", name, schema, version, cascade))
	if f.raceOn == name {
		f.install(name, f.defaults[name], publicSchemaName)
		return fmt.Errorf("extension %q: %w", name, postgres.ErrObjectExists)
	}
	info, _, _ := f.ExtensionVersion(ctx, name, version)
	target := schema
	if target == "" {
		target = publicSchemaName
	}
	if info.Schema != "" {
		target = info.Schema
	}
	var installDeps func(requires []string) error
	installDeps = func(requires []string) error {
		for _, dep := range requires {
			if f.installed[dep] != nil {
				continue
			}
			if !cascade {
				return fmt.Errorf("required extension %q is not installed", dep)
			}
			f.install(dep, f.defaults[dep], target)
			if err := installDeps(f.available[dep+"@"+f.defaults[dep]].Requires); err != nil {
				return err
			}
		}
		return nil
	}
	if err := installDeps(info.Requires); err != nil {
		return err
	}
	f.install(name, info.Version, target)
	return nil
}

func (f *fakeExtClient) UpdateExtension(_ context.Context, name, version string) error {
	f.calls = append(f.calls, fmt.Sprintf("update extension %s to %s", name, version))
	f.installed[name].Version = version
	return nil
}

func (f *fakeExtClient) DropExtension(_ context.Context, name string) error {
	f.calls = append(f.calls, "drop extension "+name)
	if f.dependents[name] {
		return &pq.Error{Code: "2BP01", Message: "cannot drop extension because other objects depend on it"}
	}
	delete(f.installed, name)
	return nil
}

// extReason returns the condition reason err carries, "" when none.
func extReason(err error) string {
	if ce, ok := errors.AsType[*conditionError](err); ok {
		return ce.reason
	}
	return ""
}

var _ = Describe("Extensions", func() {
	ctx := context.Background()
	allow := func(names ...string) *postgresv1alpha1.RolePolicySpec {
		return &postgresv1alpha1.RolePolicySpec{AllowedExtensions: names}
	}
	newDB := func(exts ...postgresv1alpha1.ExtensionSpec) *postgresv1alpha1.Database {
		return &postgresv1alpha1.Database{Spec: postgresv1alpha1.DatabaseSpec{Extensions: exts}}
	}
	statusOf := func(db *postgresv1alpha1.Database, name string) postgresv1alpha1.ExtensionStatus {
		i := slices.IndexFunc(db.Status.Extensions, func(s postgresv1alpha1.ExtensionStatus) bool { return s.Name == name })
		ExpectWithOffset(1, i).To(BeNumerically(">=", 0), "no status for %s", name)
		return db.Status.Extensions[i]
	}

	Describe("policy", func() {
		It("installs a trusted extension into public and records it as created", func() {
			f := newFakeExtClient().offer(polTrgm, extV16, true)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: polTrgm})
			eligible, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{`create extension pg_trgm schema="" version="" cascade=false`}))
			Expect(eligible.eligible).To(HaveKey(polTrgm))
			Expect(db.Status.InstalledExtensions).To(Equal([]string{polTrgm}))
			Expect(statusOf(db, polTrgm)).To(Equal(postgresv1alpha1.ExtensionStatus{Name: polTrgm, Version: extV16,
				Schema: publicSchemaName, Created: true}))
		})

		It("refuses an untrusted extension until the Cluster allows it", func() {
			f := newFakeExtClient().offer(extDblink, "1.2", false)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extDblink})
			eligible, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionNotAllowed))
			Expect(f.calls).To(BeEmpty())
			Expect(eligible.eligible).To(BeEmpty())
			Expect(statusOf(db, extDblink).Reason).To(Equal(ReasonExtensionNotAllowed))

			eligible, err = reconcileExtensions(ctx, f, db, allow(extDblink), nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(eligible.eligible).To(HaveKey(extDblink))
			Expect(statusOf(db, extDblink).Reason).To(BeEmpty())
		})

		It("checks trust for the requested version", func() {
			f := newFakeExtClient().offer(extHstore, extV14, false).offer(extHstore, extV18, true)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extHstore, Version: extV14})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionNotAllowed))
			Expect(err.Error()).To(ContainSubstring("version 1.4"))
		})

		It("reports an extension the server does not have", func() {
			f := newFakeExtClient()
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: "postgis"})
			_, err := reconcileExtensions(ctx, f, db, allow("postgis"), nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionVersionNotAvailable))
			Expect(f.calls).To(BeEmpty())
		})

		It("reports an installed extension the policy no longer allows and withholds its grants", func() {
			f := newFakeExtClient().offer(extDblink, "1.2", false)
			f.install(extDblink, "1.2", publicSchemaName)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extDblink})
			eligible, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionNotAllowed))
			Expect(eligible.eligible).To(BeEmpty())
			Expect(db.Status.InstalledExtensions).To(Equal([]string{extDblink}))
			Expect(statusOf(db, extDblink).Created).To(BeFalse())
		})
	})

	Describe("cascade", func() {
		It("requires cascade for missing dependencies", func() {
			f := newFakeExtClient().offer(extCube, "1.5", true).offer(extEarth, "1.2", false, extCube)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extEarth})
			_, err := reconcileExtensions(ctx, f, db, allow(extEarth), nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionDependencyMissing))
			Expect(err.Error()).To(ContainSubstring(extCube))
			Expect(f.calls).To(BeEmpty())

			db.Spec.Extensions[0].Cascade = true
			_, err = reconcileExtensions(ctx, f, db, allow(extEarth), nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{`create extension earthdistance schema="" version="" cascade=true`}))
			Expect(f.installed).To(HaveKey(extCube))
		})

		It("installs nothing when a dependency is not allowed, however deep", func() {
			f := newFakeExtClient().
				offer(extParentU, "1.0", false).
				offer("middle", "1.0", true, extParentU).
				offer(extChild, "1.0", true, "middle")
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extChild, Cascade: true})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionNotAllowed))
			Expect(err.Error()).To(ContainSubstring(extParentU))
			Expect(f.calls).To(BeEmpty())

			By("allowing the dependency, which also makes the install strict about schemas")
			_, err = reconcileExtensions(ctx, f, db, allow(extParentU), nil, nil)
			Expect(err).NotTo(HaveOccurred(), "public is owned by pg_database_owner, here a superuser")
			Expect(f.installed).To(HaveKey(extParentU))
		})

		It("reports a dependency the server does not have", func() {
			f := newFakeExtClient().offer(extChild, "1.0", true, "missing_dep")
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extChild, Cascade: true})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionVersionNotAvailable))
			Expect(f.calls).To(BeEmpty())
		})

		It("checks the schemas of installed dependencies", func() {
			f := newFakeExtClient().offer(extCube, "1.5", true).offer(extEarth, "1.2", true, extCube)
			f.install(extCube, "1.5", extEvil)
			f.schemas[extEvil] = postgres.SchemaWriters{Exists: true, Owner: extEvil}
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extEarth})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionSchemaNotAllowed))
			Expect(err.Error()).To(ContainSubstring("schema evil, which is owned by evil"))
			Expect(f.calls).To(BeEmpty())
		})
	})

	Describe("target schema", func() {
		It("creates a missing schema the spec does not list, owned by the operator", func() {
			f := newFakeExtClient().offer(extDblink, "1.2", false)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extDblink, Schema: extSchema})
			_, err := reconcileExtensions(ctx, f, db, allow(extDblink), nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{`create schema ext owner=""`,
				`create extension dblink schema="ext" version="" cascade=false`}))
			Expect(statusOf(db, extDblink).Schema).To(Equal(extSchema))
		})

		It("refuses a schema listed in spec.schemas that does not exist", func() {
			f := newFakeExtClient().offer(polTrgm, extV16, true)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: polTrgm, Schema: extSchema})
			db.Spec.Schemas = []postgresv1alpha1.SchemaSpec{{Name: extSchema}}
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionSchemaNotAllowed))
			Expect(err.Error()).To(ContainSubstring("does not exist"))
			Expect(f.calls).To(BeEmpty())
		})

		It("refuses a system schema even when validation was bypassed", func() {
			f := newFakeExtClient().offer(polTrgm, extV16, true)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: polTrgm, Schema: extTestPgCatalog})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionSchemaNotAllowed))
			Expect(f.calls).To(BeEmpty())
		})

		It("follows the control file's schema and refuses another one", func() {
			f := newFakeExtClient().offer(extCtl, "1.0", true)
			info := f.available[extCtl+"@1.0"]
			info.Schema = "ctl"
			f.available[extCtl+"@1.0"] = info
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extCtl, Schema: extSchema})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionSchemaNotAllowed))
			Expect(err.Error()).To(ContainSubstring("control file"))

			By("installing into the control file's schema, which CREATE EXTENSION creates")
			db.Spec.Extensions[0].Schema = ""
			_, err = reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{`create extension ctl_ext schema="" version="" cascade=false`}))
		})

		DescribeTable("refuses a schema others can write to",
			func(w postgres.SchemaWriters, trusted, managed bool, dbOwnerSuper bool, problem string) {
				dbOwner := extDBOwner
				got := extensionSchemaProblem(extSchema, w, dbOwner, dbOwnerSuper, managed, !trusted)
				if problem == "" {
					Expect(got).To(BeEmpty())
				} else {
					Expect(got).To(ContainSubstring(problem))
				}
			},
			Entry("superuser-owned, nobody else creates", postgres.SchemaWriters{Exists: true, Owner: extOperator, OwnerSuperuser: true},
				false, false, false, ""),
			Entry("PUBLIC holds CREATE", postgres.SchemaWriters{Exists: true, Owner: extOperator, OwnerSuperuser: true,
				Creators: []string{postgres.PublicGrantee}}, true, false, false, "PUBLIC can create"),
			Entry("another role holds CREATE", postgres.SchemaWriters{Exists: true, Owner: extDBOwner,
				Creators: []string{grantTestRole}}, true, true, false, "app can create"),
			Entry("the database owner holds CREATE (trusted)", postgres.SchemaWriters{Exists: true, Owner: extOperator,
				OwnerSuperuser: true, Creators: []string{extDBOwner}}, true, false, false, ""),
			Entry("owned by the database owner (trusted)", postgres.SchemaWriters{Exists: true, Owner: extDBOwner},
				true, false, false, ""),
			Entry("owned by pg_database_owner (trusted)", postgres.SchemaWriters{Exists: true, Owner: pgDatabaseOwnerRole},
				true, false, false, ""),
			Entry("owned by an unmanaged role (trusted)", postgres.SchemaWriters{Exists: true, Owner: extEvil},
				true, false, false, "neither a superuser nor the database owner"),
			Entry("owned by the declared owner of a managed schema (trusted)", postgres.SchemaWriters{Exists: true, Owner: extEvil},
				true, true, false, ""),
			Entry("owned by the database owner (untrusted)", postgres.SchemaWriters{Exists: true, Owner: extDBOwner},
				false, true, false, "not a superuser"),
			Entry("the database owner holds CREATE (untrusted)", postgres.SchemaWriters{Exists: true, Owner: extOperator,
				OwnerSuperuser: true, Creators: []string{extDBOwner}}, false, false, false, "app_owner can create"),
			Entry("pg_database_owner of an operator-owned database (untrusted)", postgres.SchemaWriters{Exists: true,
				Owner: pgDatabaseOwnerRole}, false, false, true, ""),
			Entry("pg_database_owner of an owned database (untrusted)", postgres.SchemaWriters{Exists: true,
				Owner: pgDatabaseOwnerRole}, false, false, false, "owned by app_owner"),
		)

		It("refuses public of an owned database for an untrusted extension", func() {
			f := newFakeExtClient().offer(extDblink, "1.2", false)
			f.dbOwner = extDBOwner
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extDblink})
			_, err := reconcileExtensions(ctx, f, db, allow(extDblink), nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionSchemaNotAllowed))
			Expect(f.calls).To(BeEmpty())
		})
	})

	Describe("versions", func() {
		newHstore := func(installed string) *fakeExtClient {
			f := newFakeExtClient().offer(extHstore, extV14, true).offer(extHstore, "1.10", true).offer(extHstore, extV18, true)
			f.install(extHstore, installed, publicSchemaName)
			f.paths[extHstore+"|1.4|1.8"] = true
			f.paths[extHstore+"|1.8|1.10"] = true
			return f
		}

		It("updates to a higher version", func() {
			f := newHstore(extV14)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extHstore, Version: extV18})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{"update extension hstore to 1.8"}))
			Expect(statusOf(db, extHstore).Version).To(Equal(extV18))
		})

		It("compares versions numerically", func() {
			f := newHstore(extV18)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extHstore, Version: "1.10"})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(Equal([]string{"update extension hstore to 1.10"}))
		})

		It("refuses a downgrade", func() {
			f := newHstore(extV18)
			f.paths[extHstore+"|1.8|1.4"] = true // even with a downgrade script
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extHstore, Version: extV14})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionDowngradeNotAllowed))
			Expect(f.calls).To(BeEmpty())
			Expect(statusOf(db, extHstore).Version).To(Equal(extV18))
		})

		It("refuses a version without an update path", func() {
			f := newHstore("1.10")
			f.offer(extHstore, "2.0", true)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extHstore, Version: "2.0"})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionVersionNotAvailable))
			Expect(f.calls).To(BeEmpty())
		})

		It("refuses an update whose schema became writable", func() {
			f := newHstore(extV14)
			f.schemas[publicSchemaName] = postgres.SchemaWriters{Exists: true, Owner: pgDatabaseOwnerRole,
				Creators: []string{postgres.PublicGrantee}}
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extHstore, Version: extV18})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionSchemaNotAllowed))
			Expect(f.calls).To(BeEmpty())
		})

		It("leaves an installed extension at its version without a requested version", func() {
			f := newHstore(extV14)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extHstore})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).To(BeEmpty())
		})

		It("does not move an installed extension", func() {
			f := newHstore(extV14)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: extHstore, Schema: extSchema, Version: extV18})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionSchemaMismatch))
			Expect(f.calls).To(BeEmpty())
		})

		DescribeTable("compareExtensionVersions",
			func(a, b string, want int, ok bool) {
				got, gotOK := compareExtensionVersions(a, b)
				Expect(gotOK).To(Equal(ok))
				if ok {
					Expect(got).To(Equal(want))
				}
			},
			Entry("lower", "1.4", "1.8", -1, true),
			Entry("numeric, not lexical", "1.10", "1.9", 1, true),
			Entry("padding", "1.2", "1.2.0", 0, true),
			Entry("major", "2.0", "10.0", -1, true),
			Entry("pre-release", "1.0beta1", "1.0", 0, false),
			Entry("empty part", "1..2", "1.2", 0, false),
			Entry("sign", "+1", "1", 0, false),
		)
	})

	Describe("creation record and removal", func() {
		It("records the creation before CREATE EXTENSION and creates nothing when that fails", func() {
			f := newFakeExtClient().offer(polTrgm, extV16, true)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: polTrgm})
			var saved []postgresv1alpha1.ExtensionStatus
			save := func(context.Context) error {
				saved = slices.Clone(db.Status.Extensions)
				return nil
			}
			_, err := reconcileExtensions(ctx, f, db, nil, nil, save)
			Expect(err).NotTo(HaveOccurred())
			Expect(saved).To(ContainElement(HaveField("Created", BeTrue())))

			f = newFakeExtClient().offer(polTrgm, extV16, true)
			db = newDB(postgresv1alpha1.ExtensionSpec{Name: polTrgm})
			_, err = reconcileExtensions(ctx, f, db, nil, nil, func(context.Context) error { return errors.New("conflict") })
			Expect(err).To(MatchError(ContainSubstring("nothing was created")))
			Expect(f.calls).To(BeEmpty())
			Expect(statusOf(db, polTrgm).Created).To(BeFalse())
		})

		It("does not record an extension someone else created meanwhile", func() {
			f := newFakeExtClient().offer(polTrgm, extV16, true)
			f.raceOn = polTrgm
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: polTrgm, DropOnRemoval: true})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionNotManaged))
			Expect(statusOf(db, polTrgm).Created).To(BeFalse())

			db.Spec.Extensions = nil
			_, err = reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.calls).NotTo(ContainElement("drop extension pg_trgm"))
		})

		It("leaves a removed extension installed by default", func() {
			f := newFakeExtClient().offer(polTrgm, extV16, true)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: polTrgm})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			db.Spec.Extensions = nil
			_, err = reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.installed).To(HaveKey(polTrgm))
			Expect(db.Status.Extensions).To(BeEmpty())
			Expect(db.Status.InstalledExtensions).To(BeEmpty())
		})

		It("drops a removed extension pgop created with dropOnRemoval, without CASCADE, and retries a blocked drop", func() {
			f := newFakeExtClient().offer(polTrgm, extV16, true)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: polTrgm, DropOnRemoval: true})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(statusOf(db, polTrgm).DropOnRemoval).To(BeTrue())

			f.dependents[polTrgm] = true
			db.Spec.Extensions = nil
			_, err = reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(extReason(err)).To(Equal(ReasonExtensionDropBlocked))
			Expect(statusOf(db, polTrgm).Reason).To(Equal(ReasonExtensionDropBlocked))

			delete(f.dependents, polTrgm)
			_, err = reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.installed).NotTo(HaveKey(polTrgm))
			Expect(db.Status.Extensions).To(BeEmpty())
			Expect(strings.Join(f.calls, "\n")).NotTo(ContainSubstring("CASCADE"))
		})

		It("never drops an extension it did not create", func() {
			f := newFakeExtClient().offer(polTrgm, extV16, true)
			f.install(polTrgm, extV16, publicSchemaName)
			db := newDB(postgresv1alpha1.ExtensionSpec{Name: polTrgm, DropOnRemoval: true})
			_, err := reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			db.Spec.Extensions = nil
			_, err = reconcileExtensions(ctx, f, db, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.installed).To(HaveKey(polTrgm))
		})
	})
})
