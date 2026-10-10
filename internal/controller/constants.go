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

// Common constants used across controllers
const (
	ReasonReconcileError = "ReconcileError"

	LabelAppName      = "app.kubernetes.io/name"
	LabelAppInstance  = "app.kubernetes.io/instance"
	LabelAppManagedBy = "app.kubernetes.io/managed-by"
	LabelValuePgop    = "pgop"

	AppNamePostgresql = "postgresql"
	// AppNamePostgresqlRole is the app.kubernetes.io/name of role credentials
	// Secrets.
	AppNamePostgresqlRole = "postgresql-role"
	// LabelCluster names the Cluster a role credentials Secret belongs to.
	LabelCluster = "pgop.ruck.io/cluster"

	SecretKeyUsername = "username"
	SecretKeyPassword = "password"
	SecretKeyHost     = "host"
	SecretKeyPort     = "port"
	SecretKeyDatabase = "database"
	// SecretKeySSLMode is the libpq sslmode clients should use.
	SecretKeySSLMode = "sslmode"
	// SecretKeyCACert holds the CA that issued the server certificate; only
	// present while TLS is active.
	SecretKeyCACert = "ca.crt"
	// SecretKeyURI is a ready-made postgresql:// connection URI.
	SecretKeyURI = "uri"

	// Keys of a kubernetes.io/tls Secret holding the server certificate.
	TLSSecretKeyCert = "tls.crt"
	TLSSecretKeyKey  = "tls.key"
	TLSSecretKeyCA   = "ca.crt"

	// ConditionTypeTLSReady reports whether the server is serving TLS with the
	// certificate from its TLS Secret. Only set while spec.tls is set.
	ConditionTypeTLSReady = "TLSReady"
	// ReasonTLSActive: the server presents the expected certificate.
	ReasonTLSActive = "TLSActive"
	// ReasonInvalidTLSSecret: the referenced Secret is missing, incomplete,
	// or its certificate is unusable. The StatefulSet is left untouched.
	ReasonInvalidTLSSecret = "InvalidTLSSecret"
	// ReasonWaitingForServer: the pod is not (yet) serving TLS, e.g. while it
	// restarts after TLS was enabled.
	ReasonWaitingForServer = "WaitingForServer"
	// ReasonCertificateReloading: the server still presents a previous
	// certificate; the operator asked it to reload (pg_reload_conf()), or, if
	// the previous certificate cannot be verified with the new CA, restarted
	// the pod.
	ReasonCertificateReloading = "CertificateReloading"
	// ReasonCertManagerUnavailable: spec.tls.issuerRef is set but the
	// cert-manager Certificate API is not installed.
	ReasonCertManagerUnavailable = "CertManagerUnavailable"
	// ReasonCertificatePending: the cert-manager Certificate has not been
	// issued yet (or a Certificate of the same name is not owned by the
	// Cluster).
	ReasonCertificatePending = "CertificatePending"

	// AnnotationTLSRestart is set on the pod template to restart the pod when
	// a new TLS certificate cannot be loaded with a reload (its CA changed).
	// The value is the hash of the certificate material being loaded.
	AnnotationTLSRestart = "pgop.ruck.io/tls-restart"

	// AnnotationParametersRestart is set on the pod template to restart the
	// pod when spec.parameters changed a parameter that only takes effect on
	// a restart. The value is the hash of the configuration being loaded.
	AnnotationParametersRestart = "pgop.ruck.io/parameters-restart"

	// ConditionTypeParametersApplied reports whether spec.parameters is in
	// effect on the server. Only set while spec.parameters is not empty.
	ConditionTypeParametersApplied = "ParametersApplied"
	// ReasonParametersApplied: every parameter is in effect.
	ReasonParametersApplied = "Applied"
	// ReasonWaitingForSync: the server does not see the current generated
	// configuration file yet (the kubelet updates mounted ConfigMaps with a
	// delay, or the pod has not restarted onto the file yet).
	ReasonWaitingForSync = "WaitingForSync"
	// ReasonParametersReloading: the server was asked to reload its
	// configuration and the result is being checked.
	ReasonParametersReloading = "Reloading"
	// ReasonPendingRestart: a parameter only takes effect on a restart; the
	// operator restarts the pod.
	ReasonPendingRestart = "PendingRestart"
	// ReasonInvalidParameter: the server rejects a parameter name or value.
	// Nothing is reloaded or restarted until spec.parameters is fixed.
	ReasonInvalidParameter = "InvalidParameter"
	// ReasonOverriddenByAlterSystem: a parameter is overridden by ALTER SYSTEM
	// (postgresql.auto.conf), which takes precedence over spec.parameters.
	ReasonOverriddenByAlterSystem = "OverriddenByAlterSystem"

	// ReasonUnsupportedServerVersion: the spec uses a feature that the
	// cluster's PostgreSQL version does not support (for example
	// Role.spec.parameterGrants before PostgreSQL 15).
	ReasonUnsupportedServerVersion = "UnsupportedServerVersion"
	// ReasonSettingNotAllowed: Database.spec.settings names a parameter that
	// pgop refuses to set per database (denylisted, or not a user-context
	// parameter). The other settings are still applied.
	ReasonSettingNotAllowed = "SettingNotAllowed"
	// ReasonParameterNotAllowed: Role.spec.parameterGrants names a parameter
	// that pgop refuses to grant privileges on.
	ReasonParameterNotAllowed = "ParameterNotAllowed"
	// ReasonRoleDropBlocked: the role cannot be dropped because objects or
	// privileges that pgop does not manage still depend on it.
	ReasonRoleDropBlocked = "RoleDropBlocked"
	// ReasonRolePolicyViolation: the Role requests something the Cluster's
	// spec.rolePolicy does not allow (a privileged attribute), or names an
	// existing PostgreSQL role pgop must not take over.
	ReasonRolePolicyViolation = "RolePolicyViolation"
	// ReasonMembershipNotAllowed: Role.spec.memberships/memberOf names a role
	// whose membership the policy does not allow. The other memberships are
	// still applied; a forbidden membership pgop granted earlier is revoked.
	ReasonMembershipNotAllowed = "MembershipNotAllowed"
	// ReasonReservedName: the Role or Database maps to a PostgreSQL name
	// reserved for PostgreSQL or the operator. Nothing is done in PostgreSQL.
	ReasonReservedName = "ReservedName"
	// ReasonExtensionNotAllowed: Database.spec.extensions names an extension
	// that is neither trusted nor listed in the Cluster's
	// spec.rolePolicy.allowedExtensions. It is not installed.
	ReasonExtensionNotAllowed = "ExtensionNotAllowed"
	// ReasonExtensionVersionNotAvailable: the server does not have the
	// extension, the requested version of it, or an update path from the
	// installed version to the requested one (the extension's files are not
	// in the image).
	ReasonExtensionVersionNotAvailable = "ExtensionVersionNotAvailable"
	// ReasonExtensionDowngradeNotAllowed: spec.extensions[].version is lower
	// than the installed version. pgop never downgrades an extension.
	ReasonExtensionDowngradeNotAllowed = "ExtensionDowngradeNotAllowed"
	// ReasonExtensionDependencyMissing: the extension requires extensions
	// that are not installed, and cascade is not set (or, for an update,
	// the new version requires them).
	ReasonExtensionDependencyMissing = "ExtensionDependencyMissing"
	// ReasonExtensionSchemaNotAllowed: the schema an extension's install or
	// update script would run in (its target schema, or the schema of an
	// extension it requires) can be written to by a role that could plant
	// objects the superuser-run script would pick up, or is not one the
	// Database manages. Nothing is installed or updated.
	ReasonExtensionSchemaNotAllowed = "ExtensionSchemaNotAllowed"
	// ReasonExtensionSchemaMismatch: the extension is installed in another
	// schema than spec.extensions[].schema names. pgop does not move
	// installed extensions.
	ReasonExtensionSchemaMismatch = "ExtensionSchemaMismatch"
	// ReasonExtensionNotManaged: the extension was created by someone else
	// while pgop was creating it; pgop does not record it as its own.
	ReasonExtensionNotManaged = "ExtensionNotManaged"
	// ReasonExtensionDropBlocked: an extension removed from the spec with
	// dropOnRemoval cannot be dropped without CASCADE (other objects depend
	// on it). pgop retries; it never cascades.
	ReasonExtensionDropBlocked = "ExtensionDropBlocked"
	// ReasonExtensionGrantNotAllowed: spec.extensions[].grants asks for
	// privileges on an extension schema pgop does not grant on through the
	// extension (public, a system schema, or a schema listed in
	// spec.schemas).
	ReasonExtensionGrantNotAllowed = "ExtensionGrantNotAllowed"
	// ReasonSchemaNotAllowed: Database.spec.schemas names a system schema.
	ReasonSchemaNotAllowed = "SchemaNotAllowed"
	// ReasonRoleNotManaged: the PostgreSQL role exists but does not carry this
	// Role's ownership marker (COMMENT ON ROLE); pgop leaves it alone.
	ReasonRoleNotManaged = "RoleNotManaged"
	// ReasonDuplicateRoleName: an older Role of the same Cluster resolves to
	// the same PostgreSQL role name; this Role is not reconciled.
	ReasonDuplicateRoleName = "DuplicateRoleName"
	// ReasonDatabaseNotManaged: the PostgreSQL database exists but does not
	// carry this Database's ownership marker; pgop leaves it alone.
	ReasonDatabaseNotManaged = "DatabaseNotManaged"
	// ReasonDatabaseNotConnectable: the database does not allow connections
	// (ALTER DATABASE ... WITH ALLOW_CONNECTIONS false).
	ReasonDatabaseNotConnectable = "DatabaseNotConnectable"
	// ReasonDuplicateDatabaseName: an older Database of the same Cluster
	// resolves to the same PostgreSQL database name.
	ReasonDuplicateDatabaseName = "DuplicateDatabaseName"
	// ReasonGranteeNotAllowed: Database.spec.grants or schemas[].grants names
	// a grantee the policy does not allow (not managed by a Role of the
	// Cluster nor listed in rolePolicy.allowedExistingRoles, a superuser, or
	// a reserved role). Grants to it are not applied; managed ones are
	// revoked. The other grants are still applied.
	ReasonGranteeNotAllowed = "GranteeNotAllowed"
	// ReasonPublicPrivilegeConflict: Database.spec.publicPrivileges revokes a
	// privilege from PUBLIC that spec.grants or schemas[public].grants grant
	// to PUBLIC. That PUBLIC privilege is left as it is.
	ReasonPublicPrivilegeConflict = "PublicPrivilegeConflict"
	// ReasonPublicPrivilegeStillHeld: Database.spec.publicPrivileges revokes
	// a privilege that PUBLIC still holds from a grantor other than the
	// object's owner (a role with the grant option), which pgop's REVOKE,
	// issued as the owner, does not remove.
	ReasonPublicPrivilegeStillHeld = "PublicPrivilegeStillHeld"
	// ReasonTooManyGrants: the grants declared, together with those pgop
	// still tracks, exceed what the status ledger can hold. Nothing of that
	// kind is granted or revoked until grants are removed from the spec.
	ReasonTooManyGrants = "TooManyGrants"
	// ReasonRevokeSkipped: a privilege pgop granted could not be revoked
	// without CASCADE because the grantee passed it on with a grant option
	// pgop did not give. pgop does not cascade into that; it stops tracking
	// the privilege and reports it once.
	ReasonRevokeSkipped = "RevokeSkipped"
	// ReasonSchemaNotManaged: Database.spec.schemas names an existing schema
	// the Database neither created nor owns (for example one an extension or
	// another role created). pgop does not change its owner or its grants.
	ReasonSchemaNotManaged = "SchemaNotManaged"

	DefaultPostgresImage    = "postgres:18"
	DefaultOperatorUsername = "pgop_operator"

	// defaultDatabaseName is the maintenance database the operator connects to.
	defaultDatabaseName = "postgres"
	// postgresBinary is the server command passed to the image entrypoint.
	postgresBinary = "postgres"

	ConditionTypeAvailable = "Available"

	// ConditionTypeExistingVolume reports whether the Cluster's StatefulSet
	// was created on top of a pre-existing data PVC (e.g. one retained from a
	// previously deleted Cluster of the same name). It is set once, when the
	// StatefulSet is first created, and never recomputed.
	ConditionTypeExistingVolume = "ExistingVolume"
	ReasonPreExistingPVC        = "PreExistingPVC"
	ReasonNewVolume             = "NewVolume"

	// LabelRole is set by the operator on every PostgreSQL pod: primary or
	// replica. The "<cluster>-ro" Service selects role=replica.
	LabelRole        = "pgop.ruck.io/role"
	LabelRolePrimary = "primary"
	LabelRoleReplica = "replica"
	// LabelStatefulSetPodName is set by the StatefulSet controller on each of
	// its pods. The "<cluster>" Service selects the primary pod by it, so the
	// read-write Service keeps routing to the primary even when the primary
	// pod was recreated while the operator was not running.
	LabelStatefulSetPodName = "statefulset.kubernetes.io/pod-name"

	// LabelStreaming is set by the operator on standby pods: "true" while the
	// standby streams from the primary (pg_stat_replication). The
	// "<cluster>-ro" Service only selects streaming standbys.
	LabelStreaming = "pgop.ruck.io/streaming"
	labelValueTrue = "true"

	// AnnotationPasswordSyncRestart on the credentials Secret records that
	// the primary was restarted once because it rejected the operator
	// password (value: fingerprint of that password).
	AnnotationPasswordSyncRestart = "pgop.ruck.io/password-sync-restart"
	// AnnotationAllowPrimaryInit on a Cluster ("true") allows the primary to
	// initialize an empty data directory although the Cluster held data
	// before (see initializedMarkerKey).
	AnnotationAllowPrimaryInit = "pgop.ruck.io/allow-primary-init"

	// ReplicationUsername is the role standbys use for streaming replication.
	ReplicationUsername = "pgop_replicator"
	// SecretKeyReplicationPassword is the credentials Secret key holding the
	// password of ReplicationUsername.
	SecretKeyReplicationPassword = "replication-password"
	// AnnotationReplicationPasswordFingerprint on the credentials Secret is
	// the salted fingerprint of the replication password last set in
	// PostgreSQL.
	AnnotationReplicationPasswordFingerprint = "pgop.ruck.io/replication-password-fingerprint"
	// AnnotationReplicationPasswordRollout on the credentials Secret records
	// (unix time) that the replication password changed and the standbys
	// started before then are being restarted one at a time.
	AnnotationReplicationPasswordRollout = "pgop.ruck.io/replication-password-rollout"

	// ConditionTypeReplicationHealthy reports whether every standby streams
	// from the primary. Only set while spec.replicas is greater than 1.
	ConditionTypeReplicationHealthy = "ReplicationHealthy"
	// ReasonStreaming: every standby streams from the primary.
	ReasonStreaming = "Streaming"
	// ReasonStandbyNotStreaming: at least one standby is not (yet) streaming.
	ReasonStandbyNotStreaming = "StandbyNotStreaming"
	// ReasonWaitingForPrimary: the primary is not ready, so replication
	// cannot be set up or checked.
	ReasonWaitingForPrimary = "WaitingForPrimary"
	// ReasonReplicationError: the operator could not set up or check
	// replication on the primary.
	ReasonReplicationError = "ReplicationError"

	// dataVolumeName is the name of the StatefulSet volumeClaimTemplate; the
	// StatefulSet controller names PVCs "<dataVolumeName>-<sts>-<ordinal>".
	dataVolumeName = "data"
	// defaultStorageSize is the data volume size when spec.storage.size is
	// unset.
	defaultStorageSize = "1Gi"

	envAWSAccessKeyID     = "AWS_ACCESS_KEY_ID"
	envAWSSecretAccessKey = "AWS_SECRET_ACCESS_KEY"
	envPGDatabase         = "PGDATABASE"
	envPGHost             = "PGHOST"
	envPGPort             = "PGPORT"

	volPgbackrestTmp = "pgbackrest-tmp"

	// AnnotationRestoreInProgress on a Cluster names the physical Restore
	// that stopped it (StatefulSet scaled to 0) to restore its data
	// directory. The Cluster starts again once the annotation is removed:
	// by the Restore when it succeeds or is deleted.
	AnnotationRestoreInProgress = "pgop.ruck.io/restore-in-progress"
	// ReasonPausedForRestore: the Cluster is stopped for a physical Restore.
	ReasonPausedForRestore = "PausedForRestore"

	// reasonSucceeded: a BackupRun or Restore completed successfully.
	reasonSucceeded = "Succeeded"
	// reasonInvalid: the spec cannot work as it is.
	reasonInvalid = "Invalid"
	// reasonFailed: a BackupRun or Restore failed.
	reasonFailed = "Failed"

	// ConditionTypeWALArchiving reports WAL archiving health from
	// pg_stat_archiver while the Cluster has a physical Backup.
	ConditionTypeWALArchiving = "WALArchiving"
	// ConditionTypePhysicalBackup reports on the Cluster whether WAL is
	// archived for a physical Backup: True (Enabled), False (Invalid: a
	// physical Backup names the Cluster but cannot be used; Disabled: none,
	// while pgop's image is kept). Absent for Clusters that never had one.
	ConditionTypePhysicalBackup = "PhysicalBackup"
	// ConditionTypeRestoreInterrupted is True while the Cluster is stopped
	// because a physical restore did not complete (see
	// AnnotationRestoreInterrupted).
	ConditionTypeRestoreInterrupted = "RestoreInterrupted"

	// AnnotationRestoreInterrupted on a Cluster names the physical Restore
	// that failed or was deleted after it started writing the data
	// directory. The Cluster stays stopped while it is set: run a new
	// (confirmed) Restore, or remove the annotation to start PostgreSQL on
	// the data directory as it is.
	AnnotationRestoreInterrupted = "pgop.ruck.io/restore-interrupted"
	// AnnotationAllowRestore on a Cluster confirms a physical Restore:
	// "<restore-name>" or "<restore-name>/<restore-uid>". The operator
	// removes it once that Restore finished.
	AnnotationAllowRestore = "pgop.ruck.io/allow-restore"
)
