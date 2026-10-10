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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Object grants (spec.schemas[].objectGrants) and default privileges
// (spec.schemas[].defaultPrivileges).
//
// pgop runs GRANT as a superuser, so it could grant on any object in the
// database whatever its owner. It deliberately does not: a Database writer
// may only reach objects that belong to the Cluster's trust domain. An
// object is granted on only when
//
//   - it is in a schema the Database manages (status.createdSchemas; never a
//     system schema),
//   - it does not belong to an extension (pg_depend deptype 'e'; those are
//     granted on through spec.extensions[].grants, with its own rules), and
//   - its owner is the database owner or a role managed by a Role of the
//     same Cluster (neither a superuser), or the operator itself. The
//     schema's declared owner counts only as such a role: a Database writer
//     can declare any existing role the owner of an existing schema it owns
//     (see reconcileSchemas), which must not reach that role's objects.
//
// Objects the operator owns run with superuser privileges where they act as
// their owner, so the extension-grant rules apply to them: EXECUTE only on
// SQL and PL/pgSQL functions that are not SECURITY DEFINER, table
// privileges only on plain and partitioned tables, and never TRIGGER or
// MAINTAIN (a trigger, or an index expression evaluated by maintenance, on a
// table superuser jobs write to would run code as that superuser). Anything
// else is skipped and reported (reason ObjectGrantSkipped).
//
// Objects are selected by name or with "*" (every object of the kind in the
// schema), resolved by one catalog query per kind and schema on every
// reconcile. The grant engine tracks the result per object and grantee
// (status.managedObjectGrants): an object that is no longer selected (no
// longer listed, or no longer eligible) has the privileges pgop added
// revoked, and one that was dropped is forgotten.

// Limits.
const (
	// objectGrantLedgerLimit and defaultPrivilegeLedgerLimit are the maxItems
	// of status.managedObjectGrants and status.managedDefaultPrivileges.
	objectGrantLedgerLimit      ledgerLimit = 4096
	defaultPrivilegeLedgerLimit ledgerLimit = 2048
	// maxObjectsPerKind is the most objects "*" may select per kind and
	// schema.
	maxObjectsPerKind = 5000
	// maxSkippedExamples is the maxItems of
	// status.objectGrants[].skippedExamples.
	maxSkippedExamples = 5
	// selectAll is the objects entry that selects every object of a kind.
	selectAll = "*"
)

// Routine languages whose functions run with the caller's privileges and
// can be granted on when a superuser owns them.
var invokerSafeLanguages = []string{"sql", "plpgsql"}

// objectKindInfo ties an object grant kind to its PostgreSQL kind and to
// what ALL expands to.
type objectKindInfo struct {
	api postgresv1alpha1.ObjectGrantKind
	pg  postgres.SchemaObjectKind
}

var objectKindInfos = []objectKindInfo{
	{postgresv1alpha1.ObjectGrantTable, postgres.SchemaTable},
	{postgresv1alpha1.ObjectGrantSequence, postgres.SchemaSequence},
	{postgresv1alpha1.ObjectGrantFunction, postgres.SchemaFunction},
	{postgresv1alpha1.ObjectGrantProcedure, postgres.SchemaProcedure},
	{postgresv1alpha1.ObjectGrantType, postgres.SchemaType},
}

// objectKindOf returns the object kind with the API or engine kind k.
func objectKindOf[K postgresv1alpha1.ObjectGrantKind | postgres.ObjectKind](k K) (objectKindInfo, bool) {
	for _, ki := range objectKindInfos {
		if string(ki.api) == string(k) || string(ki.pg) == string(k) {
			return ki, true
		}
	}
	return objectKindInfo{}, false
}

// isRoutine reports whether objects of the kind are functions or procedures.
func (ki objectKindInfo) isRoutine() bool {
	return ki.pg == postgres.SchemaFunction || ki.pg == postgres.SchemaProcedure
}

