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
	"strconv"
	"strings"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Extensions.
//
// pgop runs CREATE EXTENSION and ALTER EXTENSION ... UPDATE as a superuser,
// so the extension's install and update scripts run as a superuser inside
// the Database's database. Installing an extension is therefore a privileged
// operation that a Database writer, who must not become superuser-equivalent,
// may only request within limits:
//
//   - policy: every extension that would be installed or updated (the listed
//     one, and with cascade every dependency that is not installed yet) must
//     be marked trusted by the server for every version whose scripts would
//     run (the versions on the update paths, see installVersions), or be
//     listed in the Cluster's rolePolicy.allowedExtensions.
//     The dependency list is resolved before anything runs; one refused
//     dependency refuses the whole install (ExtensionNotAllowed);
//   - schemas: the scripts run with search_path set to the target schema (and
//     the schemas of the extensions it requires). A role that can create
//     objects there can plant functions or operators the superuser-run
//     script then calls (the CVE-2022-2625 / CVE-2023-39417 class). So every
//     such schema must be owned by a superuser, the database owner or a
//     schema the Database manages, and no other role (nor PUBLIC) may hold
//     CREATE on it. For an extension that is installed only because the
//     Cluster allows it (not trusted), even that is not enough: its scripts
//     were not written to be safe for a non-superuser to install, so every
//     such schema must be owned by a superuser and writable by superusers
//     only. Otherwise nothing runs (ExtensionSchemaNotAllowed). Every
//     missing schema an install needs (the target schema the spec does not
//     list, and schemas control files name) is created by pgop, owned by the
//     operator, before CREATE EXTENSION, which would otherwise reuse a schema
//     created in the meantime without checking its owner;
//   - versions: an installed extension is only ever updated to a higher
//     version the server has an update path to; downgrades are refused;
//   - removal: an extension removed from the spec is left installed, unless
//     pgop created it (the installation whose oid and owner it recorded) and
//     dropOnRemoval was recorded; then it is dropped without CASCADE.

// extensionInstallClient is the subset of *postgres.Client used to install,
// update and drop extensions (on a connection to the database).
type extensionInstallClient interface {
	ExtensionVersion(ctx context.Context, name, version string) (postgres.ExtensionVersionInfo, bool, error)
	InstalledExtension(ctx context.Context, name string) (*postgres.InstalledExtension, error)
	ExtensionUpdatePaths(ctx context.Context, name, from, to string) ([][]string, error)
	SchemaWriters(ctx context.Context, name string) (postgres.SchemaWriters, error)
	CurrentDatabaseOwner(ctx context.Context) (string, error)
	LookupRole(ctx context.Context, name string) (*postgres.ReachableRole, error)
	CreateSchema(ctx context.Context, name, owner string) error
	CreateExtension(ctx context.Context, name, schema, version string, cascade bool) error
	UpdateExtension(ctx context.Context, name, version string) error
	DropExtension(ctx context.Context, name string) error
}

var _ extensionInstallClient = (*postgres.Client)(nil)

// extensionRun is one reconcile of a Database's extensions.
type extensionRun struct {
	pg       extensionInstallClient
	policy   *postgresv1alpha1.RolePolicySpec
	database *postgresv1alpha1.Database
	// managedSchemas are the schemas the Database manages (reconcileSchemas);
	// specSchemas the schemas listed in spec.schemas.
	managedSchemas map[string]bool
	specSchemas    map[string]bool
	dbOwner        string
	dbOwnerSuper   bool
	save           statusSaver
	// cur is the working copy of status.extensions, by name.
	cur map[string]postgresv1alpha1.ExtensionStatus
	// listed are the names in spec.extensions.
	listed map[string]bool
}

// installedExtensions maps the extensions pgop may grant on (installed and
// allowed by the policy) to where they are installed.
type installedExtensions map[string]*postgres.InstalledExtension

