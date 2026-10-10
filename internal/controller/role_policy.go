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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// This file holds the security policy for what Role and Database writers can
// obtain in PostgreSQL. Whoever can create or edit Roles and Databases on a
// Cluster must not become superuser-equivalent through pgop, which runs every
// statement as a superuser. The Cluster's spec.rolePolicy (editable only by
// whoever can edit the Cluster) opts into privileged attributes, predefined
// roles and untrusted extensions.

// reservedRolePrefix is the prefix of the operator's own roles (pgop_operator,
// pgop_replicator, pgop_replica_N). Role resources cannot use it.
const reservedRolePrefix = "pgop_"

// bootstrapRoleName is the default name of PostgreSQL's bootstrap superuser.
// pgop's clusters use their own operator role, but the name stays reserved.
const bootstrapRoleName = "postgres"

// Ownership.
//
// pgop only alters, re-passwords or drops an existing PostgreSQL role or
// database when
//
//   - the resource's own status records that exact name (status.roleName /
//     status.databaseName): pgop created or adopted it earlier. Status cannot
//     be written by Role or Database writers;
//   - it creates the object itself in this reconcile; or
//   - a Cluster editor allowlisted the name in spec.rolePolicy.adoptableRoles
//     / adoptableDatabases (and, for a role, it passes the privilege checks).
//
// A comment can be set by people pgop does not trust (a database's owner, a
// role's ADMIN holder, any CREATEROLE role on PostgreSQL 15 and older), so a
// comment never authorizes a take-over. pgop still stores a signed marker
// with every object it manages:
//
//	pgop:v2:<Kind>/<name>:<base64url HMAC-SHA256(key, "<kind>|<namespace>|<cluster>|<name>")>
//
// keyed by the Cluster's <cluster>-marker-key Secret. It serves as a
// consistency check (an object recorded in status that carries an unrelated
// comment is left alone) and tells which roles Roles of the Cluster manage
// (membership policy). Markers that are missing, unsigned (v1) or signed with
// a lost key are refreshed on objects recorded in status.
const (
	markerV2Prefix = "pgop:v2:"
	// markerV1Prefix marked objects before markers were signed.
	markerV1Prefix = "pgop:v1:"

	markerKindRole     = "Role"
	markerKindDatabase = "Database"
)

// markerSigner computes and checks the ownership markers of one Cluster.
type markerSigner struct {
	key       []byte
	namespace string
	cluster   string
}

func newMarkerSigner(cluster *postgresv1alpha1.Cluster, key []byte) markerSigner {
	return markerSigner{key: key, namespace: cluster.Namespace, cluster: cluster.Name}
}

// keyID identifies the key (a short hash of it), so a marker signed with a
// lost key can be told apart from one forged under the current key.
func (s markerSigner) keyID() string {
	sum := sha256.Sum256(s.key)
	return base64.RawURLEncoding.EncodeToString(sum[:6])
}

// marker returns the marker of the resource kind/name:
// pgop:v2:<Kind>/<name>:<key id>.<HMAC>.
func (s markerSigner) marker(kind, name string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(kind + "|" + s.namespace + "|" + s.cluster + "|" + name))
	return markerV2Prefix + kind + "/" + name + ":" + s.keyID() + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// otherKeyMarker reports whether comment is a v2 marker of kind/name that
// names a key other than the current one (the key Secret was lost and
// regenerated). A v2 marker naming the current key is only accepted if its
// signature verifies.
func (s markerSigner) otherKeyMarker(comment, kind, name string) bool {
	rest, ok := strings.CutPrefix(comment, markerV2Prefix+kind+"/"+name+":")
	if !ok {
		return false
	}
	id, _, ok := strings.Cut(rest, ".")
	return ok && id != s.keyID()
}

// verify reports, in constant time, whether comment is the marker of
// kind/name.
func (s markerSigner) verify(comment, kind, name string) bool {
	return len(s.key) > 0 && hmac.Equal([]byte(comment), []byte(s.marker(kind, name)))
}

// legacyMarker is the unsigned marker an earlier pgop stored.
func legacyMarker(kind, name string) string {
	return markerV1Prefix + kind + "/" + name
}

