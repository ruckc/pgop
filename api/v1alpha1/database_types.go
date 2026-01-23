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
type DatabaseSpec struct {
	// clusterRef references the PostgreSQL Cluster this database belongs to
	// +kubebuilder:validation:Required
	ClusterRef ClusterReference `json:"clusterRef"`

	// owner is the role that owns this database.
	// If not specified, the operator superuser will be the owner.
	// +optional
	Owner string `json:"owner,omitempty"`

	// extensions lists PostgreSQL extensions to install in this database
	// +optional
	Extensions []ExtensionSpec `json:"extensions,omitempty"`

	// schemas lists schemas to create in this database
	// +optional
	Schemas []SchemaSpec `json:"schemas,omitempty"`
}

// ExtensionSpec defines a PostgreSQL extension to install
type ExtensionSpec struct {
	// name is the name of the PostgreSQL extension
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// schema is the schema to install the extension into.
	// If not specified, the extension is installed into the default schema.
	// +optional
	Schema string `json:"schema,omitempty"`

	// version is the version of the extension to install.
	// If not specified, the latest available version is installed.
	// +optional
	Version string `json:"version,omitempty"`
}

// SchemaSpec defines a schema to create in the database
type SchemaSpec struct {
	// name is the name of the schema
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// owner is the role that owns this schema.
	// If not specified, the database owner will be the schema owner.
	// +optional
	Owner string `json:"owner,omitempty"`

	// grants lists privileges to grant on this schema
	// +optional
	Grants []GrantSpec `json:"grants,omitempty"`
}

// GrantSpec defines privileges to grant to a role
type GrantSpec struct {
	// role is the role to grant privileges to
	// +kubebuilder:validation:Required
	Role string `json:"role"`

	// privileges lists the privileges to grant (e.g., USAGE, CREATE, ALL)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Privileges []string `json:"privileges"`

	// withGrantOption allows the grantee to grant the same privileges to others
	// +optional
	WithGrantOption bool `json:"withGrantOption,omitempty"`
}

// DatabaseStatus defines the observed state of Database.
type DatabaseStatus struct {
	// ready indicates if the database has been created
	Ready bool `json:"ready,omitempty"`

	// installedExtensions lists extensions that have been successfully installed
	// +optional
	InstalledExtensions []string `json:"installedExtensions,omitempty"`

	// createdSchemas lists schemas that have been successfully created
	// +optional
	CreatedSchemas []string `json:"createdSchemas,omitempty"`

	// conditions represent the current state of the Database resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef.name"
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

func init() {
	SchemeBuilder.Register(&Database{}, &DatabaseList{})
}