// extensionStates is what reconcileExtensions found out for the extension
// grants.
type extensionStates struct {
	// eligible are the extensions pgop may grant on.
	eligible installedExtensions
	// untrusted lists the eligible extensions that the server does not mark
	// trusted at their installed version (allowed only by the Cluster's
	// allowedExtensions): grants that would let a role write where their
	// superuser-run code reads are not applied on them.
	untrusted map[string]bool
	// unknown lists extensions whose state could not be read (an error that
	// is not a refusal): the grants pgop tracks on them are left alone, so a
	// transient error never revokes anything. allUnknown: nothing could be
	// read.
	unknown    map[string]bool
	allUnknown bool
}

// extensionStatusLimit is the maxItems of status.extensions: the spec's
// extensions (at most 64) and the removed ones pgop still has to drop.
const extensionStatusLimit = 128

// reconcileExtensions installs, updates and (with dropOnRemoval) drops the
// Database's extensions and records them in status.extensions and
// status.installedExtensions. managedSchemas are the schemas the Database
// manages. It returns what the extension grants need to know, and the
// problems found (joined; they do not stop the other extensions).
func reconcileExtensions(ctx context.Context, pg extensionInstallClient, database *postgresv1alpha1.Database,
	policy *postgresv1alpha1.RolePolicySpec, managedSchemas []string, save statusSaver) (extensionStates, error) {
	r := &extensionRun{
		pg: pg, policy: policy, database: database, managedSchemas: setOf(managedSchemas),
		specSchemas: map[string]bool{}, save: save, cur: map[string]postgresv1alpha1.ExtensionStatus{},
		listed: map[string]bool{},
	}
	for _, s := range database.Spec.Schemas {
		r.specSchemas[s.Name] = true
	}
	for _, st := range database.Status.Extensions {
		r.cur[st.Name] = st
	}
	states := extensionStates{eligible: installedExtensions{}, untrusted: map[string]bool{}, unknown: map[string]bool{}}
	for _, ext := range database.Spec.Extensions {
		r.listed[ext.Name] = true
	}
	if len(database.Spec.Extensions) == 0 && len(r.cur) == 0 {
		database.Status.InstalledExtensions = nil
		return states, nil
	}
	owner, err := pg.CurrentDatabaseOwner(ctx)
	if err != nil {
		return extensionStates{allUnknown: true}, err
	}
	r.dbOwner = owner
	if role, err := pg.LookupRole(ctx, owner); err != nil {
		return extensionStates{allUnknown: true}, err
	} else if role != nil {
		r.dbOwnerSuper = role.Superuser
	}

	var errs []error
	var statuses []postgresv1alpha1.ExtensionStatus
	var installedNames []string
	seen := map[string]bool{}
	for _, ext := range database.Spec.Extensions {
		if seen[ext.Name] {
			errs = append(errs, fmt.Errorf("extensions: %q is listed more than once", ext.Name))
			continue
		}
		seen[ext.Name] = true
		prev := r.cur[ext.Name]
		st := postgresv1alpha1.ExtensionStatus{Name: ext.Name, DropOnRemoval: ext.DropOnRemoval,
			Created: prev.Created, OID: prev.OID, Owner: prev.Owner}
		inst, err := r.reconcileOne(ctx, ext, &st)
		if inst != nil && st.Created && (st.OID == 0 || st.OID != inst.OID || st.Owner != inst.Owner) {
			// Only an installation whose oid and owner pgop recorded right
			// after creating it is pgop's: an intent record without them (a
			// status write lost after CREATE EXTENSION), or an extension
			// dropped and created again since, is not.
			st.Created, st.OID, st.Owner = false, 0, ""
		}
		if err != nil {
			errs = append(errs, err)
			if st.Reason == "" {
				st.Reason, st.Message = ReasonReconcileError, err.Error()
			}
			if _, refused := errors.AsType[*conditionError](err); !refused && inst == nil {
				states.unknown[ext.Name] = true
			}
		}
		if inst != nil {
			st.Version, st.Schema = inst.Version, inst.Schema
			installedNames = append(installedNames, ext.Name)
			allowed, trusted, err := r.allowedAsInstalled(ctx, inst)
			switch {
			case err != nil:
				errs = append(errs, err)
				states.unknown[ext.Name] = true
			case allowed:
				states.eligible[ext.Name] = inst
				states.untrusted[ext.Name] = !trusted
			}
		}
		r.cur[ext.Name] = st
		statuses = append(statuses, st)
	}
	statuses = append(statuses, r.dropRemoved(ctx, len(statuses), &errs)...)
	database.Status.Extensions = statuses
	database.Status.InstalledExtensions = installedNames
	return states, errors.Join(errs...)
}

