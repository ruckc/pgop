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

// DatabaseSpec defines the desired state of Database
// +kubebuilder:validation:XValidation:rule="!has(self.publicPrivileges) || !has(self.grants) || !self.grants.exists(g, g.role == 'PUBLIC' && ((has(self.publicPrivileges.connect) && !self.publicPrivileges.connect && g.privileges.exists(p, p in ['CONNECT', 'ALL'])) || (has(self.publicPrivileges.temporary) && !self.publicPrivileges.temporary && g.privileges.exists(p, p in ['TEMPORARY', 'TEMP', 'ALL']))))",message="grants must not grant PUBLIC a privilege that publicPrivileges revokes"
// +kubebuilder:validation:XValidation:rule="!has(self.publicPrivileges) || !has(self.schemas) || !self.schemas.exists(s, s.name == 'public' && has(s.grants) && s.grants.exists(g, g.role == 'PUBLIC' && ((has(self.publicPrivileges.publicSchemaUsage) && !self.publicPrivileges.publicSchemaUsage && g.privileges.exists(p, p.lowerAscii() in ['usage', 'all', 'all privileges'])) || (has(self.publicPrivileges.publicSchemaCreate) && !self.publicPrivileges.publicSchemaCreate && g.privileges.exists(p, p.lowerAscii() in ['create', 'all', 'all privileges'])))))",message="schemas[public].grants must not grant PUBLIC a privilege that publicPrivileges revokes"
// +kubebuilder:validation:XValidation:rule="self.clusterRef.name == oldSelf.clusterRef.name",message="clusterRef is immutable; create a new Database for another Cluster"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.databaseName) == has(self.databaseName) && (!has(self.databaseName) || self.databaseName == oldSelf.databaseName)",message="databaseName is immutable"
type DatabaseSpec struct {
	// clusterRef references the PostgreSQL Cluster this database belongs to
	// +kubebuilder:validation:Required
	ClusterRef ClusterReference `json:"clusterRef"`

	// databaseName is the name of the database in PostgreSQL. It defaults to
	// metadata.name when unset, and lets the PostgreSQL name use characters
	// (such as underscores) that Kubernetes object names do not allow.
	// It must be a lowercase unquoted identifier, must not be a reserved
	// database name (postgres, template0, template1), and cannot be changed
	// after creation.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +kubebuilder:validation:XValidation:rule="!(self in ['postgres', 'template0', 'template1'])",message="databaseName must not be a reserved database name (postgres, template0, template1)"
	DatabaseName string `json:"databaseName,omitempty"`

	// owner is the name of the Role resource (in the same namespace) that owns
	// this database. The database is owned by that Role's effective PostgreSQL
	// role name (its spec.roleName, or metadata.name when unset).
	// If not specified, the operator superuser will be the owner.
	// +optional
	Owner string `json:"owner,omitempty"`

	// extensions lists PostgreSQL extensions to install in this database.
	// The operator installs them as a superuser, so only extensions that the
	// server marks as trusted (pg_available_extension_versions.trusted) or
	// that the Cluster lists in spec.rolePolicy.allowedExtensions are
	// installed; others are reported with reason ExtensionNotAllowed.
	// Extensions are never dropped when removed from the list.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Extensions []ExtensionSpec `json:"extensions,omitempty"`

	// schemas lists schemas to create in this database
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	Schemas []SchemaSpec `json:"schemas,omitempty"`

	// grants lists database-level privileges (GRANT ... ON DATABASE) to grant
	// to PostgreSQL roles. Privileges that pgop granted (tracked in
	// status.managedGrants) are revoked once they are removed from the spec;
	// privileges granted outside pgop are never revoked.
	// +optional
	// +listType=map
	// +listMapKey=role
	// +kubebuilder:validation:MaxItems=256
	Grants []DatabaseGrantSpec `json:"grants,omitempty"`

	// settings are per-database defaults for configuration parameters
	// (ALTER DATABASE ... SET name TO value). They apply to new sessions.
	// Settings that pgop applied (tracked in status.managedSettings) are reset
	// (ALTER DATABASE ... RESET name) once they are removed from the spec.
	// Keys are parameter names (for example work_mem or myapp.tenant); values
	// are written as SQL string literals. For the list parameters search_path
	// and temp_tablespaces the value is a comma-separated list as in
	// postgresql.conf (the YAML string "$user", public with the double quotes
	// kept); an empty list is rejected.
	// Only parameters that any user may set (context "user" in pg_settings)
	// and custom parameters are accepted; superuser-only parameters and a
	// denylist of identity-switching, code-loading and safeguard-bypassing
	// parameters are refused (reason SettingNotAllowed), because pgop runs
	// ALTER DATABASE as a superuser.
	// +optional
	// +kubebuilder:validation:MaxProperties=256
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 127 && k.matches('^[A-Za-z_][A-Za-z0-9_]*(\\\\.[A-Za-z_][A-Za-z0-9_]*)*$'))",message="settings keys must be parameter names: identifiers ([A-Za-z_][A-Za-z0-9_]*) optionally separated by dots, at most 127 characters"
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(self[k]) <= 4096)",message="settings values must be at most 4096 characters"
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, k.lowerAscii() in ['role', 'session_authorization', 'session_preload_libraries', 'local_preload_libraries', 'shared_preload_libraries', 'dynamic_library_path', 'jit_provider', 'session_replication_role', 'lo_compat_privileges'] || k.lowerAscii().startsWith('pgaudit.') || k.lowerAscii().startsWith('set_user.') || k.lowerAscii().startsWith('anon.') || k.lowerAscii().startsWith('sepgsql.'))",message="settings must not include role, session_authorization, *_preload_libraries, dynamic_library_path, jit_provider, session_replication_role, lo_compat_privileges or pgaudit.*, set_user.*, anon.*, sepgsql.* parameters"
	Settings map[string]string `json:"settings,omitempty"`

	// publicPrivileges revokes the privileges PostgreSQL gives the PUBLIC
	// pseudo-role (every role) by default: CONNECT and TEMPORARY on the
	// database, and USAGE (and, before PostgreSQL 15, CREATE) on the schema
	// named public. A field set to false revokes that privilege from PUBLIC;
	// unset (or true) leaves PostgreSQL's default alone. pgop records what it
	// revoked in status.revokedPublicPrivileges and grants exactly that back
	// to PUBLIC once the field is unset or set to true again; a privilege
	// PUBLIC did not hold is not recorded and never granted. Revoking CONNECT
	// from PUBLIC locks out every role that has no CONNECT grant of its own
	// (the owner and superusers keep it): grant it in spec.grants.
	// +optional
	PublicPrivileges *PublicPrivilegesSpec `json:"publicPrivileges,omitempty"`
}