// refreshableComment reports whether comment, on an object the resource's
// status records on this Cluster, is a marker pgop should refresh: empty,
// the unsigned v1 marker of an earlier build, or a v2 marker of that name
// signed with a key that is no longer the Cluster's. A v2 marker that names
// the current key but does not verify is not (it was not written by pgop).
func refreshableComment(s markerSigner, comment, kind, name string) bool {
	return comment == "" || comment == legacyMarker(kind, name) || s.otherKeyMarker(comment, kind, name)
}

// ownership is how a resource relates to the PostgreSQL object of its name.
type ownership int

const (
	// ownedAbsent: the object does not exist; create it with the marker.
	ownedAbsent ownership = iota
	// owned: the status records the object and it carries the valid marker.
	owned
	// ownedRemark: the status records the object; its marker is missing,
	// unsigned or stale (lost key): refresh it.
	ownedRemark
	// adoptable: not recorded, but a Cluster editor allowlisted the name.
	adoptable
	// notOwned: leave the object alone.
	notOwned
)

// decideOwnership classifies the PostgreSQL object of kind/name. recorded
// reports whether the resource's status records the name on the referenced
// Cluster (same Cluster UID); allowlisted whether the Cluster's rolePolicy
// lists it as adoptable. The comment never grants ownership on its own.
func decideOwnership(s markerSigner, kind, name string, exists bool, comment string, recorded, allowlisted bool) ownership {
	switch {
	case !exists:
		return ownedAbsent
	case recorded && s.verify(comment, kind, name):
		return owned
	case recorded && refreshableComment(s, comment, kind, name):
		return ownedRemark
	case allowlisted:
		return adoptable
	}
	return notOwned
}

// notManagedMessage explains why pgop leaves an existing object alone and
// how a Cluster editor can let the resource take it over.
func notManagedMessage(kind, name string, recorded bool) string {
	field := "adoptableRoles"
	statusField := "status.roleName"
	if kind == "database" {
		field, statusField = "adoptableDatabases", "status.databaseName"
	}
	if recorded {
		return fmt.Sprintf("the PostgreSQL %s %s carries a comment that is not this resource's ownership marker; pgop "+
			"leaves it alone. To let this resource manage it again, a Cluster editor adds %s to the Cluster's "+
			"spec.rolePolicy.%s", kind, name, name, field)
	}
	return fmt.Sprintf("the PostgreSQL %s %s already exists and this resource did not create it (%s does not record "+
		"it); pgop does not take it over, since that would reset its owner, settings or password and could drop it. "+
		"To let this resource take it over (also after re-creating the resource without its status), a Cluster "+
		"editor adds %s to the Cluster's spec.rolePolicy.%s", kind, name, statusField, name, field)
}

// managedRoles maps the PostgreSQL role names of a Cluster's Roles to the
// ownership marker each must carry to count as managed by that Role.
type managedRoles map[string]string

// manages reports whether r is a role a Role of the Cluster manages: its name
// belongs to such a Role and it carries that Role's marker.
func (m managedRoles) manages(r postgres.ReachableRole) bool {
	marker, ok := m[r.Name]
	return ok && hmac.Equal([]byte(r.Comment), []byte(marker))
}

// reservedRoleName explains why name cannot be managed by a Role resource,
// or returns "" when it can. The CRD rejects the same names in
// spec.roleName; this also covers a Role whose metadata.name is used as the
// PostgreSQL name.
func reservedRoleName(name string) string {
	switch {
	case name == bootstrapRoleName:
		return fmt.Sprintf("the PostgreSQL role %q is reserved", name)
	case strings.HasPrefix(name, reservedRolePrefix):
		return fmt.Sprintf("role names starting with %q are reserved for the operator (%q)", reservedRolePrefix, name)
	case strings.HasPrefix(name, "pg_"):
		return fmt.Sprintf("role names starting with \"pg_\" are reserved by PostgreSQL (%q)", name)
	}
	return ""
}

// reservedDatabaseNames are the databases a Database resource cannot manage
// (or drop): the maintenance database and the templates every new database
// is copied from.
var reservedDatabaseNames = []string{defaultDatabaseName, "template0", template1Name}

// template1Name is the template every new database is copied from.
const template1Name = "template1"

