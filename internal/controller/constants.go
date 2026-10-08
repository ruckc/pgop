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

	// dataVolumeName is the name of the StatefulSet volumeClaimTemplate; the
	// StatefulSet controller names PVCs "<dataVolumeName>-<sts>-<ordinal>".
	dataVolumeName = "data"

	envAWSAccessKeyID     = "AWS_ACCESS_KEY_ID"
	envAWSSecretAccessKey = "AWS_SECRET_ACCESS_KEY"
	envPGDatabase         = "PGDATABASE"

	volPgbackrestConfig = "pgbackrest-config"
	volPgbackrestTmp    = "pgbackrest-tmp"
)