// PublicPrivilegesSpec selects default PUBLIC privileges to revoke. false
// revokes; unset or true keeps (or restores) PostgreSQL's default.
type PublicPrivilegesSpec struct {
	// connect: false revokes CONNECT on the database from PUBLIC.
	// +optional
	Connect *bool `json:"connect,omitempty"`

	// temporary: false revokes TEMPORARY on the database from PUBLIC.
	// +optional
	Temporary *bool `json:"temporary,omitempty"`

	// publicSchemaUsage: false revokes USAGE on the schema public from
	// PUBLIC.
	// +optional
	PublicSchemaUsage *bool `json:"publicSchemaUsage,omitempty"`

	// publicSchemaCreate: false revokes CREATE on the schema public from
	// PUBLIC (PostgreSQL 15 and later no longer grant it by default).
	// +optional
	PublicSchemaCreate *bool `json:"publicSchemaCreate,omitempty"`
}

// PublicPrivilege names a default PUBLIC privilege that
// spec.publicPrivileges can revoke (the name of its field there).
// +kubebuilder:validation:Enum=connect;temporary;publicSchemaUsage;publicSchemaCreate
type PublicPrivilege string

// The default PUBLIC privileges spec.publicPrivileges can revoke.
const (
	PublicPrivilegeConnect            PublicPrivilege = "connect"
	PublicPrivilegeTemporary          PublicPrivilege = "temporary"
	PublicPrivilegePublicSchemaUsage  PublicPrivilege = "publicSchemaUsage"
	PublicPrivilegePublicSchemaCreate PublicPrivilege = "publicSchemaCreate"
)

