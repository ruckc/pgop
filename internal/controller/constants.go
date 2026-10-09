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
)
