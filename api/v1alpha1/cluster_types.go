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
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RestoreRecord identifies a physical Restore that finished on a Cluster.
type RestoreRecord struct {
	// name of the Restore.
	Name string `json:"name"`
	// uid of the Restore.
	UID string `json:"uid"`
	// fingerprint is a hash of the Restore spec.
	Fingerprint string `json:"fingerprint"`
	// result is Succeeded, Failed or Interrupted.
	Result string `json:"result"`
	// completionTime is when the Restore finished.
	CompletionTime metav1.Time `json:"completionTime"`
}

// WALDropRecord is a WAL segment pgBackRest dropped instead of archiving.
type WALDropRecord struct {
	// time the segment was dropped.
	Time metav1.Time `json:"time"`
	// segment is the WAL file name.
	Segment string `json:"segment"`
	// closedBy is the BackupRun of the first successful backup that started
	// after the drop; empty while no such backup exists.
	// +optional
	ClosedBy string `json:"closedBy,omitempty"`
}

// ClusterSpec defines the desired state of Cluster
type ClusterSpec struct {
	// image is the PostgreSQL container image to use.
	// Must be compatible with the official PostgreSQL image environment variables.
	// +kubebuilder:default="postgres:18"
	Image string `json:"image,omitempty"`

	// postgresMajorVersion is the PostgreSQL major version of the image.
	//
	// The operator uses this to pick the data-directory layout that matches the
	// official image conventions: PG <=17 stores data at /var/lib/postgresql/data,
	// while PG >=18 mounts /var/lib/postgresql and stores data at
	// /var/lib/postgresql/<major>/docker.
	//
	// Normally this is auto-detected from the image tag (e.g. "postgres:18",
	// "postgis/postgis:16-3.4"). Set it explicitly when the tag does not encode a
	// parseable major version (e.g. "latest", a digest-pinned reference, or a
	// custom mirror); otherwise the reconcile fails rather than guess.
	// +optional
	// +kubebuilder:validation:Minimum=1
	PostgresMajorVersion *int32 `json:"postgresMajorVersion,omitempty"`

	// replicas is the total number of PostgreSQL instances to run: one
	// primary plus replicas-1 asynchronous streaming hot standbys.
	//
	// Pod <cluster>-0 is the primary. The other pods clone it with
	// pg_basebackup and stream from it over a dedicated replication slot. The
	// Service "<cluster>" always routes to the primary; with more than one
	// instance the Service "<cluster>-ro" routes to the standbys that stream
	// (read-only). status.ready and the Available condition follow the
	// primary only; standby health is reported by status.readyInstances and
	// the ReplicationHealthy condition.
	// There is no automated failover: when the primary is down, writes are
	// unavailable until it is back.
	//
	// The first scale-up from 1 restarts the primary once to enable
	// replication; later scaling does not restart it. Scaling down removes the
	// highest pods together with their data volumes and replication slots.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	Replicas int32 `json:"replicas,omitempty"`

	// storage defines the persistent storage configuration
	// +optional
	Storage StorageSpec `json:"storage,omitempty"`

	// resources defines the compute resources for the PostgreSQL container
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// port is the port PostgreSQL listens on
	// +kubebuilder:default=5432
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// tls enables TLS on the PostgreSQL server. When unset (the default) the
	// server runs without TLS and the operator connects with sslmode=disable,
	// exactly as before TLS support existed.
	// +optional
	TLS *ClusterTLSSpec `json:"tls,omitempty"`

	// parameters are PostgreSQL server configuration parameters (GUCs), for
	// example shared_buffers, work_mem or shared_preload_libraries. The
	// operator renders them into a configuration file in the ConfigMap
	// "<cluster>-config", which the server loads with
	// "-c config_file=..." and which includes the data directory's own
	// postgresql.conf first, so the image defaults still apply.
	//
	// Changing a parameter only updates the ConfigMap; once it is visible in
	// the pod the operator reloads the server (pg_reload_conf()). Parameters
	// that only take effect on a restart (pg_settings.pending_restart) are
	// listed in status.pendingRestart and the pod is restarted once.
	// Progress is reported by the ParametersApplied condition.
	//
	// Values are passed as strings and always quoted; values with line
	// breaks are reported as InvalidParameter. Keys must be PostgreSQL
	// parameter names. Parameters the operator manages itself
	// (listen_addresses, port, file locations, include directives, the
	// settings controlled by spec.tls, WAL archiving, and the settings
	// streaming replication depends on) are rejected; see
	// ReservedParameters. Note that ALTER SYSTEM (postgresql.auto.conf)
	// still overrides these values; the condition reports it when it does.
	//
	// Leaving parameters empty keeps the image's default configuration and
	// does not change the pod; adding the first parameter (or removing the
	// last one) restarts the pod once.
	// +optional
	// +kubebuilder:validation:MaxProperties=256
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[a-zA-Z_][a-zA-Z0-9_.]*$'))",message="parameter names must match ^[a-zA-Z_][a-zA-Z0-9_.]*$"
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, k.lowerAscii() in ['archive_command', 'archive_library', 'archive_mode', 'config_file', 'data_directory', 'external_pid_file', 'hba_file', 'hot_standby', 'ident_file', 'include', 'include_dir', 'include_if_exists', 'listen_addresses', 'max_replication_slots', 'max_wal_senders', 'port', 'primary_conninfo', 'primary_slot_name', 'restore_command', 'ssl', 'ssl_cert_file', 'ssl_key_file', 'ssl_min_protocol_version', 'unix_socket_directories', 'wal_level'])",message="parameters must not set operator-managed parameters (archive_command, archive_library, archive_mode, config_file, data_directory, external_pid_file, hba_file, hot_standby, ident_file, include, include_dir, include_if_exists, listen_addresses, max_replication_slots, max_wal_senders, port, primary_conninfo, primary_slot_name, restore_command, ssl, ssl_cert_file, ssl_key_file, ssl_min_protocol_version, unix_socket_directories, wal_level)"
	Parameters map[string]string `json:"parameters,omitempty"`

	// rolePolicy limits what Role and Database resources that reference this
	// Cluster may obtain in PostgreSQL. Without it (the default), Roles can
	// only get non-privileged attributes (login, createDB, inherit,
	// connectionLimit), cannot be members of predefined pg_* roles, and
	// Databases can only install extensions that the server marks as trusted.
	// The policy lives on the Cluster on purpose: it can only be widened by
	// someone allowed to edit the Cluster, not by someone who can only create
	// Roles or Databases.
	// +optional
	RolePolicy *RolePolicySpec `json:"rolePolicy,omitempty"`
}