// DatabaseGrantSpec grants database-level privileges to a PostgreSQL role.
// +kubebuilder:validation:XValidation:rule="self.role != 'PUBLIC' || !has(self.withGrantOption) || !self.withGrantOption",message="the grant option cannot be granted to PUBLIC"
type DatabaseGrantSpec struct {
	// role is the grantee: the PostgreSQL name of a role (a raw PostgreSQL
	// role name, not a Role resource name), or PUBLIC (upper case) for every
	// role. A role must be managed by a Role of the same Cluster (created or
	// adopted by it, recorded in its status) or be listed in the Cluster's
	// spec.rolePolicy.allowedExistingRoles; superusers, postgres, pgop_* and
	// predefined pg_* roles are never accepted (reason GranteeNotAllowed). The
	// role must exist; the grant is retried until it does.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == 'PUBLIC' || self.lowerAscii() != 'public'",message="write the PUBLIC pseudo-role in upper case"
	// +kubebuilder:validation:XValidation:rule="self != 'postgres' && !self.startsWith('pgop_') && !self.startsWith('pg_') && self.lowerAscii() != 'none'",message="role must not be postgres, none, a pgop_* role or a predefined pg_* role"
	Role string `json:"role"`

	// privileges lists the database privileges to grant: CONNECT, CREATE,
	// TEMPORARY (or TEMP), or ALL (all three).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=5
	// +kubebuilder:validation:items:Enum=CONNECT;CREATE;TEMPORARY;TEMP;ALL
	Privileges []string `json:"privileges"`

	// withGrantOption allows the grantee to grant the same privileges to
	// others. Turning it off for a grant pgop made revokes the grant option.
	// +optional
	WithGrantOption bool `json:"withGrantOption,omitempty"`
}

// ManagedDatabaseGrant records database privileges that pgop granted to a role.
type ManagedDatabaseGrant struct {
	// role is the PostgreSQL role the privileges were granted to.
	Role string `json:"role"`

	// privileges are the privileges pgop added: those the grantee did not
	// hold before pgop granted them (normalized: TEMPORARY, not TEMP; ALL is
	// expanded). Only these are revoked once they leave the spec.
	// +optional
	// +listType=set
	Privileges []string `json:"privileges,omitempty"`

	// grantOptions are the privileges whose grant option pgop added (the
	// grantee could not grant them on before). Revoking them cascades to
	// what the grantee passed on.
	// +optional
	// +listType=set
	GrantOptions []string `json:"grantOptions,omitempty"`
}

// ExtensionSpec defines a PostgreSQL extension to install
type ExtensionSpec struct {
	// name is the name of the PostgreSQL extension
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	Name string `json:"name"`

	// schema is the schema to install the extension into. If not specified,
	// the extension is installed into the schema named by its control file,
	// or else into public.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Schema string `json:"schema,omitempty"`

	// version is the version of the extension to install.
	// If not specified, the default version is installed.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version,omitempty"`
}

// SchemaSpec defines a schema to create in the database
type SchemaSpec struct {
	// name is the name of the schema. System schemas (pg_catalog,
	// information_schema and other names starting with pg_) are not allowed:
	// owning or creating objects in them would affect every session in the
	// database, including the operator's superuser sessions.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('pg_') && self != 'information_schema'",message="schema name must not be a system schema (pg_* or information_schema)"
	Name string `json:"name"`

	// owner is the role that owns this schema.
	// If not specified, the database owner will be the schema owner.
	// +optional
	Owner string `json:"owner,omitempty"`

	// grants lists privileges to grant on this schema (GRANT ... ON
	// SCHEMA). Privileges that pgop granted (tracked in
	// status.managedSchemaGrants) are revoked once they are removed from the
	// spec, also when the whole schema entry is removed (the schema itself is
	// never dropped); privileges granted outside pgop are never revoked.
	// +optional
	// +listType=map
	// +listMapKey=role
	// +kubebuilder:validation:MaxItems=16
	Grants []GrantSpec `json:"grants,omitempty"`
}

// GrantSpec defines privileges to grant to a role
// +kubebuilder:validation:XValidation:rule="self.role != 'PUBLIC' || !has(self.withGrantOption) || !self.withGrantOption",message="the grant option cannot be granted to PUBLIC"
type GrantSpec struct {
	// role is the grantee: the PostgreSQL name of a role (a raw PostgreSQL
	// role name, not a Role resource name), or PUBLIC (upper case) for every
	// role. A role must be managed by a Role of the same Cluster (created or
	// adopted by it, recorded in its status) or be listed in the Cluster's
	// spec.rolePolicy.allowedExistingRoles; superusers, postgres, pgop_* and
	// predefined pg_* roles are never accepted (reason GranteeNotAllowed). The
	// role must exist; the grant is retried until it does.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == 'PUBLIC' || self.lowerAscii() != 'public'",message="write the PUBLIC pseudo-role in upper case"
	// +kubebuilder:validation:XValidation:rule="self != 'postgres' && !self.startsWith('pgop_') && !self.startsWith('pg_') && self.lowerAscii() != 'none'",message="role must not be postgres, none, a pgop_* role or a predefined pg_* role"
	Role string `json:"role"`

	// privileges lists the schema privileges to grant: USAGE, CREATE or ALL
	// (also written ALL PRIVILEGES), in any letter case.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=32
	// +kubebuilder:validation:items:Pattern=`^(?i:usage|create|all|all privileges)$`
	Privileges []string `json:"privileges"`

	// withGrantOption allows the grantee to grant the same privileges to
	// others. Turning it off for a grant pgop made revokes the grant option.
	// +optional
	WithGrantOption bool `json:"withGrantOption,omitempty"`
}

