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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

	// replicas is the number of PostgreSQL instances to run.
	// Currently only single instance is supported.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1
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
type ClusterTLSSpec struct {
	// secretName is the name of a Secret in the Cluster namespace holding the
	// server certificate in the kubernetes.io/tls layout: tls.crt (server
	// certificate, optionally followed by intermediates), tls.key (private key)
	// and ca.crt (the CA that issued tls.crt). cert-manager Certificate
	// resources produce this layout. The certificate must be valid for the
	// Service DNS name <cluster>.<namespace>.svc.cluster.local, which the
	// operator verifies with sslmode=verify-full.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SecretName string `json:"secretName"`

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
	// ready indicates if the cluster is ready to accept connections
	Ready bool `json:"ready,omitempty"`

	// endpoint is the internal service endpoint for connecting to PostgreSQL
	Endpoint string `json:"endpoint,omitempty"`

	// secretName is the name of the Secret containing operator credentials
	SecretName string `json:"secretName,omitempty"`

	// tlsSecretHash is a hash of the certificate material (tls.crt and ca.crt)
	// that the running server was last confirmed to present. It changes when
	// the certificate is rotated and is empty while TLS is disabled or not yet
	// active.
	// +optional
	TLSSecretHash string `json:"tlsSecretHash,omitempty"`

	// conditions represent the current state of the Cluster resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"
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