// RoleAttribute is a privileged role attribute that a Cluster can allow
// Roles to request.
// +kubebuilder:validation:Enum=createRole;replication;bypassRLS
type RoleAttribute string

const (
	// RoleAttributeCreateRole is Role.spec.createRole (CREATEROLE).
	RoleAttributeCreateRole RoleAttribute = "createRole"
	// RoleAttributeReplication is Role.spec.replication (REPLICATION).
	RoleAttributeReplication RoleAttribute = "replication"
	// RoleAttributeBypassRLS is Role.spec.bypassRLS (BYPASSRLS).
	RoleAttributeBypassRLS RoleAttribute = "bypassRLS"
)

// RolePolicySpec is the policy for the Roles and Databases of a Cluster.
type RolePolicySpec struct {
	// allowedAttributes lists the privileged attributes Roles may request.
	// A Role that requests an attribute not listed here is reported with
	// reason RolePolicyViolation and gets none of the privileged attributes
	// (an existing role is altered to NOCREATEROLE NOREPLICATION
	// NOBYPASSRLS). Superuser is never allowed. Note that on PostgreSQL 15
	// and older, CREATEROLE lets the role grant itself membership in any
	// non-superuser role, including pg_execute_server_program, so allowing
	// createRole there is close to allowing superuser.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=3
	AllowedAttributes []RoleAttribute `json:"allowedAttributes,omitempty"`

	// allowedPredefinedRoles lists the predefined pg_* roles that Roles may
	// be members of (Role.spec.memberships / memberOf). Memberships in any
	// other pg_* role are refused (reason MembershipNotAllowed).
	// pg_execute_server_program, pg_read_server_files and
	// pg_write_server_files give file system or shell access on the server
	// and can never be allowed. Most predefined roles act on every database
	// of the cluster (for example pg_read_all_data reads every table of
	// every database), so allow them only when all Roles on the Cluster are
	// trusted with that.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:Enum=pg_checkpoint;pg_create_subscription;pg_maintain;pg_monitor;pg_read_all_data;pg_read_all_settings;pg_read_all_stats;pg_signal_autovacuum_worker;pg_signal_backend;pg_stat_scan_tables;pg_use_reserved_connections;pg_write_all_data
	AllowedPredefinedRoles []string `json:"allowedPredefinedRoles,omitempty"`

	// allowedExtensions lists extensions that Databases may install although
	// the server does not mark them as trusted
	// (pg_available_extension_versions.trusted). Trusted extensions are
	// always allowed: a role with CREATE on the database could install them
	// itself. Untrusted extensions (for example file_fdw, dblink, adminpack,
	// plpython3u, postgis) are installed by the operator as a superuser and
	// may give access to the server's files, network or code execution; a
	// Database requesting one that is not listed is reported with reason
	// ExtensionNotAllowed and the extension is not installed.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9_-]+$`
	AllowedExtensions []string `json:"allowedExtensions,omitempty"`
}

