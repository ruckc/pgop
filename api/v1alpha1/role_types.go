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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RoleMembership declares that this role is a member of another PostgreSQL role.
type RoleMembership struct {
	// role is the PostgreSQL name of the role to be a member of (a raw
	// PostgreSQL role name, not a Role resource name).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="!(self in ['postgres', 'pg_execute_server_program', 'pg_read_server_files', 'pg_write_server_files']) && !self.startsWith('pgop_')",message="membership in postgres, pgop_* roles, pg_execute_server_program, pg_read_server_files or pg_write_server_files is not allowed"
	Role string `json:"role"`

	// inherit sets the grant's INHERIT option: whether the member automatically
	// uses the privileges of the role. When unset, PostgreSQL's default applies
	// for a new grant (the member's own inherit attribute) and an existing grant
	// is left unchanged. Requires PostgreSQL 16 or later.
	// +optional
	Inherit *bool `json:"inherit,omitempty"`

	// set sets the grant's SET option: whether the member may SET ROLE to the
	// role. When unset, PostgreSQL's default applies for a new grant (true) and
	// an existing grant is left unchanged. Requires PostgreSQL 16 or later.
	// +optional
	Set *bool `json:"set,omitempty"`

	// admin sets the grant's ADMIN option: whether the member may grant
	// membership in the role to others. Defaults to false; a grant that has
	// the ADMIN option while admin is false has it revoked.
	// +optional
	Admin bool `json:"admin,omitempty"`
}

// PasswordRotationSpec configures scheduled rotation of an operator-generated
// role password.
type PasswordRotationSpec struct {
	// every is the rotation interval as a Go duration string (for example
	// "720h" for 30 days). The minimum is 1h. The schedule is measured from
	// status.passwordRotatedAt.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1h')",message="passwordRotation.every must be at least 1h"
	Every metav1.Duration `json:"every"`
}