// refuse records a refusal in st and returns it as a condition error.
func refuse(st *postgresv1alpha1.ExtensionStatus, reason, format string, args ...any) error {
	err := &conditionError{reason: reason, err: fmt.Errorf("extension %s: "+format, append([]any{st.Name}, args...)...)}
	st.Reason, st.Message = reason, err.Error()
	return err
}

// allowedAsInstalled reports whether the policy allows the extension at the
// version it is installed at (only then are its grants applied), and
// whether the server marks that version trusted.
func (r *extensionRun) allowedAsInstalled(ctx context.Context, inst *postgres.InstalledExtension) (allowed, trusted bool, err error) {
	info, found, err := r.pg.ExtensionVersion(ctx, inst.Name, inst.Version)
	if err != nil {
		return false, false, err
	}
	trusted = found && info.Trusted
	return trusted || r.policy.AllowsExtension(inst.Name), trusted, nil
}

// reconcileOne brings one extension to its spec and returns it as it is
// installed afterwards (nil when it is not installed).
func (r *extensionRun) reconcileOne(ctx context.Context, ext postgresv1alpha1.ExtensionSpec,
	st *postgresv1alpha1.ExtensionStatus) (*postgres.InstalledExtension, error) {
	inst, err := r.pg.InstalledExtension(ctx, ext.Name)
	if err != nil {
		return nil, err
	}
	if ext.Schema != "" && systemSchemaName(ext.Schema) {
		return inst, refuse(st, ReasonExtensionSchemaNotAllowed, "the system schema %s cannot be a target schema", ext.Schema)
	}
	switch {
	case inst == nil:
		return r.create(ctx, ext, st)
	case ext.Schema != "" && ext.Schema != inst.Schema:
		// Not moved; an update (below) is still not attempted, so the
		// mismatch is noticed first.
		return inst, refuse(st, ReasonExtensionSchemaMismatch,
			"installed in schema %s, not %s; pgop does not move installed extensions", inst.Schema, ext.Schema)
	case ext.Version != "" && ext.Version != inst.Version:
		return r.update(ctx, ext, inst, st)
	}
	allowed, _, err := r.allowedAsInstalled(ctx, inst)
	if err != nil {
		return inst, err
	}
	if !allowed {
		return inst, refuse(st, ReasonExtensionNotAllowed, "installed at version %s, which is neither trusted by the server "+
			"nor listed in the Cluster's spec.rolePolicy.allowedExtensions; its grants are not applied", inst.Version)
	}
	return inst, nil
}

// dependencies resolves the extensions info requires, recursively: those
// that are installed (their own dependencies are installed too), those that
// CASCADE would install (at their default version) and those the server
// does not have.
func (r *extensionRun) dependencies(ctx context.Context, info postgres.ExtensionVersionInfo) (
	installed []*postgres.InstalledExtension, toInstall []postgres.ExtensionVersionInfo, unavailable []string, err error) {
	seen := map[string]bool{info.Name: true}
	queue := slices.Clone(info.Requires)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		inst, err := r.pg.InstalledExtension(ctx, name)
		if err != nil {
			return nil, nil, nil, err
		}
		if inst != nil {
			installed = append(installed, inst)
			continue
		}
		dep, found, err := r.pg.ExtensionVersion(ctx, name, "")
		if err != nil {
			return nil, nil, nil, err
		}
		if !found {
			unavailable = append(unavailable, name)
			continue
		}
		toInstall = append(toInstall, dep)
		queue = append(queue, dep.Requires...)
	}
	return installed, toInstall, unavailable, nil
}