// GrantablePredefinedRoles are the predefined roles a Cluster can allow in
// spec.rolePolicy.allowedPredefinedRoles. The enum on that field must list
// exactly these names; a test checks that the generated CRD and this list
// agree.
var GrantablePredefinedRoles = []string{
	"pg_checkpoint",
	"pg_create_subscription",
	"pg_maintain",
	"pg_monitor",
	"pg_read_all_data",
	"pg_read_all_settings",
	"pg_read_all_stats",
	"pg_signal_autovacuum_worker",
	"pg_signal_backend",
	"pg_stat_scan_tables",
	"pg_use_reserved_connections",
	"pg_write_all_data",
}

// ForbiddenPredefinedRoles are predefined roles that give file system or
// shell access on the server. Membership in them is always refused.
var ForbiddenPredefinedRoles = []string{
	"pg_execute_server_program",
	"pg_read_server_files",
	"pg_write_server_files",
}

// AllowsAttribute reports whether the policy allows Roles to request attr.
// A nil policy allows none.
func (p *RolePolicySpec) AllowsAttribute(attr RoleAttribute) bool {
	return p != nil && slices.Contains(p.AllowedAttributes, attr)
}

// AllowsPredefinedRole reports whether the policy allows membership in the
// predefined role name. A nil policy allows none.
func (p *RolePolicySpec) AllowsPredefinedRole(name string) bool {
	return p != nil && slices.Contains(p.AllowedPredefinedRoles, name) && slices.Contains(GrantablePredefinedRoles, name)
}

// AllowsExtension reports whether the policy explicitly allows the untrusted
// extension name. A nil policy allows none.
func (p *RolePolicySpec) AllowsExtension(name string) bool {
	return p != nil && slices.Contains(p.AllowedExtensions, name)
}

// ReservedParameters are the PostgreSQL parameters spec.parameters must not
// set because the operator manages them: the listen address, port and file
// locations; include directives (which would read arbitrary files); the TLS
// settings controlled by spec.tls; WAL archiving / restore_command, which
// are reserved for operator-managed physical backups; and the settings
// streaming replication (spec.replicas > 1) depends on: wal_level keeps its
// default (replica), max_wal_senders and max_replication_slots are set to 32
// by the operator once the Cluster has had standbys, hot_standby stays on so
// standbys serve reads, and primary_conninfo / primary_slot_name are set by
// the operator.
//
// This is the single list to change when relaxing a reservation. The CEL rule
// on ClusterSpec.Parameters must list exactly these names (lower case); a
// test checks that the generated CRD and this list agree.
var ReservedParameters = []string{
	"archive_command",
	"archive_library",
	"archive_mode",
	"config_file",
	"data_directory",
	"external_pid_file",
	"hba_file",
	"hot_standby",
	"ident_file",
	"include",
	"include_dir",
	"include_if_exists",
	"listen_addresses",
	"max_replication_slots",
	"max_wal_senders",
	"port",
	"primary_conninfo",
	"primary_slot_name",
	"restore_command",
	"ssl",
	"ssl_cert_file",
	"ssl_key_file",
	"ssl_min_protocol_version",
	"unix_socket_directories",
	"wal_level",
}