// normalizeObjectPrivileges validates privileges for objects of kind and
// returns them upper case, with ALL expanded (to every privilege of the kind
// but MAINTAIN), sorted and without duplicates.
func normalizeObjectPrivileges(kind postgres.SchemaObjectKind, privileges []string) ([]string, error) {
	allowed := postgres.SchemaObjectPrivileges[kind]
	if len(privileges) == 0 {
		return nil, fmt.Errorf("no %s privileges given", strings.ToLower(string(kind)))
	}
	var out []string
	for _, p := range privileges {
		u := strings.ToUpper(strings.Join(strings.Fields(p), " "))
		switch {
		case u == postgres.PrivilegeAll || u == allPrivilegesKeyword:
			out = append(out, subtract(allowed, []string{postgres.PrivilegeMaintain})...)
		case slices.Contains(allowed, u):
			out = append(out, u)
		default:
			return nil, fmt.Errorf("invalid %s privilege %q (allowed: %s, ALL)", strings.ToLower(string(kind)), p,
				strings.Join(allowed, ", "))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// dropMaintain removes MAINTAIN from privs on servers older than
// PostgreSQL 17 and reports whether it did.
func dropMaintain(privs []string, serverVersion int) ([]string, bool) {
	if serverVersion >= postgres.MinMaintainPrivilegeVersion || !slices.Contains(privs, postgres.PrivilegeMaintain) {
		return privs, false
	}
	return slices.DeleteFunc(slices.Clone(privs), func(p string) bool { return p == postgres.PrivilegeMaintain }), true
}

// objectTarget is the ledger target of privileges on one schema object.
func objectTarget(kind postgres.SchemaObjectKind, schema, identity, grantee string) grantTarget {
	return grantTarget{Kind: postgres.ObjectKind(kind), Name: identity, Schema: schema, Grantee: postgres.CanonicalGrantee(grantee)}
}

// parseRoutineName splits a routine entry of objects into its name and
// argument list: "f(integer, text)" is ("f", "integer, text", true), "f" is
// ("f", "", false).
func parseRoutineName(s string) (name, args string, signature bool) {
	open := strings.IndexByte(s, '(')
	if open <= 0 || !strings.HasSuffix(s, ")") {
		return s, "", false
	}
	return s[:open], s[open+1 : len(s)-1], true
}

// objectGrantClient is the subset of *postgres.Client used to reconcile
// object grants and default privileges (on a connection to the database).
type objectGrantClient interface {
	granteeClient
	RoleExists(ctx context.Context, name string) (bool, error)
	SchemaExists(ctx context.Context, name string) (bool, error)
	CurrentDatabaseOwner(ctx context.Context) (string, error)
	RoleOIDs(ctx context.Context, names []string) (map[string]int64, error)
	ListSchemaObjects(ctx context.Context, schema string, kind postgres.SchemaObjectKind,
		sel postgres.SchemaObjectSelector) ([]postgres.SchemaObject, error)
	ResolveRoutine(ctx context.Context, schema, name, args string) (int64, bool, error)
	ResolveSchemaObject(ctx context.Context, kind postgres.SchemaObjectKind, schema, identity string) (string, bool, error)
	GrantOnSchemaObject(ctx context.Context, kind postgres.SchemaObjectKind, schema, identity string, oid int64,
		grantee string, privileges []string, withGrantOption bool) error
	RevokeOnSchemaObject(ctx context.Context, kind postgres.SchemaObjectKind, schema, identity, grantee string,
		privileges []string, mode postgres.RevokeMode) error
	HeldDefaultPrivileges(ctx context.Context, t postgres.DefaultPrivilegesTarget, grantee string) ([]string, []string, error)
	GrantDefaultPrivileges(ctx context.Context, t postgres.DefaultPrivilegesTarget, grantee string, privileges []string,
		withGrantOption bool) error
	RevokeDefaultPrivileges(ctx context.Context, t postgres.DefaultPrivilegesTarget, grantee string, privileges []string,
		mode postgres.RevokeMode) error
}

var _ objectGrantClient = (*postgres.Client)(nil)

// objectEntry is one spec.schemas[].objectGrants entry, normalized.
type objectEntry struct {
	schema, field   string
	kind            objectKindInfo
	grantee         string
	privileges      []string
	withGrantOption bool
	all             bool
	names           []string
}

// objectGroup is the objects of one kind in one schema the entries select.
type objectGroup struct {
	schema  string
	kind    objectKindInfo
	all     bool
	names   []string
	sigs    map[string]int64 // routine signature -> OID (absent: not found)
	badSigs map[string]bool  // routine signatures the server could not parse
	objects []postgres.SchemaObject
	// failed: the objects could not be listed (or "*" selects too many);
	// the ledger entries of the group are kept as they are.
	failed bool
}

func groupKey(schema string, kind postgres.SchemaObjectKind) string {
	return schema + "|" + string(kind)
}

// objectStats counts the selected objects of one group.
type objectStats struct {
	granted, skipped map[string]bool
	examples         map[string]string // identity -> why skipped
}

func (s *objectStats) skip(identity, why string) {
	s.skipped[identity] = true
	if _, ok := s.examples[identity]; !ok {
		s.examples[identity] = why
	}
}

// pickExamples returns up to maxSkippedExamples skipped objects with the
// reason, one per distinct reason first (an extension's many members must
// not hide a superuser's SECURITY DEFINER function), then the rest, each in
// object order.
func (s *objectStats) pickExamples() []string {
	ids := slices.Sorted(maps.Keys(s.examples))
	var out []string
	picked := map[string]bool{}
	reasons := map[string]bool{}
	for _, pass := range []bool{true, false} {
		for _, id := range ids {
			if len(out) == maxSkippedExamples {
				return out
			}
			why := s.examples[id]
			if picked[id] || (pass && reasons[why]) {
				continue
			}
			picked[id], reasons[why] = true, true
			out = append(out, id+" "+why)
		}
	}
	return out
}

// objectPlanner works out the desired object grants of one reconcile.
type objectPlanner struct {
	pg            objectGrantClient
	database      *postgresv1alpha1.Database
	checker       *granteeChecker
	serverVersion int
	dbOwner       string

	groups  map[string]*objectGroup
	order   []string // group keys in spec order
	stats   map[string]*objectStats
	desired map[string]privilegeGrant
	objects map[string]postgres.SchemaObject // target kind|identity -> object
	fields  []string
	byField map[string][]string // field -> desired keys

	notFound, refused, unsupported, tooMany []string
	listErrs                                []error
}

// entries normalizes the object grants of the managed schemas.
func (p *objectPlanner) entries(managedSchemas map[string]bool) ([]objectEntry, error) {
	var out []objectEntry
	for _, s := range p.database.Spec.Schemas {
		if !managedSchemas[s.Name] || systemSchemaName(s.Name) || len(s.ObjectGrants) == 0 {
			continue
		}
		field := fmt.Sprintf("schemas[%s].objectGrants", s.Name)
		if slices.Contains(p.fields, field) {
			return nil, fmt.Errorf("schemas: schema %q is listed more than once", s.Name)
		}
		p.fields = append(p.fields, field)
		for i, g := range s.ObjectGrants {
			e, err := p.entry(s.Name, fmt.Sprintf("%s[%d]", field, i), g)
			if err != nil {
				return nil, err
			}
			if len(e.privileges) > 0 {
				e.field = field
				out = append(out, e)
			}
		}
	}
	return out, nil
}

func (p *objectPlanner) entry(schema, at string, g postgresv1alpha1.ObjectGrantSpec) (objectEntry, error) {
	ki, ok := objectKindOf(g.Kind)
	if !ok {
		return objectEntry{}, fmt.Errorf("%s: unsupported kind %q", at, g.Kind)
	}
	privs, err := normalizeObjectPrivileges(ki.pg, g.Privileges)
	if err != nil {
		return objectEntry{}, fmt.Errorf("%s: %w", at, err)
	}
	grantee := postgres.CanonicalGrantee(g.Role)
	if grantee == postgres.PublicGrantee && g.WithGrantOption {
		return objectEntry{}, fmt.Errorf("%s: the grant option cannot be granted to PUBLIC", at)
	}
	if len(g.Objects) == 0 {
		return objectEntry{}, fmt.Errorf("%s: no objects given", at)
	}
	e := objectEntry{schema: schema, kind: ki, grantee: grantee, withGrantOption: g.WithGrantOption}
	for _, o := range g.Objects {
		if o == selectAll {
			if len(g.Objects) > 1 {
				return objectEntry{}, fmt.Errorf("%s: %q must be the only entry of objects", at, selectAll)
			}
			e.all = true
			continue
		}
		e.names = append(e.names, o)
	}
	var dropped bool
	if e.privileges, dropped = dropMaintain(privs, p.serverVersion); dropped {
		p.unsupported = append(p.unsupported, fmt.Sprintf("%s (%s): MAINTAIN", at, grantee))
	}
	return e, nil
}

// group returns the group of the entry's schema and kind, creating it.
func (p *objectPlanner) group(e objectEntry) *objectGroup {
	k := groupKey(e.schema, e.kind.pg)
	g := p.groups[k]
	if g == nil {
		g = &objectGroup{schema: e.schema, kind: e.kind, sigs: map[string]int64{}, badSigs: map[string]bool{}}
		p.groups[k] = g
		p.order = append(p.order, k)
		p.stats[k] = &objectStats{granted: map[string]bool{}, skipped: map[string]bool{}, examples: map[string]string{}}
	}
	return g
}

// list resolves the routine signatures of g and lists its objects with one
// catalog query.
func (p *objectPlanner) list(ctx context.Context, g *objectGroup) {
	sel := postgres.SchemaObjectSelector{All: g.all}
	if g.all {
		sel.Limit = maxObjectsPerKind + 1
	} else {
		for _, n := range slices.Compact(slices.Sorted(slices.Values(g.names))) {
			name, args, sig := parseRoutineName(n)
			if !g.kind.isRoutine() || !sig {
				sel.Names = append(sel.Names, n)
				continue
			}
			oid, found, err := p.pg.ResolveRoutine(ctx, g.schema, name, args)
			if err != nil {
				// A malformed signature (older servers raise an error): reported
				// once, here, not again as a name that matches nothing.
				p.notFound = append(p.notFound, fmt.Sprintf("%s %s in schema %s: %v", g.kind.api, n, g.schema, err))
				g.badSigs[n] = true
				continue
			}
			if found {
				g.sigs[n] = oid
				sel.OIDs = append(sel.OIDs, oid)
			}
		}
	}
	objects, err := p.pg.ListSchemaObjects(ctx, g.schema, g.kind.pg, sel)
	switch {
	case err != nil:
		g.failed = true
		p.listErrs = append(p.listErrs, err)
	case g.all && len(objects) > maxObjectsPerKind:
		g.failed = true
		p.tooMany = append(p.tooMany, fmt.Sprintf("schema %s has more than %d %ss", g.schema, maxObjectsPerKind, g.kind.api))
	default:
		g.objects = objects
	}
}

// selected returns the objects of g the entry selects, and reports the
// names that match nothing.
func (p *objectPlanner) selected(e objectEntry, g *objectGroup) []postgres.SchemaObject {
	if e.all {
		return g.objects
	}
	var out []postgres.SchemaObject
	for _, n := range e.names {
		_, _, sig := parseRoutineName(n)
		if e.kind.isRoutine() && g.badSigs[n] {
			continue
		}
		before := len(out)
		for _, o := range g.objects {
			if e.kind.isRoutine() && sig {
				if oid, ok := g.sigs[n]; ok && o.OID == oid {
					out = append(out, o)
				}
			} else if o.Name == n {
				out = append(out, o)
			}
		}
		if len(out) == before {
			p.notFound = append(p.notFound, fmt.Sprintf("%s (%s): %s %q does not exist in schema %s",
				e.field, e.grantee, e.kind.api, n, e.schema))
		}
	}
	return out
}

// ownerProblem explains why pgop does not grant on o at all, or returns ""
// when it may. denied lists privileges it does not grant on o.
func (p *objectPlanner) ownerProblem(ctx context.Context, kind objectKindInfo, o postgres.SchemaObject) (
	problem string, denied []string, err error) {
	switch {
	case o.Extension != "":
		return fmt.Sprintf("belongs to the extension %s (grant on it in spec.extensions[].grants)", o.Extension), nil, nil
	case o.OwnerIsSessionUser:
		problem, denied = operatorObjectProblem(kind, o)
		return problem, denied, nil
	case o.OwnerSuperuser:
		return "is owned by the superuser " + o.Owner, nil, nil
	case o.Owner == p.dbOwner:
		return "", nil, nil
	}
	managed, err := p.checker.managesRole(ctx, o.Owner)
	if err != nil || managed {
		return "", nil, err
	}
	return fmt.Sprintf("is owned by %s, which is neither the database owner nor a role managed by a Role of this Cluster",
		o.Owner), nil, nil
}

// relkindNames names the relation kinds a table entry selects.
var relkindNames = map[string]string{"v": "view", "m": "materialized view", "f": "foreign table"}

// operatorObjectProblem applies the rules for objects the operator (a
// superuser) owns.
func operatorObjectProblem(kind objectKindInfo, o postgres.SchemaObject) (string, []string) {
	switch {
	case kind.isRoutine() && o.SecurityDefiner:
		return "is a SECURITY DEFINER routine owned by the superuser " + o.Owner, nil
	case kind.isRoutine() && !slices.Contains(invokerSafeLanguages, o.Language):
		return fmt.Sprintf("is written in %s and owned by the superuser %s", o.Language, o.Owner), nil
	case kind.pg == postgres.SchemaTable && relkindNames[o.SubKind] != "":
		return fmt.Sprintf("is a %s owned by the superuser %s", relkindNames[o.SubKind], o.Owner), nil
	case kind.pg == postgres.SchemaTable:
		return "", []string{postgres.PrivilegeMaintain, postgres.PrivilegeTrigger}
	}
	return "", nil
}

// add records what entry e grants on o.
func (p *objectPlanner) add(ctx context.Context, e objectEntry, o postgres.SchemaObject) error {
	st := p.stats[groupKey(e.schema, e.kind.pg)]
	problem, denied, err := p.ownerProblem(ctx, e.kind, o)
	if err != nil {
		return err
	}
	privs := e.privileges
	if problem != "" {
		privs = nil // the whole object is skipped
	} else if d := intersect(privs, denied); len(d) > 0 {
		problem = fmt.Sprintf("%s: not granted on a table owned by the superuser %s", strings.Join(d, ", "), o.Owner)
		privs = subtract(privs, d)
	}
	if problem != "" {
		st.skip(o.Identity, problem)
		if !e.all {
			p.refused = append(p.refused, fmt.Sprintf("%s (%s): %s %s", e.field, e.grantee, o.Identity, problem))
		}
	}
	if len(privs) == 0 {
		return nil
	}
	st.granted[o.Identity] = true
	t := objectTarget(e.kind.pg, e.schema, o.Identity, e.grantee)
	d := desiredGrant(t, privs, e.withGrantOption)
	k := t.key()
	if prev, ok := p.desired[k]; ok {
		d = mergeGrant(prev, d)
	} else {
		p.byField[e.field] = append(p.byField[e.field], k)
	}
	p.desired[k] = d
	p.objects[string(t.Kind)+"|"+t.Name] = o
	return nil
}

// plan works out the desired grants of the managed schemas.
func (p *objectPlanner) plan(ctx context.Context, managedSchemas map[string]bool) error {
	entries, err := p.entries(managedSchemas)
	if err != nil {
		return err
	}
	for _, e := range entries {
		g := p.group(e)
		g.all = g.all || e.all
		g.names = append(g.names, e.names...)
	}
	for _, k := range p.order {
		p.list(ctx, p.groups[k])
	}
	for _, e := range entries {
		g := p.groups[groupKey(e.schema, e.kind.pg)]
		if g.failed {
			continue
		}
		for _, o := range p.selected(e, g) {
			if err := p.add(ctx, e, o); err != nil {
				return err
			}
		}
	}
	return nil
}

// status returns status.objectGrants: the counts of the groups listed now,
// and the previous counts of the groups that could not be listed.
func (p *objectPlanner) status() []postgresv1alpha1.ObjectGrantStatus {
	var out []postgresv1alpha1.ObjectGrantStatus
	for _, k := range p.order {
		g := p.groups[k]
		if g.failed {
			if i := slices.IndexFunc(p.database.Status.ObjectGrants, func(s postgresv1alpha1.ObjectGrantStatus) bool {
				return s.Schema == g.schema && s.Kind == g.kind.api
			}); i >= 0 {
				out = append(out, p.database.Status.ObjectGrants[i])
			}
			continue
		}
		st := p.stats[k]
		s := postgresv1alpha1.ObjectGrantStatus{Schema: g.schema, Kind: g.kind.api,
			Granted: int32(len(st.granted)), Skipped: int32(len(st.skipped))}
		s.SkippedExamples = st.pickExamples()
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b postgresv1alpha1.ObjectGrantStatus) int {
		return strings.Compare(a.Schema+"|"+string(a.Kind), b.Schema+"|"+string(b.Kind))
	})
	return out
}

// errs reports what the plan could not apply.
func (p *objectPlanner) errs() []error {
	errs := slices.Clone(p.listErrs)
	if len(p.refused) > 0 {
		errs = append(errs, &conditionError{reason: ReasonObjectGrantSkipped, err: fmt.Errorf(
			"objects not granted on (and privileges pgop granted on them revoked): %s", strings.Join(p.refused, "; "))})
	}
	if len(p.tooMany) > 0 {
		errs = append(errs, &conditionError{reason: ReasonTooManyObjects, err: fmt.Errorf(
			"%q selects at most %d objects per kind and schema; nothing was granted or revoked there: %s",
			selectAll, maxObjectsPerKind, strings.Join(p.tooMany, "; "))})
	}
	if len(p.notFound) > 0 {
		errs = append(errs, &conditionError{reason: ReasonObjectNotFound, err: fmt.Errorf(
			"objects not found (retried; migrations may not have created them yet): %s", strings.Join(p.notFound, "; "))})
	}
	if len(p.unsupported) > 0 {
		errs = append(errs, &conditionError{reason: ReasonUnsupportedServerVersion, err: fmt.Errorf(
			"the MAINTAIN privilege needs PostgreSQL 17 or later: %s", strings.Join(p.unsupported, "; "))})
	}
	return errs
}

// objectLedger converts status.managedObjectGrants to ledger entries.
func objectLedger(managed []postgresv1alpha1.ManagedObjectGrant) []privilegeGrant {
	out := make([]privilegeGrant, 0, len(managed))
	for _, m := range managed {
		ki, ok := objectKindOf(m.Kind)
		if !ok {
			continue
		}
		out = append(out, privilegeGrant{Target: objectTarget(ki.pg, m.Schema, m.Object, m.Role),
			Privileges: m.Privileges, GrantOptions: m.GrantOptions})
	}
	return out
}

// objectLedgerStatus converts ledger entries to status.managedObjectGrants.
func objectLedgerStatus(ledger []privilegeGrant) []postgresv1alpha1.ManagedObjectGrant {
	if len(ledger) == 0 {
		return nil
	}
	out := make([]postgresv1alpha1.ManagedObjectGrant, 0, len(ledger))
	for _, g := range ledger {
		ki, ok := objectKindOf(g.Target.Kind)
		if !ok {
			continue
		}
		out = append(out, postgresv1alpha1.ManagedObjectGrant{Schema: g.Target.Schema, Kind: ki.api, Object: g.Target.Name,
			Role: g.Target.Grantee, Privileges: g.Privileges, GrantOptions: g.GrantOptions})
	}
	return out
}

// objectGrantOps returns the engine ops for object grants. objects are the
// listed objects (their ACLs answer held); oids maps grantees to role OIDs.
func objectGrantOps(pg objectGrantClient, objects map[string]postgres.SchemaObject, oids map[string]int64) privilegeOps {
	roleIsGone := roleGone(pg)
	return privilegeOps{
		held: func(_ context.Context, t grantTarget) ([]string, []string, error) {
			o, ok := objects[string(t.Kind)+"|"+t.Name]
			if !ok {
				return nil, nil, nil
			}
			oid, known := oids[t.Grantee]
			if !postgres.IsPublic(t.Grantee) && !known {
				return nil, nil, nil
			}
			privs, grantable := o.Held(oid)
			return privs, grantable, nil
		},
		grant: func(ctx context.Context, t grantTarget, privileges []string, withGrantOption bool) error {
			// The OID the owner rules were checked on: a name that now stands
			// for another object is not granted on.
			oid := objects[string(t.Kind)+"|"+t.Name].OID
			err := pg.GrantOnSchemaObject(ctx, postgres.SchemaObjectKind(t.Kind), t.Schema, t.Name, oid, t.Grantee,
				privileges, withGrantOption)
			if errors.Is(err, postgres.ErrObjectGone) {
				return nil // dropped meanwhile: forgotten on the next reconcile
			}
			return err
		},
		revoke: func(ctx context.Context, t grantTarget, privileges []string, mode postgres.RevokeMode) error {
			return pg.RevokeOnSchemaObject(ctx, postgres.SchemaObjectKind(t.Kind), t.Schema, t.Name, t.Grantee, privileges, mode)
		},
		gone: func(ctx context.Context, t grantTarget) (bool, error) {
			if gone, err := roleIsGone(ctx, t); err != nil || gone {
				return gone, err
			}
			_, found, err := pg.ResolveSchemaObject(ctx, postgres.SchemaObjectKind(t.Kind), t.Schema, t.Name)
			return !found, err
		},
	}
}

// granteeOIDs returns the OIDs of the role grantees of desired (PUBLIC is 0).
func granteeOIDs(ctx context.Context, pg objectGrantClient, desired []privilegeGrant) (map[string]int64, error) {
	var names []string
	for _, d := range desired {
		if !postgres.IsPublic(d.Target.Grantee) && !slices.Contains(names, d.Target.Grantee) {
			names = append(names, d.Target.Grantee)
		}
	}
	if len(names) == 0 {
		return map[string]int64{}, nil
	}
	return pg.RoleOIDs(ctx, names)
}

// reconcileObjectGrants brings the privileges on schema objects to the state
// declared by spec.schemas[].objectGrants and records what pgop added in
// database.Status.ManagedObjectGrants and the counts in
// database.Status.ObjectGrants (the caller persists the status).
// managedSchemas are the schemas the Database manages (see reconcileSchemas).
func reconcileObjectGrants(ctx context.Context, pg objectGrantClient, database *postgresv1alpha1.Database,
	managedSchemas map[string]bool, serverVersion int, checker *granteeChecker, save statusSaver) error {
	dbOwner, err := pg.CurrentDatabaseOwner(ctx)
	if err != nil {
		return err
	}
	p := &objectPlanner{pg: pg, database: database, checker: checker, serverVersion: serverVersion, dbOwner: dbOwner,
		groups: map[string]*objectGroup{}, stats: map[string]*objectStats{},
		desired: map[string]privilegeGrant{}, objects: map[string]postgres.SchemaObject{}, byField: map[string][]string{}}
	if err := p.plan(ctx, managedSchemas); err != nil {
		return err
	}
	database.Status.ObjectGrants = p.status()

	// Ledger entries of groups that could not be listed are kept as they
	// are: their objects are unknown, so nothing is granted or revoked there.
	var kept []privilegeGrant
	managed := slices.DeleteFunc(objectLedger(database.Status.ManagedObjectGrants), func(g privilegeGrant) bool {
		if gr := p.groups[groupKey(g.Target.Schema, postgres.SchemaObjectKind(g.Target.Kind))]; gr != nil && gr.failed {
			kept = append(kept, g)
			return true
		}
		return false
	})
	withKept := func(ledger []privilegeGrant) []postgresv1alpha1.ManagedObjectGrant {
		m := map[string]privilegeGrant{}
		for _, g := range slices.Concat(ledger, kept) {
			m[g.key()] = g
		}
		return objectLedgerStatus(sortedGrants(m))
	}

	held := &grantFilter{checker: checker}
	var desired []privilegeGrant
	for _, field := range p.fields {
		list := make([]privilegeGrant, 0, len(p.byField[field]))
		for _, k := range p.byField[field] {
			list = append(list, p.desired[k])
		}
		allowed, err := held.filter(ctx, field, list)
		if err != nil {
			return err
		}
		desired = append(desired, allowed...)
	}
	errs := p.errs()
	if len(desired) > 0 || len(managed) > 0 {
		oids, err := granteeOIDs(ctx, pg, desired)
		if err != nil {
			return err
		}
		persist := func(ctx context.Context, ledger []privilegeGrant) error {
			database.Status.ManagedObjectGrants = withKept(ledger)
			if save == nil {
				return nil
			}
			return save(ctx)
		}
		after, err := applyPrivilegeGrants(ctx, desired, managed, objectGrantOps(pg, p.objects, oids),
			max(objectGrantLedgerLimit-ledgerLimit(len(kept)), 1), persist)
		database.Status.ManagedObjectGrants = withKept(after)
		errs = append([]error{err}, errs...)
	}
	return errors.Join(append(errs, held.err())...)
}

// setObjectGrantsCondition sets the ObjectGrantsComplete condition from
// status.objectGrants (removed when the Database has no object grants).
func setObjectGrantsCondition(database *postgresv1alpha1.Database) {
	if len(database.Status.ObjectGrants) == 0 {
		meta.RemoveStatusCondition(&database.Status.Conditions, ConditionTypeObjectGrantsComplete)
		return
	}
	var skipped []string
	total := 0
	for _, s := range database.Status.ObjectGrants {
		if s.Skipped == 0 {
			continue
		}
		total += int(s.Skipped)
		skipped = append(skipped, fmt.Sprintf("%d %s(s) in schema %s (%s)", s.Skipped, s.Kind, s.Schema,
			strings.Join(s.SkippedExamples, "; ")))
	}
	c := metav1.Condition{Type: ConditionTypeObjectGrantsComplete, Status: metav1.ConditionTrue,
		ObservedGeneration: database.Generation, Reason: ReasonAllObjectsGranted,
		Message: "every object the object grants select is granted on"}
	if total > 0 {
		c.Status = metav1.ConditionFalse
		c.Reason = ReasonObjectGrantSkipped
		c.Message = fmt.Sprintf("%d selected objects are not granted on (their owner is not in the Cluster's trust "+
			"domain, they belong to an extension, or they act with superuser privileges): %s", total,
			strings.Join(skipped, "; "))
	}
	meta.SetStatusCondition(&database.Status.Conditions, c)
}

// defaultKindInfo ties a default privilege kind to its PostgreSQL kind.
type defaultKindInfo struct {
	api    postgresv1alpha1.DefaultPrivilegeKind
	pg     postgres.DefaultObjectKind
	object postgres.SchemaObjectKind
}

var defaultKindInfos = []defaultKindInfo{
	{postgresv1alpha1.DefaultPrivilegeTable, postgres.DefaultTables, postgres.SchemaTable},
	{postgresv1alpha1.DefaultPrivilegeSequence, postgres.DefaultSequences, postgres.SchemaSequence},
	{postgresv1alpha1.DefaultPrivilegeFunction, postgres.DefaultFunctions, postgres.SchemaFunction},
	{postgresv1alpha1.DefaultPrivilegeType, postgres.DefaultTypes, postgres.SchemaType},
}

// engine is the engine kind of the default privileges.
func (ki defaultKindInfo) engine() postgres.ObjectKind {
	return postgres.ObjectKind("DEFAULT " + string(ki.pg))
}

// defaultKindOf returns the default privilege kind with the API or engine
// kind k.
func defaultKindOf[K postgresv1alpha1.DefaultPrivilegeKind | postgres.ObjectKind](k K) (defaultKindInfo, bool) {
	for _, ki := range defaultKindInfos {
		if string(ki.api) == string(k) || string(ki.engine()) == string(k) {
			return ki, true
		}
	}
	return defaultKindInfo{}, false
}

// defaultTarget is the ledger target of default privileges.
func defaultTarget(ki defaultKindInfo, schema, forRole, grantee string) grantTarget {
	return grantTarget{Kind: ki.engine(), Name: schema, ForRole: forRole, Grantee: postgres.CanonicalGrantee(grantee)}
}

// pgTarget returns the PostgreSQL default privileges a target names.
func defaultPGTarget(t grantTarget) (postgres.DefaultPrivilegesTarget, error) {
	ki, ok := defaultKindOf(t.Kind)
	if !ok {
		return postgres.DefaultPrivilegesTarget{}, fmt.Errorf("unsupported default privilege kind %q", t.Kind)
	}
	return postgres.DefaultPrivilegesTarget{ForRole: t.ForRole, Schema: t.Name, Kind: ki.pg}, nil
}

// desiredDefaultPrivileges converts spec.schemas[].defaultPrivileges of the
// managed schemas to privilege grants, grouped by spec field.
func desiredDefaultPrivileges(database *postgresv1alpha1.Database, managedSchemas map[string]bool, serverVersion int) (
	fields []string, grants map[string][]privilegeGrant, unsupported []string, err error) {
	grants = map[string][]privilegeGrant{}
	seen := map[string]bool{}
	for _, s := range database.Spec.Schemas {
		if !managedSchemas[s.Name] || systemSchemaName(s.Name) || len(s.DefaultPrivileges) == 0 {
			continue
		}
		field := fmt.Sprintf("schemas[%s].defaultPrivileges", s.Name)
		if _, dup := grants[field]; dup {
			return nil, nil, nil, fmt.Errorf("schemas: schema %q is listed more than once", s.Name)
		}
		fields = append(fields, field)
		var list []privilegeGrant
		for _, d := range s.DefaultPrivileges {
			ki, ok := defaultKindOf(d.Kind)
			if !ok {
				return nil, nil, nil, fmt.Errorf("%s: unsupported kind %q", field, d.Kind)
			}
			privs, err := normalizeObjectPrivileges(ki.object, d.Privileges)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("%s[%s/%s]: %w", field, d.ForRole, d.Role, err)
			}
			t := defaultTarget(ki, s.Name, d.ForRole, d.Role)
			if seen[t.key()] {
				return nil, nil, nil, fmt.Errorf("%s: forRole %q, role %q and kind %s are listed more than once",
					field, d.ForRole, t.Grantee, d.Kind)
			}
			seen[t.key()] = true
			if t.Grantee == postgres.PublicGrantee && d.WithGrantOption {
				return nil, nil, nil, fmt.Errorf("%s[%s/PUBLIC]: the grant option cannot be granted to PUBLIC", field, d.ForRole)
			}
			var dropped bool
			if privs, dropped = dropMaintain(privs, serverVersion); dropped {
				unsupported = append(unsupported, fmt.Sprintf("%s[%s/%s]: MAINTAIN", field, d.ForRole, t.Grantee))
			}
			if len(privs) > 0 {
				list = append(list, desiredGrant(t, privs, d.WithGrantOption))
			}
		}
		grants[field] = list
	}
	return fields, grants, unsupported, nil
}

// forRoleFilter holds back default privileges whose forRole is not allowed.
type forRoleFilter struct {
	checker                  *granteeChecker
	refused, missing, paused []string
}

func (f *forRoleFilter) filter(ctx context.Context, field string, desired []privilegeGrant) ([]privilegeGrant, error) {
	out := make([]privilegeGrant, 0, len(desired))
	for _, g := range desired {
		res, err := f.checker.checkForRole(ctx, g.Target.ForRole)
		if err != nil {
			return nil, err
		}
		switch res.verdict {
		case granteeAllowed:
			out = append(out, g)
		case granteeMissing:
			f.missing = append(f.missing, fmt.Sprintf("%s: forRole %q does not exist yet", field, g.Target.ForRole))
		case granteePaused:
			f.paused = append(f.paused, fmt.Sprintf("%s: default privileges for %s are paused: the Role is being deleted",
				field, g.Target.ForRole))
		case granteeRefused:
			f.refused = append(f.refused, fmt.Sprintf("%s: forRole %s %s", field, g.Target.ForRole, res.problem))
		}
	}
	return out, nil
}

func (f *forRoleFilter) err() error {
	var errs []error
	if len(f.refused) > 0 {
		errs = append(errs, &conditionError{reason: ReasonDefaultPrivilegeNotAllowed, err: fmt.Errorf(
			"default privileges not applied (and removed if pgop set them): %s", strings.Join(f.refused, "; "))})
	}
	for _, m := range [][]string{f.missing, f.paused} {
		if len(m) > 0 {
			errs = append(errs, errors.New(strings.Join(m, "; ")))
		}
	}
	return errors.Join(errs...)
}

// defaultLedger converts status.managedDefaultPrivileges to ledger entries.
func defaultLedger(managed []postgresv1alpha1.ManagedDefaultPrivilege) []privilegeGrant {
	out := make([]privilegeGrant, 0, len(managed))
	for _, m := range managed {
		ki, ok := defaultKindOf(m.Kind)
		if !ok {
			continue
		}
		out = append(out, privilegeGrant{Target: defaultTarget(ki, m.Schema, m.ForRole, m.Role),
			Privileges: m.Privileges, GrantOptions: m.GrantOptions})
	}
	return out
}

// defaultLedgerStatus converts ledger entries to
// status.managedDefaultPrivileges.
func defaultLedgerStatus(ledger []privilegeGrant) []postgresv1alpha1.ManagedDefaultPrivilege {
	if len(ledger) == 0 {
		return nil
	}
	out := make([]postgresv1alpha1.ManagedDefaultPrivilege, 0, len(ledger))
	for _, g := range ledger {
		ki, ok := defaultKindOf(g.Target.Kind)
		if !ok {
			continue
		}
		out = append(out, postgresv1alpha1.ManagedDefaultPrivilege{Schema: g.Target.Name, ForRole: g.Target.ForRole,
			Kind: ki.api, Role: g.Target.Grantee, Privileges: g.Privileges, GrantOptions: g.GrantOptions})
	}
	return out
}

// defaultPrivilegeOps returns the engine ops for default privileges.
func defaultPrivilegeOps(pg objectGrantClient) privilegeOps {
	roleIsGone := roleGone(pg)
	return privilegeOps{
		held: func(ctx context.Context, t grantTarget) ([]string, []string, error) {
			dt, err := defaultPGTarget(t)
			if err != nil {
				return nil, nil, err
			}
			return pg.HeldDefaultPrivileges(ctx, dt, t.Grantee)
		},
		grant: func(ctx context.Context, t grantTarget, privileges []string, withGrantOption bool) error {
			dt, err := defaultPGTarget(t)
			if err != nil {
				return err
			}
			return pg.GrantDefaultPrivileges(ctx, dt, t.Grantee, privileges, withGrantOption)
		},
		revoke: func(ctx context.Context, t grantTarget, privileges []string, mode postgres.RevokeMode) error {
			dt, err := defaultPGTarget(t)
			if err != nil {
				return err
			}
			return pg.RevokeDefaultPrivileges(ctx, dt, t.Grantee, privileges, mode)
		},
		gone: func(ctx context.Context, t grantTarget) (bool, error) {
			if exists, err := pg.SchemaExists(ctx, t.Name); err != nil || !exists {
				return !exists, err
			}
			if exists, err := pg.RoleExists(ctx, t.ForRole); err != nil || !exists {
				return !exists, err
			}
			return roleIsGone(ctx, t)
		},
	}
}

// reconcileDefaultPrivileges brings the default privileges to the state
// declared by spec.schemas[].defaultPrivileges and records what pgop added in
// database.Status.ManagedDefaultPrivileges (the caller persists the status).
func reconcileDefaultPrivileges(ctx context.Context, pg objectGrantClient, database *postgresv1alpha1.Database,
	managedSchemas map[string]bool, serverVersion int, checker *granteeChecker, save statusSaver) error {
	fields, all, unsupported, err := desiredDefaultPrivileges(database, managedSchemas, serverVersion)
	if err != nil {
		return err
	}
	managed := defaultLedger(database.Status.ManagedDefaultPrivileges)
	if len(fields) == 0 && len(managed) == 0 {
		return nil
	}
	forRoles := &forRoleFilter{checker: checker}
	held := &grantFilter{checker: checker}
	var desired []privilegeGrant
	for _, field := range fields {
		allowed, err := forRoles.filter(ctx, field, all[field])
		if err != nil {
			return err
		}
		if allowed, err = held.filter(ctx, field, allowed); err != nil {
			return err
		}
		desired = append(desired, allowed...)
	}
	persist := func(ctx context.Context, ledger []privilegeGrant) error {
		database.Status.ManagedDefaultPrivileges = defaultLedgerStatus(ledger)
		if save == nil {
			return nil
		}
		return save(ctx)
	}
	after, err := applyPrivilegeGrants(ctx, desired, managed, defaultPrivilegeOps(pg), defaultPrivilegeLedgerLimit, persist)
	database.Status.ManagedDefaultPrivileges = defaultLedgerStatus(after)
	errs := []error{err, forRoles.err(), held.err()}
	if len(unsupported) > 0 {
		errs = append(errs, &conditionError{reason: ReasonUnsupportedServerVersion, err: fmt.Errorf(
			"the MAINTAIN privilege needs PostgreSQL 17 or later: %s", strings.Join(unsupported, "; "))})
	}
	return errors.Join(errs...)
}

// hasObjectAccess reports whether the Database declares or tracks object
// grants or default privileges.
func hasObjectAccess(database *postgresv1alpha1.Database) bool {
	if len(database.Status.ManagedObjectGrants) > 0 || len(database.Status.ManagedDefaultPrivileges) > 0 {
		return true
	}
	return slices.ContainsFunc(database.Spec.Schemas, func(s postgresv1alpha1.SchemaSpec) bool {
		return len(s.ObjectGrants) > 0 || len(s.DefaultPrivileges) > 0
	})
}

// reconcileObjectAccess runs reconcileObjectGrants and
// reconcileDefaultPrivileges on a connection to the database and sets the
// ObjectGrantsComplete condition.
func reconcileObjectAccess(ctx context.Context, pg objectGrantClient, version func(context.Context) (int, error),
	database *postgresv1alpha1.Database, managedSchemas []string, checker *granteeChecker, save statusSaver) error {
	defer setObjectGrantsCondition(database)
	if !hasObjectAccess(database) {
		database.Status.ObjectGrants = nil
		return nil
	}
	serverVersion, err := version(ctx)
	if err != nil {
		return err
	}
	schemas := setOf(managedSchemas)
	err = errors.Join(
		reconcileObjectGrants(ctx, pg, database, schemas, serverVersion, checker, save),
		reconcileDefaultPrivileges(ctx, pg, database, schemas, serverVersion, checker, save),
	)
	if err != nil {
		logf.FromContext(ctx).Error(err, "Failed to reconcile object grants or default privileges")
	}
	return err
}

// objectAccessNames reports whether spec.schemas[].objectGrants or
// defaultPrivileges name the PostgreSQL role pgName (as grantee or forRole).
func objectAccessNames(database *postgresv1alpha1.Database, pgName string) bool {
	return slices.ContainsFunc(database.Spec.Schemas, func(s postgresv1alpha1.SchemaSpec) bool {
		return slices.ContainsFunc(s.ObjectGrants, func(g postgresv1alpha1.ObjectGrantSpec) bool { return g.Role == pgName }) ||
			slices.ContainsFunc(s.DefaultPrivileges, func(d postgresv1alpha1.DefaultPrivilegeSpec) bool {
				return d.Role == pgName || d.ForRole == pgName
			})
	})
}