// installPlan is what CREATE EXTENSION would install.
type installPlan struct {
	info          postgres.ExtensionVersionInfo
	installedDeps []*postgres.InstalledExtension
	toInstall     []postgres.ExtensionVersionInfo
	// strict is set when a script of one of the extensions to install (of
	// any version it may run) is not trusted.
	strict bool
}

// installVersions returns the versions of the extension name whose scripts
// installing version may run: version itself and every version on an
// update path that ends at it (PostgreSQL may run an older version's install
// script and update from there, running every update script on the way).
// pgop cannot see which path PostgreSQL picks, so it takes all of them that
// start at an installable version (one pg_available_extension_versions
// lists; the others have no install script to start from).
func (r *extensionRun) installVersions(ctx context.Context, name, version string) ([]string, error) {
	paths, err := r.pg.ExtensionUpdatePaths(ctx, name, "", version)
	if err != nil {
		return nil, err
	}
	versions := []string{version}
	for _, p := range paths {
		_, installable, err := r.pg.ExtensionVersion(ctx, name, p[0])
		if err != nil {
			return nil, err
		}
		if installable {
			versions = append(versions, p...)
		}
	}
	return uniqueSorted(versions), nil
}

// untrustedVersions returns the versions of the extension name that the
// server does not mark trusted (or does not have).
func (r *extensionRun) untrustedVersions(ctx context.Context, name string, versions []string) ([]string, error) {
	var out []string
	for _, v := range versions {
		info, found, err := r.pg.ExtensionVersion(ctx, name, v)
		if err != nil {
			return nil, err
		}
		if !found || !info.Trusted {
			out = append(out, v)
		}
	}
	return out, nil
}

// checkVersions checks the policy for the scripts of versions of the
// extension name: every one must be trusted, or the Cluster must allow the
// extension. It returns a problem ("" when allowed) and whether a script is
// not trusted (strict schema rules then apply).
func (r *extensionRun) checkVersions(ctx context.Context, name string, versions []string) (problem string, untrusted bool, err error) {
	bad, err := r.untrustedVersions(ctx, name, versions)
	if err != nil || len(bad) == 0 {
		return "", false, err
	}
	if r.policy.AllowsExtension(name) {
		return "", true, nil
	}
	return fmt.Sprintf("%s version(s) %s, whose scripts it would run, are neither trusted by the server nor allowed by the "+
		"Cluster's spec.rolePolicy.allowedExtensions", name, strings.Join(bad, ", ")), true, nil
}

// planCreate resolves what installing ext would install and checks it
// against the policy: every version of every extension whose scripts would
// run. A nil plan comes with the refusal.
func (r *extensionRun) planCreate(ctx context.Context, ext postgresv1alpha1.ExtensionSpec,
	st *postgresv1alpha1.ExtensionStatus) (*installPlan, error) {
	info, found, err := r.pg.ExtensionVersion(ctx, ext.Name, ext.Version)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, refuse(st, ReasonExtensionVersionNotAvailable, "%s is not available on the server "+
			"(its files are not in the image)", versionLabel(ext.Version))
	}
	versions, err := r.installVersions(ctx, ext.Name, info.Version)
	if err != nil {
		return nil, err
	}
	problem, strict, err := r.checkVersions(ctx, ext.Name, versions)
	if err != nil {
		return nil, err
	}
	if problem != "" {
		return nil, refuse(st, ReasonExtensionNotAllowed, "not installed: %s", problem)
	}
	installedDeps, toInstall, unavailable, err := r.dependencies(ctx, info)
	if err != nil {
		return nil, err
	}
	if len(unavailable) > 0 {
		return nil, refuse(st, ReasonExtensionVersionNotAvailable, "requires %s, which the server does not have",
			strings.Join(unavailable, ", "))
	}
	if len(toInstall) > 0 && !ext.Cascade {
		return nil, refuse(st, ReasonExtensionDependencyMissing, "requires %s, which is not installed: list it "+
			"before this extension, or set cascade: true", strings.Join(extensionNames(toInstall), ", "))
	}
	plan := &installPlan{info: info, installedDeps: installedDeps, toInstall: toInstall, strict: strict}
	var refused []string
	for _, dep := range toInstall {
		versions, err := r.installVersions(ctx, dep.Name, dep.Version)
		if err != nil {
			return nil, err
		}
		problem, untrusted, err := r.checkVersions(ctx, dep.Name, versions)
		if err != nil {
			return nil, err
		}
		if problem != "" {
			refused = append(refused, problem)
		}
		plan.strict = plan.strict || untrusted
	}
	if len(refused) > 0 {
		return nil, refuse(st, ReasonExtensionNotAllowed, "cascade would install dependencies the policy does not allow, "+
			"nothing was installed: %s", strings.Join(refused, "; "))
	}
	return plan, nil
}

