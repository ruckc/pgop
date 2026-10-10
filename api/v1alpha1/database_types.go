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

	// extensions lists PostgreSQL extensions to install in this database,
	// in order. The operator installs them as a superuser, so only extensions
	// that the server marks as trusted (pg_available_extension_versions.trusted)
	// for the requested version or that the Cluster lists in
	// spec.rolePolicy.allowedExtensions are installed; others are reported
	// with reason ExtensionNotAllowed. Extensions are not dropped when
	// removed from the list unless dropOnRemoval is set.
	// +optional
	// +listType=map
	// +listMapKey=name
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

	// withGrantOption is deprecated and no longer written: pgop v0.15 recorded
	// with it that all privileges were granted with the grant option. A
	// ledger that still has it is read as grantOptions = privileges.
	// +optional
	WithGrantOption bool `json:"withGrantOption,omitempty"`
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
	// or else into public. A schema that does not exist and is not listed in
	// spec.schemas is created by pgop, owned by the operator (a superuser),
	// so the extension's install and update scripts run in a schema no other
	// role can write to. The schema is checked before every CREATE EXTENSION
	// and ALTER EXTENSION ... UPDATE (reason ExtensionSchemaNotAllowed): see
	// the Database documentation. pgop never moves an installed extension to
	// another schema.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('pg_') && self != 'information_schema'",message="schema must not be a system schema (pg_* or information_schema)"
	Schema string `json:"schema,omitempty"`

	// version is the version of the extension to install. If not specified,
	// the default version is installed, and an installed extension is left
	// at its version. Changing it on an installed extension updates it
	// (ALTER EXTENSION ... UPDATE TO); a version lower than the installed one
	// is refused (reason ExtensionDowngradeNotAllowed), and so is a version
	// the server has no update path to (ExtensionVersionNotAvailable).
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._+~-]*$`
	Version string `json:"version,omitempty"`

	// cascade installs the extensions this extension requires that are not
	// installed yet (CREATE EXTENSION ... CASCADE). Every one of them must
	// pass the same policy as a listed extension (trusted, or listed in the
	// Cluster's spec.rolePolicy.allowedExtensions); pgop resolves the whole
	// dependency list first and installs nothing when one is refused (reason
	// ExtensionNotAllowed). Without cascade, an extension whose dependencies
	// are not installed is reported (ExtensionDependencyMissing): list the
	// dependencies before it, or set cascade.
	// +optional
	Cascade bool `json:"cascade,omitempty"`

	// dropOnRemoval drops the extension (DROP EXTENSION, without CASCADE)
	// once its entry is removed from spec.extensions. It only applies to an
	// extension pgop created for this Database (status.extensions[].created)
	// and only once pgop has recorded the setting in
	// status.extensions[].dropOnRemoval. Dependencies installed through
	// cascade are never dropped. A drop PostgreSQL refuses because other
	// objects depend on the extension is reported (ExtensionDropBlocked) and
	// retried. Default false: removing an extension from the list leaves it
	// installed, because dropping it deletes the data stored in its types.
	// +optional
	DropOnRemoval bool `json:"dropOnRemoval,omitempty"`

	// grants grants privileges on the extension's own objects (those that
	// belong to the extension, pg_depend deptype 'e') and on its schema to
	// PostgreSQL roles, for example EXECUTE on pg_partman's functions. They
	// are applied while the extension is installed and allowed, re-applied
	// to objects an update adds, and revoked once they are removed from the
	// spec (tracked in status.managedExtensionGrants). Grantees follow the
	// same policy as spec.grants. EXECUTE is only granted on functions and
	// procedures written in SQL or PL/pgSQL that are not SECURITY DEFINER,
	// and table privileges only on plain and partitioned tables: the other
	// objects run with the privileges of their owner (a superuser) or rely
	// on not being executable by others, and are skipped
	// (status.extensions[].skippedObjects).
	// +optional
	// +listType=map
	// +listMapKey=role
	// +kubebuilder:validation:MaxItems=16
	Grants []ExtensionGrantSpec `json:"grants,omitempty"`
}

// ExtensionGrantSpec grants privileges on an extension's objects to a role.
// +kubebuilder:validation:XValidation:rule="has(self.schema) || has(self.tables) || has(self.sequences) || has(self.functions)",message="an extension grant must list schema, tables, sequences or functions privileges"
type ExtensionGrantSpec struct {
	// role is the grantee: the PostgreSQL name of a role (a raw PostgreSQL
	// role name, not a Role resource name), or PUBLIC (upper case) for every
	// role. It follows the same grantee policy as spec.grants (reason
	// GranteeNotAllowed).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == 'PUBLIC' || self.lowerAscii() != 'public'",message="write the PUBLIC pseudo-role in upper case"
	// +kubebuilder:validation:XValidation:rule="self != 'postgres' && !self.startsWith('pgop_') && !self.startsWith('pg_') && self.lowerAscii() != 'none'",message="role must not be postgres, none, a pgop_* role or a predefined pg_* role"
	Role string `json:"role"`

	// schema lists privileges on the extension's schema: USAGE, CREATE or
	// ALL. Only applied when the extension has a schema of its own: not
	// public, not a system schema and not a schema listed in spec.schemas
	// (grant on those in spec.schemas[].grants; reason
	// ExtensionGrantNotAllowed). Granting CREATE lets the grantee write to
	// the schema the extension's scripts run in, so later updates of the
	// extension are refused (ExtensionSchemaNotAllowed).
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=3
	// +kubebuilder:validation:items:Enum=USAGE;CREATE;ALL
	Schema []string `json:"schema,omitempty"`

	// tables lists privileges on the extension's plain and partitioned
	// tables: SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, MAINTAIN
	// (PostgreSQL 17 and later) or ALL (all of these but MAINTAIN). TRIGGER
	// is not offered: a trigger on a table the extension's superuser-run code
	// writes to would run as that superuser.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Enum=SELECT;INSERT;UPDATE;DELETE;TRUNCATE;REFERENCES;MAINTAIN;ALL
	Tables []string `json:"tables,omitempty"`

	// sequences lists privileges on the extension's sequences: USAGE,
	// SELECT, UPDATE or ALL.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +kubebuilder:validation:items:Enum=USAGE;SELECT;UPDATE;ALL
	Sequences []string `json:"sequences,omitempty"`

	// functions lists privileges on the extension's functions and
	// procedures: EXECUTE (or ALL, the same). See grants for the functions
	// that are skipped.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:items:Enum=EXECUTE;ALL
	Functions []string `json:"functions,omitempty"`
}

// ExtensionObjectKind is the kind of extension object an extension grant
// is on.
// +kubebuilder:validation:Enum=schema;tables;sequences;functions
type ExtensionObjectKind string

// Extension object kinds.
const (
	ExtensionObjectSchema    ExtensionObjectKind = "schema"
	ExtensionObjectTables    ExtensionObjectKind = "tables"
	ExtensionObjectSequences ExtensionObjectKind = "sequences"
	ExtensionObjectFunctions ExtensionObjectKind = "functions"
)

// ManagedExtensionGrant records privileges pgop granted on an extension's
// objects of one kind to a role.
type ManagedExtensionGrant struct {
	// extension is the extension whose objects the privileges are on.
	Extension string `json:"extension"`

	// role is the grantee: a PostgreSQL role, or PUBLIC.
	Role string `json:"role"`

	// kind is the kind of object: schema (the extension's schema), tables,
	// sequences or functions (the extension's member objects of that kind).
	Kind ExtensionObjectKind `json:"kind"`

	// schema is the extension's schema, for kind schema.
	// +optional
	Schema string `json:"schema,omitempty"`

	// privileges are the privileges pgop added on at least one object of the
	// kind (normalized, ALL expanded). Once they leave the spec they are
	// revoked from the grantee on every object of the kind that belongs to
	// the extension.
	// +optional
	// +listType=set
	Privileges []string `json:"privileges,omitempty"`
}

// ExtensionStatus reports one extension of the Database.
type ExtensionStatus struct {
	// name is the extension.
	Name string `json:"name"`

	// version is the installed version (pg_extension.extversion), empty when
	// the extension is not installed.
	// +optional
	Version string `json:"version,omitempty"`

	// schema is the schema the extension is installed in.
	// +optional
	Schema string `json:"schema,omitempty"`

	// created reports that pgop created the extension for this Database
	// (recorded before CREATE EXTENSION runs). Only such an extension is
	// dropped by dropOnRemoval.
	// +optional
	Created bool `json:"created,omitempty"`

	// dropOnRemoval is the extension's dropOnRemoval setting as last
	// reconciled; it decides what happens once the entry leaves the spec.
	// +optional
	DropOnRemoval bool `json:"dropOnRemoval,omitempty"`

	// reason is why the extension is not installed, not at the requested
	// version or not dropped (a condition reason such as
	// ExtensionNotAllowed), empty when it is as requested.
	// +optional
	Reason string `json:"reason,omitempty"`

	// message explains reason.
	// +optional
	Message string `json:"message,omitempty"`

	// skippedObjects counts the extension's objects that grants asked for
	// but that pgop does not grant on (SECURITY DEFINER functions, functions
	// in languages other than SQL and PL/pgSQL, views and other relations
	// that are not plain or partitioned tables).
	// +optional
	SkippedObjects int32 `json:"skippedObjects,omitempty"`
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

	// withGrantOption is deprecated and no longer written: pgop v0.15 recorded
	// with it that all privileges were granted with the grant option. A
	// ledger that still has it is read as grantOptions = privileges.
	// +optional
	WithGrantOption bool `json:"withGrantOption,omitempty"`
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

	// installedExtensions lists the extensions of spec.extensions that are
	// installed.
	// +optional
	InstalledExtensions []string `json:"installedExtensions,omitempty"`

	// extensions reports each extension of spec.extensions (installed
	// version, schema, why it is not as requested), and removed extensions
	// pgop still has to drop (dropOnRemoval).
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=128
	Extensions []ExtensionStatus `json:"extensions,omitempty"`

	// managedExtensionGrants lists the privileges pgop has granted on
	// extensions' objects. Only these are revoked when they are removed from
	// spec.extensions[].grants.
	// +optional
	// +listType=map
	// +listMapKey=extension
	// +listMapKey=role
	// +listMapKey=kind
	// +kubebuilder:validation:MaxItems=1024
	ManagedExtensionGrants []ManagedExtensionGrant `json:"managedExtensionGrants,omitempty"`

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
