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

// RestorePhase is the current execution phase of the Restore
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type RestorePhase string

const (
	RestorePhasePending   RestorePhase = "Pending"
	RestorePhaseRunning   RestorePhase = "Running"
	RestorePhaseSucceeded RestorePhase = "Succeeded"
	RestorePhaseFailed    RestorePhase = "Failed"
)

// RestoreSpec defines the desired state of Restore.
type RestoreSpec struct {
	// type selects the restore strategy: logical (pg_restore from a pg_dump
	// artifact in object storage) or physical (pgBackRest restore).
	// +kubebuilder:validation:Required
	Type BackupType `json:"type"`

	// backupRunRef references the BackupRun to restore from. The referenced
	// BackupRun's parent Backup provides the destination/credentials.
	// +kubebuilder:validation:Required
	BackupRunRef ClusterReference `json:"backupRunRef"`

	// clusterRef references the target Cluster to restore into. It may differ
	// from the cluster the backup was taken from.
	// +kubebuilder:validation:Required
	ClusterRef ClusterReference `json:"clusterRef"`

	// databaseRef references the target Database for logical restores.
	// Required when type is logical.
	// +optional
	DatabaseRef *ClusterReference `json:"databaseRef,omitempty"`

	// targetTime performs a point-in-time recovery to the given timestamp
	// (physical restores only). When omitted, the latest available state is
	// restored.
	// +optional
	TargetTime *metav1.Time `json:"targetTime,omitempty"`
}

// RestoreStatus defines the observed state of Restore.
type RestoreStatus struct {
	// phase is the current execution phase.
	// +optional
	Phase RestorePhase `json:"phase,omitempty"`

	// startTime is when the restore job started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// completionTime is when the restore job completed (success or failure).
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// jobName is the name of the Kubernetes Job executing this restore.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// conditions represent the current state of the Restore resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Started",type="date",JSONPath=".status.startTime"
// +kubebuilder:printcolumn:name="Completed",type="date",JSONPath=".status.completionTime"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Restore is the Schema for the restores API.
// Each instance triggers a one-shot restore of a BackupRun into a target Cluster/Database.
type Restore struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec RestoreSpec `json:"spec"`

	// +optional
	Status RestoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RestoreList contains a list of Restore
type RestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Restore `json:"items"`
}