// checkInstallSchemas checks the schemas the install scripts of plan run in:
// each new extension's target schema, and the schemas of the installed
// extensions they require. It returns the schemas that do not exist yet and
// that pgop must create (owned by the operator) before CREATE EXTENSION, so
// CREATE EXTENSION never creates one itself: a schema it would create could
// be created by the database owner in the meantime, and CREATE EXTENSION
// would use it without checking who owns it.
func (r *extensionRun) checkInstallSchemas(ctx context.Context, ext postgresv1alpha1.ExtensionSpec,
	st *postgresv1alpha1.ExtensionStatus, plan *installPlan) (toCreate []string, err error) {
	target := ext.Schema
	if s := plan.info.Schema; s != "" {
		if ext.Schema != "" && ext.Schema != s {
			return nil, refuse(st, ReasonExtensionSchemaNotAllowed, "its control file installs it in schema %s, not %s",
				s, ext.Schema)
		}
		target = s
	}
	if target == "" {
		target = publicSchemaName
	}
	// controlSchemas are schemas named by control files: CREATE EXTENSION
	// would create them when missing.
	controlSchemas := map[string]bool{plan.info.Schema: plan.info.Schema != ""}
	schemas := []string{target}
	for _, dep := range plan.toInstall {
		s := dep.Schema
		if s != "" {
			controlSchemas[s] = true
		} else if s = ext.Schema; s == "" {
			s = publicSchemaName
		}
		schemas = append(schemas, s)
	}
	for _, dep := range plan.installedDeps {
		schemas = append(schemas, dep.Schema)
	}
	var problems []string
	for _, s := range uniqueSorted(schemas) {
		p, missing, err := r.schemaProblem(ctx, s, plan.strict)
		if err != nil {
			return nil, err
		}
		switch {
		case missing && (controlSchemas[s] || s == ext.Schema && !r.specSchemas[s]):
			toCreate = append(toCreate, s)
		case missing:
			problems = append(problems, fmt.Sprintf("schema %s does not exist", s))
		case p != "":
			problems = append(problems, p)
		}
	}
	if len(problems) > 0 {
		return nil, refuse(st, ReasonExtensionSchemaNotAllowed, "not installed: its install script would run as a "+
			"superuser in %s", strings.Join(problems, "; "))
	}
	return toCreate, nil
}

// createSchemas creates the missing schemas an install needs, owned by the
// operator (the session user), so no other role can write to them. A schema
// someone created since the check is checked again and refused unless it is
// acceptable.
func (r *extensionRun) createSchemas(ctx context.Context, ext postgresv1alpha1.ExtensionSpec,
	st *postgresv1alpha1.ExtensionStatus, schemas []string, strict bool) error {
	for _, s := range schemas {
		err := r.pg.CreateSchema(ctx, s, "")
		if err == nil {
			continue
		}
		if !errors.Is(err, postgres.ErrObjectExists) {
			return fmt.Errorf("extension %s: creating schema %s: %w", ext.Name, s, err)
		}
		p, missing, err := r.schemaProblem(ctx, s, strict)
		if err != nil {
			return err
		}
		if missing {
			p = fmt.Sprintf("schema %s, which was created and dropped while pgop was creating it", s)
		}
		if p != "" {
			return refuse(st, ReasonExtensionSchemaNotAllowed, "not installed: someone created its schema while pgop was "+
				"creating it, and its install script would run as a superuser in %s", p)
		}
	}
	return nil
}