// reservedDatabaseName explains why name cannot be managed by a Database
// resource, or returns "" when it can.
func reservedDatabaseName(name string) string {
	if slices.Contains(reservedDatabaseNames, name) {
		return fmt.Sprintf("the PostgreSQL database %q is reserved", name)
	}
	return ""
}

// systemSchemaName reports whether name is a system schema (pg_catalog,
// pg_toast, pg_temp_N, ... or information_schema). Owning or creating objects
// in one of them affects every session in the database, superuser sessions
// included.
func systemSchemaName(name string) bool {
	return strings.HasPrefix(name, "pg_") || name == "information_schema"
}

// membershipNameProblem explains why membership in the role name is never
// allowed, or returns "" when its name alone does not rule it out.
func membershipNameProblem(name string, policy *postgresv1alpha1.RolePolicySpec) string {
	switch {
	case name == bootstrapRoleName:
		return "is reserved (bootstrap superuser)"
	case strings.HasPrefix(name, reservedRolePrefix):
		return "is reserved for the operator"
	case slices.Contains(postgresv1alpha1.ForbiddenPredefinedRoles, name):
		return "gives access to the server's files or programs and can never be granted"
	case strings.HasPrefix(name, "pg_") && !policy.AllowsPredefinedRole(name):
		return "is a predefined role that the Cluster's spec.rolePolicy.allowedPredefinedRoles does not list"
	}
	return ""
}

// attributeProblem explains which attribute of r goes beyond the policy, or
// returns "" when none does.
func attributeProblem(r postgres.ReachableRole, policy *postgresv1alpha1.RolePolicySpec) string {
	var denied []string
	if r.CreateRole && !policy.AllowsAttribute(postgresv1alpha1.RoleAttributeCreateRole) {
		denied = append(denied, "CREATEROLE")
	}
	if r.Replication && !policy.AllowsAttribute(postgresv1alpha1.RoleAttributeReplication) {
		denied = append(denied, "REPLICATION")
	}
	if r.BypassRLS && !policy.AllowsAttribute(postgresv1alpha1.RoleAttributeBypassRLS) {
		denied = append(denied, "BYPASSRLS")
	}
	if len(denied) == 0 {
		return ""
	}
	return fmt.Sprintf("has %s, which the Cluster's spec.rolePolicy.allowedAttributes does not allow", strings.Join(denied, ", "))
}

// reachableRoleProblem explains why being able to act as r is not allowed,
// or returns "" when it is. managed lists the roles the Cluster's Roles
// manage: any other role (a DBA's, a bootstrap Job's, one restored from
// another Cluster) is only allowed when the policy lists it in
// allowedExistingRoles. Predefined pg_* roles are governed by
// allowedPredefinedRoles instead.
func reachableRoleProblem(r postgres.ReachableRole, policy *postgresv1alpha1.RolePolicySpec, managed managedRoles) string {
	if p := membershipNameProblem(r.Name, policy); p != "" {
		return p
	}
	if r.Superuser {
		return "is a superuser"
	}
	if p := attributeProblem(r, policy); p != "" {
		return p
	}
	if !strings.HasPrefix(r.Name, "pg_") && !managed.manages(r) && !policy.AllowsExistingRole(r.Name) {
		return "is not managed by a Role of this Cluster and the Cluster's spec.rolePolicy.allowedExistingRoles does not list it"
	}
	return ""
}

// builtinContainedRoles are the predefined roles PostgreSQL itself makes
// members of another predefined role (pg_monitor includes the roles that read
// settings and statistics).
var builtinContainedRoles = map[string][]string{
	"pg_monitor": {"pg_read_all_settings", "pg_read_all_stats", "pg_stat_scan_tables"},
}

// containedPredefinedRole reports whether reached is a predefined role that
// PostgreSQL places inside a predefined role of the closure that the policy
// allows, so allowing the outer role (for example pg_monitor) allows what it
// contains.
func containedPredefinedRole(reached string, closure []postgres.ReachableRole, policy *postgresv1alpha1.RolePolicySpec) bool {
	for _, r := range closure {
		if policy.AllowsPredefinedRole(r.Name) && slices.Contains(builtinContainedRoles[r.Name], reached) {
			return true
		}
	}
	return false
}

