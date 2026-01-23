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

// RoleSpec defines the desired state of Role
type RoleSpec struct {
	// clusterRef references the PostgreSQL Cluster this role belongs to
	// +kubebuilder:validation:Required
	ClusterRef ClusterReference `json:"clusterRef"`

	// login allows the role to log in (connect to the database)
	// +kubebuilder:default=true
	Login bool `json:"login,omitempty"`

	// superuser grants superuser privileges to the role
	// +optional
	Superuser bool `json:"superuser,omitempty"`

	// createDB allows the role to create new databases
	// +optional
	CreateDB bool `json:"createDB,omitempty"`

	// createRole allows the role to create other roles
	// +optional
	CreateRole bool `json:"createRole,omitempty"`

	// inherit allows the role to inherit privileges from roles it is a member of
	// +kubebuilder:default=true
	Inherit bool `json:"inherit,omitempty"`

	// replication allows the role to initiate replication connections
	// +optional
	Replication bool `json:"replication,omitempty"`

	// bypassRLS allows the role to bypass row-level security policies
	// +optional
	BypassRLS bool `json:"bypassRLS,omitempty"`

	// connectionLimit sets the maximum number of concurrent connections for this role.
	// -1 means unlimited.
	// +kubebuilder:default=-1
	// +kubebuilder:validation:Minimum=-1
	ConnectionLimit int32 `json:"connectionLimit,omitempty"`

	// memberOf lists roles this role should be a member of
	// +optional
	MemberOf []string `json:"memberOf,omitempty"`

	// passwordSecretRef references a Secret containing the password for this role.
	// The secret must contain a key with the password value.
	// +optional
	PasswordSecretRef *SecretKeySelector `json:"passwordSecretRef,omitempty"`
}

// RoleStatus defines the observed state of Role.
type RoleStatus struct {
	// ready indicates if the role has been created in the database
	Ready bool `json:"ready,omitempty"`

	// secretName is the name of the Secret containing the role's credentials.
	// The secret contains 'username' and 'password' keys.
	SecretName string `json:"secretName,omitempty"`

	// conditions represent the current state of the Role resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef.name"
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

func init() {
	SchemeBuilder.Register(&Role{}, &RoleList{})
}