// TLSProtocolVersion is a minimum TLS protocol version accepted by the server.
// +kubebuilder:validation:Enum=TLSv1.2;TLSv1.3
type TLSProtocolVersion string

const (
	// TLSProtocolVersion12 is TLS 1.2.
	TLSProtocolVersion12 TLSProtocolVersion = "TLSv1.2"
	// TLSProtocolVersion13 is TLS 1.3.
	TLSProtocolVersion13 TLSProtocolVersion = "TLSv1.3"
)

// ClusterTLSSpec configures server-side TLS for a Cluster.
//
// The server certificate comes from one of three sources:
//   - secretName: a Secret you provide (for example written by your own
//     cert-manager Certificate);
//   - issuerRef: the operator creates and owns a cert-manager Certificate
//     issued by the referenced issuer;
//   - neither (self-managed): the operator generates a CA and a server
//     certificate, stores them in Secrets owned by the Cluster and renews and
//     rotates them before they expire.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.secretName) && has(self.issuerRef))",message="secretName and issuerRef are mutually exclusive; set at most one (neither selects the self-managed CA)"
type ClusterTLSSpec struct {
	// secretName is the name of a Secret in the Cluster namespace holding the
	// server certificate in the kubernetes.io/tls layout: tls.crt (server
	// certificate, optionally followed by intermediates), tls.key (private key)
	// and ca.crt (the CA that issued tls.crt). cert-manager Certificate
	// resources produce this layout. The certificate must be valid for the
	// Service DNS name <cluster>.<namespace>.svc.cluster.local, which the
	// operator verifies with sslmode=verify-full. Mutually exclusive with
	// issuerRef.
	// +kubebuilder:validation:MinLength=1
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// issuerRef makes the operator create and own a cert-manager Certificate
	// "<cluster>-server" issued by this issuer. cert-manager writes the
	// certificate to the Secret "<cluster>-server-tls", which is then used
	// exactly like secretName. The issuer must populate ca.crt (CA, Vault and
	// self-signed issuers do). Requires cert-manager; without it the Cluster
	// reports TLSReady=False with reason CertManagerUnavailable. Mutually
	// exclusive with secretName.
	// +optional
	IssuerRef *CertManagerIssuerReference `json:"issuerRef,omitempty"`

	// requireTLS rejects non-TLS TCP connections through an operator-managed
	// pg_hba.conf (hostnossl ... reject). Connections over the local Unix
	// socket (probes, the postStart hook) are unaffected. Defaults to true.
	// +kubebuilder:default=true
	// +optional
	RequireTLS *bool `json:"requireTLS,omitempty"`

	// minProtocolVersion is the minimum TLS protocol version the server accepts
	// (PostgreSQL ssl_min_protocol_version).
	// +kubebuilder:default="TLSv1.2"
	// +optional
	MinProtocolVersion TLSProtocolVersion `json:"minProtocolVersion,omitempty"`
}

// CertManagerIssuerReference references a cert-manager issuer.
type CertManagerIssuerReference struct {
	// name of the issuer.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// kind of the issuer: Issuer (namespaced, in the Cluster namespace),
	// ClusterIssuer, or the kind of an external issuer.
	// +kubebuilder:default=Issuer
	// +optional
	Kind string `json:"kind,omitempty"`

	// group of the issuer. Defaults to cert-manager.io; set it for external
	// issuers.
	// +kubebuilder:default="cert-manager.io"
	// +optional
	Group string `json:"group,omitempty"`
}

