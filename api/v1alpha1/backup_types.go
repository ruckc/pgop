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

// BackupType is the type of backup to perform
// +kubebuilder:validation:Enum=physical;logical
type BackupType string

const (
	BackupTypePhysical BackupType = "physical"
	BackupTypeLogical  BackupType = "logical"
)

// DestinationType is the type of destination storage
// +kubebuilder:validation:Enum=s3;azure;gcs
type DestinationType string

const (
	DestinationTypeS3    DestinationType = "s3"
	DestinationTypeAzure DestinationType = "azure"
	DestinationTypeGCS   DestinationType = "gcs"
)

// PhysicalBackupConfig configures schedules for physical (pgBackRest) backups
type PhysicalBackupConfig struct {
	// fullSchedule is the cron schedule for full backups
	// +kubebuilder:default="0 2 * * 0"
	FullSchedule string `json:"fullSchedule,omitempty"`

	// incrementalSchedule is the cron schedule for incremental backups
	// +kubebuilder:default="0 2 * * 1-6"
	IncrementalSchedule string `json:"incrementalSchedule,omitempty"`
}

// RetentionSpec configures how long backups are retained
type RetentionSpec struct {
	// disabled prevents the operator from deleting old backups.
	// Set to true when using write-only credentials to avoid ransomware exposure.
	// +kubebuilder:default=true
	Disabled bool `json:"disabled,omitempty"`

	// keepLast retains the most recent N backups (requires disabled=false)
	// +optional
	KeepLast *int32 `json:"keepLast,omitempty"`

	// keepDays retains backups newer than N days (requires disabled=false)
	// +optional
	KeepDays *int32 `json:"keepDays,omitempty"`
}

// S3Destination configures an S3 (or S3-compatible) backup destination
type S3Destination struct {
	// bucket is the S3 bucket name
	// +kubebuilder:validation:Required
	Bucket string `json:"bucket"`

	// prefix is the path prefix within the bucket
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// region is the AWS region (or equivalent)
	// +kubebuilder:validation:Required
	Region string `json:"region"`

	// endpoint overrides the default S3 endpoint (for S3-compatible storage)
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// credentialsSecretRef references a Secret with AWS credentials.
	// Keys: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY.
	// If omitted, ambient credentials (IRSA/instance profile) are used.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
}

// AzureDestination configures an Azure Blob Storage backup destination
type AzureDestination struct {
	// container is the Azure Blob container name
	// +kubebuilder:validation:Required
	Container string `json:"container"`

	// storageAccount is the Azure storage account name
	// +kubebuilder:validation:Required
	StorageAccount string `json:"storageAccount"`

	// credentialsSecretRef references a Secret with Azure credentials.
	// Key: AZURE_STORAGE_KEY or AZURE_CLIENT_SECRET etc.
	// If omitted, workload identity is used.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
}

// GCSDestination configures a Google Cloud Storage backup destination
type GCSDestination struct {
	// bucket is the GCS bucket name
	// +kubebuilder:validation:Required
	Bucket string `json:"bucket"`

	// prefix is the path prefix within the bucket
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// credentialsSecretRef references a Secret with a GCS service account key JSON.
	// Key: credentials.json.
	// If omitted, workload identity is used.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
}

// DestinationSpec defines where backups are stored
type DestinationSpec struct {
	// type is the storage provider type
	// +kubebuilder:validation:Required
	Type DestinationType `json:"type"`

	// s3 configures S3 or S3-compatible storage
	// +optional
	S3 *S3Destination `json:"s3,omitempty"`

	// azure configures Azure Blob Storage
	// +optional
	Azure *AzureDestination `json:"azure,omitempty"`

	// gcs configures Google Cloud Storage
	// +optional
	GCS *GCSDestination `json:"gcs,omitempty"`
}

// EncryptionSpec configures client-side encryption for backups
type EncryptionSpec struct {
	// enabled enables client-side AES-256 encryption
	Enabled bool `json:"enabled"`

	// keySecretRef references the secret containing the encryption key
	// +optional
	KeySecretRef *SecretKeySelector `json:"keySecretRef,omitempty"`
}

// BackupSpec defines the desired state of Backup
type BackupSpec struct {
	// type is the backup strategy: physical (pgBackRest) or logical (pg_dump)
	// +kubebuilder:validation:Required
	Type BackupType `json:"type"`

	// clusterRef references the Cluster to back up (required for physical backups)
	// +optional
	ClusterRef *ClusterReference `json:"clusterRef,omitempty"`

	// databaseRef references the Database to back up (required for logical backups)
	// +optional
	DatabaseRef *ClusterReference `json:"databaseRef,omitempty"`

	// physical configures schedules for physical backups
	// +optional
	Physical *PhysicalBackupConfig `json:"physical,omitempty"`

	// schedule is the cron schedule for logical backups (schema + data share one schedule)
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// retention configures backup retention policy
	// +optional
	Retention RetentionSpec `json:"retention,omitempty"`

	// backupRunTTL is how long to keep completed BackupRun records
	// +kubebuilder:default="168h"
	BackupRunTTL string `json:"backupRunTTL,omitempty"`

	// destination configures where backups are stored
	// +kubebuilder:validation:Required
	Destination DestinationSpec `json:"destination"`

	// encryption configures optional client-side encryption
	// +optional
	Encryption *EncryptionSpec `json:"encryption,omitempty"`
}

// BackupStatus defines the observed state of Backup
type BackupStatus struct {
	// lastFullBackupTime is when the last full backup completed
	// +optional
	LastFullBackupTime *metav1.Time `json:"lastFullBackupTime,omitempty"`

	// lastIncrementalBackupTime is when the last incremental backup completed
	// +optional
	LastIncrementalBackupTime *metav1.Time `json:"lastIncrementalBackupTime,omitempty"`

	// conditions represent the current state of the Backup resource
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Backup is the Schema for the backups API.
// It defines a backup policy for a PostgreSQL cluster or database.
type Backup struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec BackupSpec `json:"spec"`

	// +optional
	Status BackupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupList contains a list of Backup
type BackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Backup `json:"items"`
}