// RoleSpec defines the desired state of Role
// +kubebuilder:validation:XValidation:rule="self.clusterRef.name == oldSelf.clusterRef.name",message="clusterRef is immutable; create a new Role for another Cluster"
// +kubebuilder:validation:XValidation:rule="!(has(self.passwordSecretRef) && has(self.passwordRotation))",message="passwordRotation cannot be combined with passwordSecretRef; rotate the referenced Secret instead"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.roleName) == has(self.roleName) && (!has(self.roleName) || self.roleName == oldSelf.roleName)",message="roleName is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.memberOf) || !has(self.memberships) || self.memberOf.all(r, !self.memberships.exists(m, m.role == r))",message="a role must not be listed in both memberOf and memberships"
type RoleSpec struct {
	// clusterRef references the PostgreSQL Cluster this role belongs to
	// +kubebuilder:validation:Required
	ClusterRef ClusterReference `json:"clusterRef"`

	// roleName is the name of the role in PostgreSQL. It defaults to
	// metadata.name when unset, and lets the PostgreSQL name use characters
	// (such as underscores) that Kubernetes object names do not allow.
	// It must be a lowercase unquoted identifier, must not start with "pg_"
	// (reserved by PostgreSQL) or "pgop_" (reserved for the operator's own
	// roles), must not be "postgres", and cannot be changed after creation.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('pg_')",message="roleName must not start with 'pg_' (reserved by PostgreSQL)"
	// +kubebuilder:validation:XValidation:rule="self != 'postgres' && !self.startsWith('pgop_')",message="roleName must not be postgres or start with 'pgop_' (reserved for the operator)"
	RoleName string `json:"roleName,omitempty"`

	// login allows the role to log in (connect to the database)
	// +kubebuilder:default=true
	Login *bool `json:"login,omitempty"`

	// createDB allows the role to create new databases
	// +optional
	CreateDB bool `json:"createDB,omitempty"`

	// createRole allows the role to create other roles. It is a privileged
	// attribute: the Cluster must list createRole in
	// spec.rolePolicy.allowedAttributes (reason RolePolicyViolation
	// otherwise).
	// +optional
	CreateRole bool `json:"createRole,omitempty"`

	// inherit allows the role to inherit privileges from roles it is a member of
	// +kubebuilder:default=true
	Inherit *bool `json:"inherit,omitempty"`

	// replication allows the role to initiate replication connections. It is
	// a privileged attribute: the Cluster must list replication in
	// spec.rolePolicy.allowedAttributes (reason RolePolicyViolation
	// otherwise).
	// +optional
	Replication bool `json:"replication,omitempty"`

	// bypassRLS allows the role to bypass row-level security policies. It is
	// a privileged attribute: the Cluster must list bypassRLS in
	// spec.rolePolicy.allowedAttributes (reason RolePolicyViolation
	// otherwise).
	// +optional
	BypassRLS bool `json:"bypassRLS,omitempty"`

	// connectionLimit sets the maximum number of concurrent connections for this role.
	// -1 means unlimited.
	// +kubebuilder:default=-1
	// +kubebuilder:validation:Minimum=-1
	ConnectionLimit *int32 `json:"connectionLimit,omitempty"`

	// memberOf lists PostgreSQL roles this role should be a member of, granted
	// with PostgreSQL's default options.
	//
	// Deprecated: use memberships instead. memberOf will be removed in a future
	// API version. Each entry is treated as a memberships entry with only role
	// set, and a role must not appear in both fields.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self.all(r, !(r in ['postgres', 'pg_execute_server_program', 'pg_read_server_files', 'pg_write_server_files']) && !r.startsWith('pgop_'))",message="membership in postgres, pgop_* roles, pg_execute_server_program, pg_read_server_files or pg_write_server_files is not allowed"
	MemberOf []string `json:"memberOf,omitempty"`

	// memberships lists PostgreSQL roles this role should be a member of, with
	// per-grant options. Memberships that pgop granted and that are later
	// removed from the spec are revoked (see revokeRemovedMemberships);
	// memberships granted outside pgop are never revoked.
	//
	// Memberships that would give the role more than the Cluster's
	// spec.rolePolicy allows are refused with reason MembershipNotAllowed
	// (and revoked if pgop granted them before): postgres, pgop_* roles,
	// pg_execute_server_program, pg_read_server_files, pg_write_server_files,
	// predefined pg_* roles not listed in allowedPredefinedRoles, and any
	// role that is (or is, directly or indirectly, a member of) a superuser,
	// a forbidden role, or a role with a privileged attribute the policy does
	// not allow. The other memberships are still applied.
	// +optional
	// +listType=map
	// +listMapKey=role
	// +kubebuilder:validation:MaxItems=256
	Memberships []RoleMembership `json:"memberships,omitempty"`

	// revokeRemovedMemberships controls whether memberships that pgop granted
	// (tracked in status.managedMemberships) are revoked once they are removed
	// from memberOf/memberships. Set it to false to keep the legacy grant-only
	// behavior; memberships removed while it is false are no longer tracked and
	// are never revoked later.
	//
	// This is a transitional opt-out: it will be removed together with the
	// deprecated memberOf field, after which removed memberships are always
	// revoked.
	// +kubebuilder:default=true
	// +optional
	RevokeRemovedMemberships *bool `json:"revokeRemovedMemberships,omitempty"`

	// passwordSecretRef references a Secret in the Role's namespace whose key
	// holds the password for this role. When set, the operator uses that value
	// instead of generating one, copies it into the role credentials Secret
	// and follows changes to the referenced Secret. The referenced Secret is
	// never modified or owned by the operator. When the reference is removed,
	// the current password is kept. Mutually exclusive with passwordRotation.
	// The value must be the plaintext password: valid UTF-8, without NUL
	// bytes, and not a SCRAM-SHA-256 or MD5 hash (reason
	// PasswordSecretInvalid otherwise).
	// Security: the operator reads the referenced Secret with its own
	// permissions, so whoever can create or update Roles in a namespace can
	// read the Secrets in it through the credentials Secret. Secrets managed
	// by pgop (labeled app.kubernetes.io/managed-by=pgop or owned by a pgop
	// resource, such as the Cluster's superuser credentials) and Cluster TLS
	// Secrets are refused (reason RolePolicyViolation).
	// +optional
	PasswordSecretRef *SecretKeySelector `json:"passwordSecretRef,omitempty"`

	// passwordRotation enables scheduled rotation of the operator-generated
	// password. A rotation sets a new password in PostgreSQL and then updates
	// the role credentials Secret (and the credentials Secrets of Databases
	// owned by this role). The old password stops working for new connections
	// immediately; existing sessions stay connected. Mutually exclusive with
	// passwordSecretRef.
	// +optional
	PasswordRotation *PasswordRotationSpec `json:"passwordRotation,omitempty"`

	// parameterGrants grants privileges on configuration parameters to this
	// role (GRANT SET ON PARAMETER), so it can change superuser-only
	// parameters in its sessions. Requires PostgreSQL 15 or later. Grants
	// that pgop made (tracked in status.managedParameterGrants) are revoked
	// once they are removed from the spec. Parameters that switch identity,
	// load code or bypass safeguards (role, session_authorization,
	// *_preload_libraries, dynamic_library_path, jit_provider,
	// session_replication_role, pgaudit.*, set_user.*, anon.*, sepgsql.*)
	// cannot be granted.
	// +optional
	// +listType=map
	// +listMapKey=parameter
	// +kubebuilder:validation:MaxItems=256
	ParameterGrants []ParameterGrantSpec `json:"parameterGrants,omitempty"`

	// settings are per-role defaults for configuration parameters (ALTER ROLE
	// ... SET name TO value). They apply to new sessions of this role only
	// (sessions that log in as it, not SET ROLE). Settings that pgop applied
	// (tracked in status.managedSettings) are reset (ALTER ROLE ... RESET
	// name) once they are removed from the spec. Keys and values follow the
	// rules of Database spec.settings: parameter names, values written as SQL
	// string literals, and for search_path and temp_tablespaces a
	// comma-separated list as in postgresql.conf. Only parameters that any
	// user may set (context "user" in pg_settings) and custom parameters are
	// accepted; superuser-only parameters and the same denylist as Database
	// settings are refused (reason SettingNotAllowed), because pgop runs
	// ALTER ROLE as a superuser. Values are readable by every role on the
	// server (pg_roles.rolconfig): do not put secrets here.
	// +optional
	// +kubebuilder:validation:MaxProperties=256
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 127 && k.matches('^[A-Za-z_][A-Za-z0-9_]*(\\\\.[A-Za-z_][A-Za-z0-9_]*)*$'))",message="settings keys must be parameter names: identifiers ([A-Za-z_][A-Za-z0-9_]*) optionally separated by dots, at most 127 characters"
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(self[k]) <= 4096)",message="settings values must be at most 4096 characters"
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, k.lowerAscii() in ['role', 'session_authorization', 'session_preload_libraries', 'local_preload_libraries', 'shared_preload_libraries', 'dynamic_library_path', 'jit_provider', 'session_replication_role', 'lo_compat_privileges'] || k.lowerAscii().startsWith('pgaudit.') || k.lowerAscii().startsWith('set_user.') || k.lowerAscii().startsWith('anon.') || k.lowerAscii().startsWith('sepgsql.'))",message="settings must not include role, session_authorization, *_preload_libraries, dynamic_library_path, jit_provider, session_replication_role, lo_compat_privileges or pgaudit.*, set_user.*, anon.*, sepgsql.* parameters"
	Settings map[string]string `json:"settings,omitempty"`

	// databaseSettings are per-role defaults that apply only in one database
	// (ALTER ROLE ... IN DATABASE db SET name TO value). They take precedence
	// over settings and over the database's own settings. The same rules as
	// for settings apply, and removed entries pgop applied (tracked in
	// status.managedDatabaseSettings) are reset. A database that does not
	// exist yet is retried: the Role stays Available and the condition
	// message lists the pending databases.
	// +optional
	// +listType=map
	// +listMapKey=database
	// +kubebuilder:validation:MaxItems=32
	DatabaseSettings []RoleDatabaseSettings `json:"databaseSettings,omitempty"`
}