// IsSelfManaged reports whether the operator manages the CA and server
// certificate itself (neither secretName nor issuerRef is set).
func (t *ClusterTLSSpec) IsSelfManaged() bool { return t.SecretName == "" && t.IssuerRef == nil }

// IsRequireTLS reports whether non-TLS TCP connections are rejected (CRD
// default: true).
func (t *ClusterTLSSpec) IsRequireTLS() bool { return t.RequireTLS == nil || *t.RequireTLS }

// GetMinProtocolVersion returns the minimum TLS protocol version (CRD
// default: TLSv1.2).
func (t *ClusterTLSSpec) GetMinProtocolVersion() TLSProtocolVersion {
	if t.MinProtocolVersion == "" {
		return TLSProtocolVersion12
	}
	return t.MinProtocolVersion
}

// ClusterStatus defines the observed state of Cluster.
type ClusterStatus struct {
	// lastRestore records the last physical Restore that finished on this
	// Cluster (succeeded, failed or interrupted). A Restore with the same
	// name and spec within 24 hours needs a confirmation that names its UID
	// (see the pgop.ruck.io/allow-restore annotation).
	// +optional
	LastRestore *RestoreRecord `json:"lastRestore,omitempty"`

	// lastWALDrop records the last WAL segment pgBackRest dropped because
	// the archive queue exceeded archive-push-queue-max. Point-in-time
	// recovery across it is impossible; closedBy names the first successful
	// backup that started after it (recovery from that backup on works).
	// +optional
	LastWALDrop *WALDropRecord `json:"lastWALDrop,omitempty"`

	// ready indicates if the cluster is ready to accept connections
	Ready bool `json:"ready,omitempty"`

	// endpoint is the internal service endpoint for connecting to PostgreSQL
	Endpoint string `json:"endpoint,omitempty"`

	// secretName is the name of the Secret containing operator credentials
	SecretName string `json:"secretName,omitempty"`

	// readyInstances is the number of PostgreSQL pods (primary and standbys)
	// that are ready.
	// +optional
	ReadyInstances int32 `json:"readyInstances,omitempty"`

	// currentPrimary is the name of the pod running the primary (read-write)
	// instance. The Service "<cluster>" routes to this pod only.
	// +optional
	CurrentPrimary string `json:"currentPrimary,omitempty"`

	// readOnlyEndpoint is the internal endpoint of the "<cluster>-ro"
	// Service, which routes to the hot standbys. Only set while
	// spec.replicas is greater than 1.
	// +optional
	ReadOnlyEndpoint string `json:"readOnlyEndpoint,omitempty"`

	// tlsSecretHash is a hash of the certificate material (tls.crt and ca.crt)
	// that the running server was last confirmed to present. It changes when
	// the certificate is rotated and is empty while TLS is disabled or not yet
	// active.
	// +optional
	TLSSecretHash string `json:"tlsSecretHash,omitempty"`

	// parametersHash identifies the generated configuration (spec.parameters)
	// the server was last asked to reload. Empty while spec.parameters is
	// empty.
	// +optional
	ParametersHash string `json:"parametersHash,omitempty"`

	// pendingRestart lists the parameters the server reports as changed but
	// not yet in effect until a restart (pg_settings.pending_restart). The
	// operator restarts the pod once when one of them comes from
	// spec.parameters.
	// +optional
	// +listType=atomic
	PendingRestart []string `json:"pendingRestart,omitempty"`

	// conditions represent the current state of the Cluster resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Instances",type="integer",JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Ready Instances",type="integer",JSONPath=".status.readyInstances"
// +kubebuilder:printcolumn:name="Primary",type="string",JSONPath=".status.currentPrimary",priority=1
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Cluster is the Schema for the clusters API.
// It represents a PostgreSQL database cluster managed by the operator.
type Cluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Cluster
	// +required
	Spec ClusterSpec `json:"spec"`

	// status defines the observed state of Cluster
	// +optional
	Status ClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterList contains a list of Cluster
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Cluster `json:"items"`
}