// ManagedSchemaGrant records schema privileges that pgop granted to a role.
type ManagedSchemaGrant struct {
	// schema is the schema the privileges are on.
	Schema string `json:"schema"`

	// role is the grantee: a PostgreSQL role, or PUBLIC.
	Role string `json:"role"`

	// privileges are the privileges pgop added: those the grantee did not
	// hold before pgop granted them (normalized: ALL is expanded to CREATE
	// and USAGE). Only these are revoked once they leave the spec.
	// +optional
	// +listType=set
	Privileges []string `json:"privileges,omitempty"`

	// grantOptions are the privileges whose grant option pgop added (the
	// grantee could not grant them on before). Revoking them cascades to
	// what the grantee passed on.
	// +optional
	// +listType=set
	GrantOptions []string `json:"grantOptions,omitempty"`
}

// DatabaseStatus defines the observed state of Database.
type DatabaseStatus struct {
	// ready indicates if the database has been created
	Ready bool `json:"ready,omitempty"`

	// databaseName is the effective PostgreSQL database name that was reconciled.
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`

	// clusterUID is the UID of the Cluster on which databaseName was created
	// or adopted. pgop only treats databaseName as this Database's own when
	// it matches the UID of the Cluster the Database references.
	// +optional
	ClusterUID string `json:"clusterUID,omitempty"`

	// installedExtensions lists extensions that have been successfully installed
	// +optional
	InstalledExtensions []string `json:"installedExtensions,omitempty"`

	// createdSchemas lists the schemas this Database manages: those it
	// created, and existing ones owned by a non-superuser that is the
	// schema's declared owner, the database owner or pg_database_owner. Only
	// these are re-owned and granted on; a schema listed here stays managed
	// while it is in spec.schemas.
	// +optional
	CreatedSchemas []string `json:"createdSchemas,omitempty"`

	// managedGrants lists the database privileges pgop has granted. Only
	// these are revoked when they are removed from spec.grants.
	// +optional
	// +listType=map
	// +listMapKey=role
	// +kubebuilder:validation:MaxItems=512
	ManagedGrants []ManagedDatabaseGrant `json:"managedGrants,omitempty"`

	// managedSchemaGrants lists the schema privileges pgop has granted. Only
	// these are revoked when they are removed from spec.schemas[].grants.
	// +optional
	// +listType=map
	// +listMapKey=schema
	// +listMapKey=role
	// +kubebuilder:validation:MaxItems=2048
	ManagedSchemaGrants []ManagedSchemaGrant `json:"managedSchemaGrants,omitempty"`

	// revokedPublicPrivileges lists the default PUBLIC privileges pgop has
	// revoked for spec.publicPrivileges. Only these are granted back to
	// PUBLIC when spec.publicPrivileges no longer revokes them.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=4
	RevokedPublicPrivileges []PublicPrivilege `json:"revokedPublicPrivileges,omitempty"`

	// managedSettings lists the parameter names pgop has set with ALTER
	// DATABASE ... SET (normalized to lowercase). Only these are reset when
	// they are removed from spec.settings.
	// +optional
	// +listType=set
	ManagedSettings []string `json:"managedSettings,omitempty"`

	// conditions represent the current state of the Database resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="has(self.spec.databaseName) || !(self.metadata.name in ['postgres', 'template0', 'template1'])",message="a Database named postgres, template0 or template1 must set spec.databaseName: those PostgreSQL databases are reserved"
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef.name"
// +kubebuilder:printcolumn:name="PGName",type="string",JSONPath=".status.databaseName"
// +kubebuilder:printcolumn:name="Owner",type="string",JSONPath=".spec.owner"
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Database is the Schema for the databases API.
// It represents a PostgreSQL database managed by the operator.
type Database struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Database
	// +required
	Spec DatabaseSpec `json:"spec"`

	// status defines the observed state of Database
	// +optional
	Status DatabaseStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DatabaseList contains a list of Database
type DatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Database `json:"items"`
}

// PostgresName returns the database's name in PostgreSQL: spec.databaseName
// when set, otherwise metadata.name.
func (d *Database) PostgresName() string {
	if d.Spec.DatabaseName != "" {
		return d.Spec.DatabaseName
	}
	return d.Name
}
