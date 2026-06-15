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

// BackupRunType is the kind of backup this run represents
// +kubebuilder:validation:Enum=full;incremental;schema;data
type BackupRunType string

const (
	BackupRunTypeFull        BackupRunType = "full"
	BackupRunTypeIncremental BackupRunType = "incremental"
	BackupRunTypeSchema      BackupRunType = "schema"
	BackupRunTypeData        BackupRunType = "data"
)

// BackupRunPhase is the current execution phase of the BackupRun
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type BackupRunPhase string

const (
	BackupRunPhasePending   BackupRunPhase = "Pending"
	BackupRunPhaseRunning   BackupRunPhase = "Running"
	BackupRunPhaseSucceeded BackupRunPhase = "Succeeded"
	BackupRunPhaseFailed    BackupRunPhase = "Failed"
)

// BackupRunSpec defines the desired state of BackupRun
type BackupRunSpec struct {
	// backupRef references the Backup policy that owns this run
	// +kubebuilder:validation:Required
	BackupRef ClusterReference `json:"backupRef"`

	// type is the kind of backup this run represents
	// +kubebuilder:validation:Required
	Type BackupRunType `json:"type"`

	// ttl is how long to retain this BackupRun record after completion.
	// Overrides the parent Backup's backupRunTTL when set.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

// BackupRunStatus defines the observed state of BackupRun
type BackupRunStatus struct {
	// phase is the current execution phase
	// +optional
	Phase BackupRunPhase `json:"phase,omitempty"`

	// startTime is when the backup job started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// completionTime is when the backup job completed (success or failure)
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// location is the destination path of the completed backup artifact
	// +optional
	Location string `json:"location,omitempty"`

	// sizeBytes is the size of the backup artifact in bytes
	// +optional
	SizeBytes *int64 `json:"sizeBytes,omitempty"`

	// jobName is the name of the Kubernetes Job executing this backup
	// +optional
	JobName string `json:"jobName,omitempty"`

	// conditions represent the current state of the BackupRun resource
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

// BackupRun is the Schema for the backupruns API.
// Each instance represents one execution of a Backup policy and serves as a history record.
type BackupRun struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec BackupRunSpec `json:"spec"`

	// +optional
	Status BackupRunStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupRunList contains a list of BackupRun
type BackupRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BackupRun `json:"items"`
}
