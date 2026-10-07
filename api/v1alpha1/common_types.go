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

// ClusterReference references a PostgreSQL Cluster resource in the same namespace
type ClusterReference struct {
	// name is the name of the Cluster resource in the same namespace
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// SecretKeySelector selects a key from a Secret
type SecretKeySelector struct {
	// name is the name of the secret
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// key is the key in the secret to select
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// StorageRetainPolicy controls what happens to a Cluster's PersistentVolumeClaim
// when the Cluster is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type StorageRetainPolicy string

const (
	// StorageRetainPolicyRetain keeps the PVC (and its data) after the Cluster
	// is deleted. A Cluster recreated with the same name reuses the volume.
	StorageRetainPolicyRetain StorageRetainPolicy = "Retain"

	// StorageRetainPolicyDelete deletes the PVC (and, depending on the
	// StorageClass reclaim policy, its data) when the Cluster is deleted.
	StorageRetainPolicyDelete StorageRetainPolicy = "Delete"
)

// StorageSpec defines storage configuration for the cluster
type StorageSpec struct {
	// size is the size of the persistent volume claim
	// +kubebuilder:default="1Gi"
	Size string `json:"size,omitempty"`

	// storageClassName is the name of the StorageClass to use
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// retainPolicy controls whether the PersistentVolumeClaim is kept or
	// deleted when the Cluster is deleted. Retain (the default) keeps the data,
	// so a Cluster recreated with the same name starts on the old volume.
	// Delete removes the PVC together with the Cluster. Delete requires
	// Kubernetes 1.27+ (StatefulSet persistentVolumeClaimRetentionPolicy).
	// +kubebuilder:default=Retain
	// +optional
	RetainPolicy StorageRetainPolicy `json:"retainPolicy,omitempty"`
}