// create installs an extension that is not installed.
func (r *extensionRun) create(ctx context.Context, ext postgresv1alpha1.ExtensionSpec,
	st *postgresv1alpha1.ExtensionStatus) (*postgres.InstalledExtension, error) {
	// An extension pgop recorded as created but that is gone is no longer
	// pgop's (it may be created by someone else next).
	st.Created, st.OID, st.Owner = false, 0, ""
	plan, err := r.planCreate(ctx, ext, st)
	if err != nil {
		return nil, err
	}
	toCreate, err := r.checkInstallSchemas(ctx, ext, st, plan)
	if err != nil {
		return nil, err
	}
	if err := r.createSchemas(ctx, ext, st, toCreate, plan.strict); err != nil {
		return nil, err
	}

	// Record the creation before it happens (an intent log, as for grants):
	// a status update lost after CREATE EXTENSION must not make pgop forget
	// that it created the extension. The record only becomes proof once the
	// extension's oid and owner are recorded too (below); a failed create
	// clears it.
	st.Created = true
	if err := r.persist(ctx, *st); err != nil {
		st.Created = false
		return nil, fmt.Errorf("extension %s: recording the extension pgop is about to create failed, nothing was "+
			"created: %w", ext.Name, err)
	}
	if err := r.pg.CreateExtension(ctx, ext.Name, ext.Schema, ext.Version, ext.Cascade); err != nil {
		st.Created = false
		if errors.Is(err, postgres.ErrObjectExists) {
			inst, lookupErr := r.pg.InstalledExtension(ctx, ext.Name)
			return inst, errors.Join(lookupErr, refuse(st, ReasonExtensionNotManaged,
				"was created by someone else while pgop was creating it; it is not recorded as created by pgop"))
		}
		return nil, err
	}
	inst, err := r.pg.InstalledExtension(ctx, ext.Name)
	if inst != nil {
		st.OID, st.Owner = inst.OID, inst.Owner
	}
	return inst, err
}

// update updates an installed extension to the requested version.
func (r *extensionRun) update(ctx context.Context, ext postgresv1alpha1.ExtensionSpec, inst *postgres.InstalledExtension,
	st *postgresv1alpha1.ExtensionStatus) (*postgres.InstalledExtension, error) {
	if cmp, ok := compareExtensionVersions(ext.Version, inst.Version); ok && cmp < 0 {
		return inst, refuse(st, ReasonExtensionDowngradeNotAllowed, "installed at version %s; pgop does not "+
			"downgrade to %s", inst.Version, ext.Version)
	}
	info, found, err := r.pg.ExtensionVersion(ctx, ext.Name, ext.Version)
	if err != nil {
		return inst, err
	}
	if !found {
		return inst, refuse(st, ReasonExtensionVersionNotAvailable, "%s is not available on the server "+
			"(its files are not in the image)", versionLabel(ext.Version))
	}
	paths, err := r.pg.ExtensionUpdatePaths(ctx, ext.Name, inst.Version, ext.Version)
	if err != nil {
		return inst, err
	}
	if len(paths) == 0 || len(paths[0]) < 2 {
		return inst, refuse(st, ReasonExtensionVersionNotAvailable, "the server has no update path from version %s to %s",
			inst.Version, ext.Version)
	}
	// ALTER EXTENSION ... UPDATE runs the update script of every step: each
	// version after the installed one must pass the policy.
	problem, strict, err := r.checkVersions(ctx, ext.Name, paths[0][1:])
	if err != nil {
		return inst, err
	}
	if problem != "" {
		return inst, refuse(st, ReasonExtensionNotAllowed, "not updated, it stays at version %s: %s", inst.Version, problem)
	}
	// ALTER EXTENSION ... UPDATE does not cascade.
	installedDeps, toInstall, unavailable, err := r.dependencies(ctx, info)
	if err != nil {
		return inst, err
	}
	if missing := append(extensionNames(toInstall), unavailable...); len(missing) > 0 {
		return inst, refuse(st, ReasonExtensionDependencyMissing, "version %s requires %s, which is not installed: "+
			"list it before this extension", ext.Version, strings.Join(missing, ", "))
	}
	schemas := []string{inst.Schema}
	for _, dep := range installedDeps {
		schemas = append(schemas, dep.Schema)
	}
	var problems []string
	for _, s := range uniqueSorted(schemas) {
		p, missing, err := r.schemaProblem(ctx, s, strict)
		if err != nil {
			return inst, err
		}
		if missing {
			p = fmt.Sprintf("schema %s does not exist", s)
		}
		if p != "" {
			problems = append(problems, p)
		}
	}
	if len(problems) > 0 {
		return inst, refuse(st, ReasonExtensionSchemaNotAllowed, "not updated: its update script would run as a "+
			"superuser in %s", strings.Join(problems, "; "))
	}
	if err := r.pg.UpdateExtension(ctx, ext.Name, ext.Version); err != nil {
		return inst, err
	}
	return r.pg.InstalledExtension(ctx, ext.Name)
}

