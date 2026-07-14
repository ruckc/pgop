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
}

// ClusterStatus defines the observed state of Cluster.
type ClusterStatus struct {
	// ready indicates if the cluster is ready to accept connections
	Ready bool `json:"ready,omitempty"`

	// endpoint is the internal service endpoint for connecting to PostgreSQL
	Endpoint string `json:"endpoint,omitempty"`

	// secretName is the name of the Secret containing operator credentials
	SecretName string `json:"secretName,omitempty"`

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