// membershipProblem explains why membership in target is not allowed, or
// returns "" when it is. closure is target's MembershipClosure (target
// itself followed by every role it is a member of, empty when target does
// not exist). Grant options are ignored on purpose: a member that cannot SET
// ROLE today can be given that option later, and memberships of target can
// change after this check.
func membershipProblem(target string, closure []postgres.ReachableRole, policy *postgresv1alpha1.RolePolicySpec, managed managedRoles) string {
	if p := membershipNameProblem(target, policy); p != "" {
		return fmt.Sprintf("%s %s", target, p)
	}
	for _, r := range closure {
		if r.Name != target && containedPredefinedRole(r.Name, closure, policy) {
			continue
		}
		p := reachableRoleProblem(r, policy, managed)
		switch {
		case p == "":
		case r.Name == target:
			return fmt.Sprintf("%s %s", target, p)
		default:
			return fmt.Sprintf("%s is a member of %s, which %s", target, r.Name, p)
		}
	}
	return ""
}

// adoptionProblem explains why pgop must not take over the existing role
// name, or returns "" when it may. closure is the role's MembershipClosure.
// Taking over a role sets its password and hands it to the Role writer, so a
// role that is a superuser, or that is already a member (granted outside
// pgop) of a role the policy does not allow, is refused. The role's own
// privileged attributes are not a reason to refuse: they are altered down to
// what the policy allows.
func adoptionProblem(name string, closure []postgres.ReachableRole, policy *postgresv1alpha1.RolePolicySpec, managed managedRoles) string {
	for _, r := range closure {
		if r.Via == "" {
			if r.Superuser {
				return fmt.Sprintf("pgop does not take over the existing PostgreSQL role %s: it is a superuser", name)
			}
			continue
		}
		if containedPredefinedRole(r.Name, closure, policy) {
			continue
		}
		if p := reachableRoleProblem(r, policy, managed); p != "" {
			return fmt.Sprintf("pgop does not take over the existing PostgreSQL role %s: it is a member of %s, which %s",
				name, r.Name, p)
		}
	}
	return ""
}

// deniedAttributes lists the privileged attributes spec requests that the
// policy does not allow.
func deniedAttributes(spec *postgresv1alpha1.RoleSpec, policy *postgresv1alpha1.RolePolicySpec) []postgresv1alpha1.RoleAttribute {
	var denied []postgresv1alpha1.RoleAttribute
	for _, a := range []struct {
		requested bool
		attr      postgresv1alpha1.RoleAttribute
	}{
		{spec.CreateRole, postgresv1alpha1.RoleAttributeCreateRole},
		{spec.Replication, postgresv1alpha1.RoleAttributeReplication},
		{spec.BypassRLS, postgresv1alpha1.RoleAttributeBypassRLS},
	} {
		if a.requested && !policy.AllowsAttribute(a.attr) {
			denied = append(denied, a.attr)
		}
	}
	return denied
}

// desiredRoleOptions returns the role attributes to apply for spec under
// policy, and a RolePolicyViolation error when spec requests a privileged
// attribute the policy does not allow. In that case none of the privileged
// attributes is applied (an existing role is altered to NOCREATEROLE
// NOREPLICATION NOBYPASSRLS): a role is never left with only part of the
// privileges it asked for.
func desiredRoleOptions(spec *postgresv1alpha1.RoleSpec, policy *postgresv1alpha1.RolePolicySpec) (postgres.RoleOptions, error) {
	opts := postgres.RoleOptions{
		Login:           spec.IsLogin(),
		CreateDB:        spec.CreateDB,
		CreateRole:      spec.CreateRole,
		Inherit:         spec.IsInherit(),
		Replication:     spec.Replication,
		BypassRLS:       spec.BypassRLS,
		ConnectionLimit: spec.GetConnectionLimit(),
	}
	denied := deniedAttributes(spec, policy)
	if len(denied) == 0 {
		return opts, nil
	}
	opts.CreateRole, opts.Replication, opts.BypassRLS = false, false, false
	names := make([]string, 0, len(denied))
	for _, a := range denied {
		names = append(names, string(a))
	}
	return opts, &conditionError{reason: ReasonRolePolicyViolation, err: fmt.Errorf(
		"the Cluster's spec.rolePolicy.allowedAttributes does not allow %s; the role has none of "+
			"createRole, replication and bypassRLS until the Cluster allows them or the Role stops requesting them",
		strings.Join(names, ", "))}
}