// schemaProblem explains why an extension script must not run as a
// superuser in the schema name, or returns "". strict applies the rules for
// an extension that is not trusted. missing reports a schema that does not
// exist.
func (r *extensionRun) schemaProblem(ctx context.Context, name string, strict bool) (problem string, missing bool, err error) {
	w, err := r.pg.SchemaWriters(ctx, name)
	if err != nil || !w.Exists {
		return "", !w.Exists, err
	}
	return extensionSchemaProblem(name, w, r.dbOwner, r.dbOwnerSuper, r.managedSchemas[name], strict), false, nil
}

// extensionSchemaProblem decides whether an extension script may run as a
// superuser in the schema name, which w describes (see reconcileExtensions):
//
//   - always: the schema's owner must be a superuser, the database owner
//     (pg_database_owner stands for it) or the schema must be managed by the
//     Database, and no role other than the owner, a superuser or the
//     database owner, nor PUBLIC, may hold CREATE on it;
//   - strict (an extension that is not trusted): the owner must be a
//     superuser and only superusers may hold CREATE.
//
// It returns "" when the script may run there.
func extensionSchemaProblem(name string, w postgres.SchemaWriters, dbOwner string, dbOwnerSuper, managed, strict bool) string {
	owner, ownerSuper := w.Owner, w.OwnerSuperuser
	if owner == pgDatabaseOwnerRole {
		owner, ownerSuper = dbOwner, dbOwnerSuper
	}
	var problems []string
	creators := w.Creators
	if strict {
		if !ownerSuper {
			problems = append(problems, fmt.Sprintf("is owned by %s, which is not a superuser (an extension the server does "+
				"not mark trusted may only be installed in a schema only superusers can write to)", owner))
		}
	} else {
		if !ownerSuper && owner != dbOwner && !managed {
			problems = append(problems, fmt.Sprintf("is owned by %s, which is neither a superuser nor the database "+
				"owner, and the Database does not manage it", owner))
		}
		creators = slices.DeleteFunc(slices.Clone(creators), func(c string) bool { return c == dbOwner })
	}
	if len(creators) > 0 {
		problems = append(problems, fmt.Sprintf("%s can create objects in it", strings.Join(creators, ", ")))
	}
	if len(problems) == 0 {
		return ""
	}
	return fmt.Sprintf("schema %s, which %s", name, strings.Join(problems, " and "))
}