// RoleDatabaseSettings are a role's settings in one database.
type RoleDatabaseSettings struct {
	// database is the PostgreSQL name of the database (a raw PostgreSQL
	// database name, not a Database resource name). The settings only affect
	// this role's sessions in it, so any database of the Cluster may be named.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Database string `json:"database"`

	// settings are the parameter defaults for this role in database, with the
	// same rules as the Role's spec.settings.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 127 && k.matches('^[A-Za-z_][A-Za-z0-9_]*(\\\\.[A-Za-z_][A-Za-z0-9_]*)*$'))",message="settings keys must be parameter names: identifiers ([A-Za-z_][A-Za-z0-9_]*) optionally separated by dots, at most 127 characters"
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(self[k]) <= 4096)",message="settings values must be at most 4096 characters"
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, k.lowerAscii() in ['role', 'session_authorization', 'session_preload_libraries', 'local_preload_libraries', 'shared_preload_libraries', 'dynamic_library_path', 'jit_provider', 'session_replication_role', 'lo_compat_privileges'] || k.lowerAscii().startsWith('pgaudit.') || k.lowerAscii().startsWith('set_user.') || k.lowerAscii().startsWith('anon.') || k.lowerAscii().startsWith('sepgsql.'))",message="settings must not include role, session_authorization, *_preload_libraries, dynamic_library_path, jit_provider, session_replication_role, lo_compat_privileges or pgaudit.*, set_user.*, anon.*, sepgsql.* parameters"
	Settings map[string]string `json:"settings,omitempty"`
}