// dropRemoved drops the extensions that left the spec and that pgop created
// with dropOnRemoval recorded, and returns the status entries to keep for
// removed extensions (drops that failed). An extension is only dropped when
// it is still the installation pgop created (same oid and owner). listed is
// the number of status entries of the spec's extensions: the entries kept
// stay within extensionStatusLimit, the others are no longer tracked (and
// so never dropped) and reported.
func (r *extensionRun) dropRemoved(ctx context.Context, listed int, errs *[]error) []postgresv1alpha1.ExtensionStatus {
	var keep []postgresv1alpha1.ExtensionStatus
	for _, name := range slices.Sorted(maps.Keys(r.cur)) {
		st := r.cur[name]
		if r.listed[name] || !st.Created || !st.DropOnRemoval {
			continue
		}
		inst, err := r.pg.InstalledExtension(ctx, name)
		if err != nil {
			*errs = append(*errs, err)
			keep = append(keep, st)
			continue
		}
		if inst == nil {
			continue
		}
		if st.OID == 0 || inst.OID != st.OID || inst.Owner != st.Owner {
			*errs = append(*errs, refuse(&st, ReasonExtensionNotManaged, "removed from the spec with dropOnRemoval, but it "+
				"is not the installation pgop created (it was dropped and created again, or pgop could not confirm "+
				"its creation); it is not dropped and no longer tracked"))
			continue
		}
		err = r.pg.DropExtension(ctx, name)
		switch {
		case postgres.DependentObjectsExist(err):
			*errs = append(*errs, refuse(&st, ReasonExtensionDropBlocked, "removed from the spec with dropOnRemoval, but "+
				"other objects depend on it; pgop does not drop with CASCADE. Drop them, or list the extension again: %v", err))
			keep = append(keep, st)
		case err != nil:
			*errs = append(*errs, err)
			st.Reason, st.Message = ReasonReconcileError, err.Error()
			keep = append(keep, st)
		}
	}
	if room := max(extensionStatusLimit-listed, 0); len(keep) > room {
		dropped := make([]string, 0, len(keep)-room)
		for _, st := range keep[room:] {
			dropped = append(dropped, st.Name)
		}
		*errs = append(*errs, &conditionError{reason: ReasonExtensionDropBlocked, err: fmt.Errorf(
			"status.extensions is full: removed extensions %s are no longer tracked and will not be dropped by pgop",
			strings.Join(dropped, ", "))})
		keep = keep[:room]
	}
	return keep
}

// persist records st (with the rest of the working status) before pgop
// creates the extension. The record holds the spec's extensions and the
// removed ones pgop still has to drop, within extensionStatusLimit.
func (r *extensionRun) persist(ctx context.Context, st postgresv1alpha1.ExtensionStatus) error {
	r.cur[st.Name] = st
	if r.save == nil {
		return nil
	}
	var record, removed []postgresv1alpha1.ExtensionStatus
	for _, name := range slices.Sorted(maps.Keys(r.cur)) {
		switch e := r.cur[name]; {
		case r.listed[name]:
			record = append(record, e)
		case e.Created && e.DropOnRemoval:
			removed = append(removed, e)
		}
	}
	record = append(record, removed[:min(len(removed), max(extensionStatusLimit-len(record), 0))]...)
	saved := r.database.Status.Extensions
	r.database.Status.Extensions = record
	if err := r.save(ctx); err != nil {
		r.database.Status.Extensions = saved
		return err
	}
	return nil
}

// extensionNames returns the names of exts.
func extensionNames(exts []postgres.ExtensionVersionInfo) []string {
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		out = append(out, e.Name)
	}
	return out
}

// uniqueSorted returns s sorted without duplicates.
func uniqueSorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}

// versionLabel names a requested version in messages.
func versionLabel(version string) string {
	if version == "" {
		return "its default version"
	}
	return "version " + version
}

// compareExtensionVersions compares two extension versions made of numbers
// separated by dots (1.10 is higher than 1.9; 1.2 equals 1.2.0). ok is false
// when either version has another form (such as 1.0beta1): extension
// versions are free text, and only numeric ones can be ordered.
func compareExtensionVersions(a, b string) (cmp int, ok bool) {
	pa, oka := numericVersion(a)
	pb, okb := numericVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range max(len(pa), len(pb)) {
		var x, y uint64
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		switch {
		case x < y:
			return -1, true
		case x > y:
			return 1, true
		}
	}
	return 0, true
}

// numericVersion splits a dotted numeric version into its numbers.
func numericVersion(v string) ([]uint64, bool) {
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]uint64, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil || p == "" || p[0] == '+' {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