// ManagedRoleDatabaseSettings records the settings pgop applied for a role in
// one database.
type ManagedRoleDatabaseSettings struct {
	// database is the PostgreSQL database name.
	Database string `json:"database"`

	// settings are the parameter names pgop set (normalized to lowercase).
	// +listType=set
	Settings []string `json:"settings"`
}

// ParameterGrantSpec grants privileges on a configuration parameter.
type ParameterGrantSpec struct {
	// parameter is the configuration parameter name, for example
	// log_statement or a custom parameter such as myapp.tenant_id.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=127
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`
	// +kubebuilder:validation:XValidation:rule="!(self.lowerAscii() in ['role', 'session_authorization', 'session_preload_libraries', 'local_preload_libraries', 'shared_preload_libraries', 'dynamic_library_path', 'jit_provider', 'session_replication_role', 'lo_compat_privileges'] || self.lowerAscii().startsWith('pgaudit.') || self.lowerAscii().startsWith('set_user.') || self.lowerAscii().startsWith('anon.') || self.lowerAscii().startsWith('sepgsql.'))",message="privileges on role, session_authorization, *_preload_libraries, dynamic_library_path, jit_provider, session_replication_role, lo_compat_privileges and pgaudit.*, set_user.*, anon.*, sepgsql.* parameters cannot be granted"
	Parameter string `json:"parameter"`

	// privileges lists the privileges to grant. Only SET is supported (the
	// default): ALTER SYSTEM would let the role rewrite the server
	// configuration and is deliberately not offered.
	// +optional
	// +kubebuilder:default={"SET"}
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1
	// +kubebuilder:validation:items:Enum=SET
	Privileges []string `json:"privileges,omitempty"`

	// withGrantOption allows the role to grant the same privileges to others.
	// Turning it off for a grant pgop made revokes the grant option.
	// +optional
	WithGrantOption bool `json:"withGrantOption,omitempty"`
}

// ManagedParameterGrant records privileges on a configuration parameter that
// pgop granted to the role.
type ManagedParameterGrant struct {
	// parameter is the parameter name, normalized to lowercase.
	Parameter string `json:"parameter"`

	// privileges are the granted privileges.
	// +listType=set
	Privileges []string `json:"privileges"`

	// withGrantOption records whether pgop granted the privileges with the
	// grant option.
	// +optional
	WithGrantOption bool `json:"withGrantOption,omitempty"`
}

// RoleStatus defines the observed state of Role.
type RoleStatus struct {
	// ready indicates if the role has been created in the database
	Ready bool `json:"ready,omitempty"`

	// roleName is the effective PostgreSQL role name that was reconciled.
	// +optional
	RoleName string `json:"roleName,omitempty"`

	// clusterUID is the UID of the Cluster on which roleName was created or
	// adopted. pgop only treats roleName as this Role's own when it matches
	// the UID of the Cluster the Role references; a Cluster deleted and
	// re-created under the same name has a new UID.
	// +optional
	ClusterUID string `json:"clusterUID,omitempty"`

	// secretName is the name of the Secret containing the role's credentials.
	// The secret contains 'username' and 'password' keys.
	SecretName string `json:"secretName,omitempty"`

	// passwordHash is deprecated and no longer written: the operator clears it
	// on the next reconcile. It held a salted SHA-256 fingerprint of the
	// password, which anyone able to read the Role could brute-force offline.
	// The fingerprint now lives in the pgop.ruck.io/password-fingerprint
	// annotation of the credentials Secret, next to the password itself. The
	// field is kept only so existing objects stay valid and will be removed in
	// a future API version.
	// +optional
	PasswordHash string `json:"passwordHash,omitempty"`

	// passwordRotatedAt is when the operator last generated the role's
	// password (initially or by rotation). The rotation schedule is measured
	// from it. Unset while the password comes from passwordSecretRef.
	// +optional
	PasswordRotatedAt *metav1.Time `json:"passwordRotatedAt,omitempty"`

	// passwordRotationRequest is the last value of the
	// pgop.ruck.io/rotate-password annotation that the operator acted on.
	// +optional
	PasswordRotationRequest string `json:"passwordRotationRequest,omitempty"`

	// managedMemberships lists the PostgreSQL roles whose membership pgop has
	// granted to this role. Only these are revoked when they are removed from
	// the spec.
	// +optional
	// +listType=set
	ManagedMemberships []string `json:"managedMemberships,omitempty"`

	// managedParameterGrants lists the parameter privileges pgop has granted
	// to this role. Only these are revoked when they are removed from
	// spec.parameterGrants.
	// +optional
	// +listType=map
	// +listMapKey=parameter
	ManagedParameterGrants []ManagedParameterGrant `json:"managedParameterGrants,omitempty"`

	// managedSettings lists the parameter names pgop has set with ALTER ROLE
	// ... SET (normalized to lowercase). Only these are reset when they are
	// removed from spec.settings.
	// +optional
	// +listType=set
	ManagedSettings []string `json:"managedSettings,omitempty"`

	// managedDatabaseSettings lists, per database, the parameter names pgop
	// has set with ALTER ROLE ... IN DATABASE ... SET. Only these are reset
	// when they are removed from spec.databaseSettings.
	// +optional
	// +listType=map
	// +listMapKey=database
	ManagedDatabaseSettings []ManagedRoleDatabaseSettings `json:"managedDatabaseSettings,omitempty"`

	// conditions represent the current state of the Role resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="has(self.spec.roleName) || self.metadata.name != 'postgres'",message="a Role named postgres must set spec.roleName: the PostgreSQL role postgres is reserved"
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef.name"
// +kubebuilder:printcolumn:name="PGName",type="string",JSONPath=".status.roleName"
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Role is the Schema for the roles API.
// It represents a PostgreSQL role (user) managed by the operator.
type Role struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Role
	// +required
	Spec RoleSpec `json:"spec"`

	// status defines the observed state of Role
	// +optional
	Status RoleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RoleList contains a list of Role
type RoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Role `json:"items"`
}

// IsLogin reports whether the role may log in (CRD default: true).
func (s *RoleSpec) IsLogin() bool { return s.Login == nil || *s.Login }

// IsInherit reports whether the role inherits privileges (CRD default: true).
func (s *RoleSpec) IsInherit() bool { return s.Inherit == nil || *s.Inherit }

// GetConnectionLimit returns the connection limit (CRD default: -1, unlimited).
func (s *RoleSpec) GetConnectionLimit() int32 {
	if s.ConnectionLimit == nil {
		return -1
	}
	return *s.ConnectionLimit
}

// ShouldRevokeRemovedMemberships reports whether memberships removed from the
// spec are revoked (CRD default: true).
func (s *RoleSpec) ShouldRevokeRemovedMemberships() bool {
	return s.RevokeRemovedMemberships == nil || *s.RevokeRemovedMemberships
}

// DesiredMemberships returns the memberships from memberships followed by the
// deprecated memberOf entries (as memberships with default options). Duplicate
// roles keep their first occurrence.
func (s *RoleSpec) DesiredMemberships() []RoleMembership {
	out := make([]RoleMembership, 0, len(s.Memberships)+len(s.MemberOf))
	seen := make(map[string]bool, cap(out))
	for _, m := range s.Memberships {
		if !seen[m.Role] {
			seen[m.Role] = true
			out = append(out, m)
		}
	}
	for _, r := range s.MemberOf {
		if !seen[r] {
			seen[r] = true
			out = append(out, RoleMembership{Role: r})
		}
	}
	return out
}

// PostgresName returns the role's name in PostgreSQL: spec.roleName when set,
// otherwise metadata.name.
func (r *Role) PostgresName() string {
	if r.Spec.RoleName != "" {
		return r.Spec.RoleName
	}
	return r.Name
}
