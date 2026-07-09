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

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

const (
	appNameRestore     = "pgop-restore"
	restoreVolumeName  = "restore"
	restoreVolumeMount = "/restore"
	labelRestoreType   = "pgop.ruck.io/restore-type"
)

// RestoreReconciler reconciles Restore objects
type RestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pgop.ruck.io,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=restores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=restores/finalizers,verbs=update
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backupruns,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backups,verbs=get;list;watch

func (r *RestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	restore := &postgresv1alpha1.Restore{}
	if err := r.Get(ctx, req.NamespacedName, restore); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// A restore is a one-shot operation; the Job it creates is garbage
	// collected via owner references, so no finalizer is needed.

	// Once a Job exists, keep status in sync with it.
	if restore.Status.JobName != "" {
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: restore.Status.JobName, Namespace: restore.Namespace}, job)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err == nil {
			if r.syncFromJob(restore, job) {
				if err := r.Status().Update(ctx, restore); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
		if restore.Status.Phase == postgresv1alpha1.RestorePhaseRunning {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, nil
	}

	// No Job yet: build and create it.
	job, err := r.buildRestoreJob(ctx, restore)
	if err != nil {
		log.Error(err, "Failed to build restore Job")
		return r.markFailed(ctx, restore, err)
	}

	if err := controllerutil.SetControllerReference(restore, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	restore.Status.JobName = job.Name
	restore.Status.Phase = postgresv1alpha1.RestorePhaseRunning
	now := metav1.Now()
	restore.Status.StartTime = &now
	meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             "Running",
		Message:            "Restore job created",
		ObservedGeneration: restore.Generation,
		LastTransitionTime: now,
	})
	if err := r.Status().Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// buildRestoreJob resolves the referenced BackupRun/Backup/Cluster and returns
// the Job that performs the restore.
func (r *RestoreReconciler) buildRestoreJob(ctx context.Context, restore *postgresv1alpha1.Restore) (*batchv1.Job, error) {
	backupRun := &postgresv1alpha1.BackupRun{}
	if err := r.Get(ctx, types.NamespacedName{Name: restore.Spec.BackupRunRef.Name, Namespace: restore.Namespace}, backupRun); err != nil {
		return nil, fmt.Errorf("failed to get backupRun %s: %w", restore.Spec.BackupRunRef.Name, err)
	}

	backup := &postgresv1alpha1.Backup{}
	if err := r.Get(ctx, types.NamespacedName{Name: backupRun.Spec.BackupRef.Name, Namespace: restore.Namespace}, backup); err != nil {
		return nil, fmt.Errorf("failed to get backup %s: %w", backupRun.Spec.BackupRef.Name, err)
	}

	cluster := &postgresv1alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: restore.Spec.ClusterRef.Name, Namespace: restore.Namespace}, cluster); err != nil {
		return nil, fmt.Errorf("failed to get cluster %s: %w", restore.Spec.ClusterRef.Name, err)
	}

	switch restore.Spec.Type {
	case postgresv1alpha1.BackupTypeLogical:
		return r.buildLogicalRestoreJob(ctx, restore, backup, backupRun, cluster)
	case postgresv1alpha1.BackupTypePhysical:
		return r.buildPhysicalRestoreJob(restore, backup)
	default:
		return nil, fmt.Errorf("unsupported restore type %q", restore.Spec.Type)
	}
}

// buildLogicalRestoreJob downloads the pg_dump artifact from object storage and
// runs pg_restore into the target database.
func (r *RestoreReconciler) buildLogicalRestoreJob(
	ctx context.Context,
	restore *postgresv1alpha1.Restore,
	backup *postgresv1alpha1.Backup,
	backupRun *postgresv1alpha1.BackupRun,
	cluster *postgresv1alpha1.Cluster,
) (*batchv1.Job, error) {
	if restore.Spec.DatabaseRef == nil {
		return nil, fmt.Errorf("databaseRef is required for logical restores")
	}
	if backupRun.Status.Location == "" {
		return nil, fmt.Errorf("backupRun %s has no artifact location yet", backupRun.Name)
	}

	database := &postgresv1alpha1.Database{}
	if err := r.Get(ctx, types.NamespacedName{Name: restore.Spec.DatabaseRef.Name, Namespace: restore.Namespace}, database); err != nil {
		return nil, fmt.Errorf("failed to get database %s: %w", restore.Spec.DatabaseRef.Name, err)
	}

	clusterSecretName := cluster.Name + "-credentials"
	pgPort := cluster.Spec.Port
	if pgPort == 0 {
		pgPort = 5432
	}
	pgHost := fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, restore.Namespace)

	downloadScript := fmt.Sprintf(`
set -e
aws s3 cp "%s" /restore/artifact.dump %s
echo "Downloaded %s"
`, backupRun.Status.Location, endpointFlagForDestination(backup.Spec.Destination), backupRun.Status.Location)

	// -Fc dumps are restored with pg_restore; --no-owner avoids failures when
	// the target cluster's role set differs from the source.
	restoreScript := fmt.Sprintf(`
set -e
pg_restore -h $PGHOST -p $PGPORT -U $PGUSER -d %s --no-owner --clean --if-exists /restore/artifact.dump
echo "Restore complete"
`, database.Name)

	envVars := s3EnvVarsForDestination(backup.Spec.Destination)

	job := r.newRestoreJob(restore, string(postgresv1alpha1.BackupTypeLogical))
	job.Spec.Template.Spec.SecurityContext = restorePodSecurityContext(70)
	job.Spec.Template.Spec.Volumes = []corev1.Volume{
		{Name: restoreVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	job.Spec.Template.Spec.InitContainers = []corev1.Container{
		{
			Name:            "s3-download",
			Image:           awsCLIImage,
			SecurityContext: restoreContainerSecurityContext(false),
			Command:         []string{shBin, "-c", downloadScript},
			Env:             envVars,
			VolumeMounts:    []corev1.VolumeMount{{Name: restoreVolumeName, MountPath: restoreVolumeMount}},
		},
	}
	job.Spec.Template.Spec.Containers = []corev1.Container{
		{
			Name:            "pg-restore",
			Image:           cluster.Spec.Image,
			SecurityContext: restoreContainerSecurityContext(true),
			Command:         []string{shBin, "-c", restoreScript},
			Env: []corev1.EnvVar{
				secretEnv("PGUSER", clusterSecretName, "username"),
				secretEnv("PGPASSWORD", clusterSecretName, "password"),
				{Name: "PGHOST", Value: pgHost},
				{Name: "PGPORT", Value: fmt.Sprintf("%d", pgPort)},
			},
			VolumeMounts: []corev1.VolumeMount{{Name: restoreVolumeName, MountPath: restoreVolumeMount}},
		},
	}
	return job, nil
}

// buildPhysicalRestoreJob runs `pgbackrest restore`, optionally to a
// point-in-time target.
//
// NOTE: a physical restore rewrites the PostgreSQL data directory and therefore
// requires the target cluster to be stopped with its data volume mounted
// read-write. This Job issues the pgBackRest restore command against the shared
// repository config; operators must scale the target Cluster down before
// running it. See docs/user-guide/restores.md.
func (r *RestoreReconciler) buildPhysicalRestoreJob(
	restore *postgresv1alpha1.Restore,
	backup *postgresv1alpha1.Backup,
) (*batchv1.Job, error) {
	target := ""
	if restore.Spec.TargetTime != nil {
		target = fmt.Sprintf(` --type=time --target="%s"`, restore.Spec.TargetTime.Format(time.RFC3339))
	}

	restoreScript := fmt.Sprintf(
		"pgbackrest --config=/etc/pgbackrest/pgbackrest.conf --stanza=main restore --delta%s",
		target,
	)

	envVars := s3EnvVarsForDestination(backup.Spec.Destination)
	cmName := fmt.Sprintf("%s-pgbackrest", backup.Name)

	job := r.newRestoreJob(restore, string(postgresv1alpha1.BackupTypePhysical))
	job.Spec.Template.Spec.SecurityContext = restorePodSecurityContext(2000)
	job.Spec.Template.Spec.Containers = []corev1.Container{
		{
			Name:            "pgbackrest",
			Image:           pgbackrestImage,
			SecurityContext: restoreContainerSecurityContext(true),
			Command:         []string{shBin, "-c", restoreScript},
			Env:             envVars,
			VolumeMounts: []corev1.VolumeMount{
				{Name: volPgbackrestConfig, MountPath: "/etc/pgbackrest", ReadOnly: true},
				{Name: volPgbackrestTmp, MountPath: "/tmp"},
			},
		},
	}
	job.Spec.Template.Spec.Volumes = []corev1.Volume{
		{
			Name: volPgbackrestConfig,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
				},
			},
		},
		{Name: volPgbackrestTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	return job, nil
}

// newRestoreJob returns a Job skeleton with the common metadata and pod-level
// scheduling settings shared by logical and physical restores.
func (r *RestoreReconciler) newRestoreJob(restore *postgresv1alpha1.Restore, restoreType string) *batchv1.Job {
	ttlSeconds := int32(3600)
	parallelism := int32(1)
	completions := int32(1)
	backoffLimit := int32(2)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-restore", restore.Name),
			Namespace: restore.Namespace,
			Labels: map[string]string{
				LabelAppName:      appNameRestore,
				LabelAppInstance:  restore.Name,
				LabelAppManagedBy: LabelValuePgop,
				labelRestoreType:  restoreType,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttlSeconds,
			Parallelism:             &parallelism,
			Completions:             &completions,
			BackoffLimit:            &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
				},
			},
		},
	}
}

func (r *RestoreReconciler) syncFromJob(restore *postgresv1alpha1.Restore, job *batchv1.Job) bool {
	changed := false

	if job.Status.StartTime != nil && restore.Status.StartTime == nil {
		restore.Status.StartTime = job.Status.StartTime
		changed = true
	}

	if restore.Status.Phase == postgresv1alpha1.RestorePhaseSucceeded ||
		restore.Status.Phase == postgresv1alpha1.RestorePhaseFailed {
		return changed
	}

	switch {
	case job.Status.Succeeded > 0:
		now := metav1.Now()
		restore.Status.Phase = postgresv1alpha1.RestorePhaseSucceeded
		restore.Status.CompletionTime = &now
		meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             "Succeeded",
			Message:            "Restore completed successfully",
			ObservedGeneration: restore.Generation,
			LastTransitionTime: now,
		})
		changed = true
	case job.Status.Failed > 0:
		msg := "restore job failed"
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed {
				msg = c.Message
				break
			}
		}
		now := metav1.Now()
		restore.Status.Phase = postgresv1alpha1.RestorePhaseFailed
		restore.Status.CompletionTime = &now
		meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeAvailable,
			Status:             metav1.ConditionFalse,
			Reason:             "Failed",
			Message:            msg,
			ObservedGeneration: restore.Generation,
			LastTransitionTime: now,
		})
		changed = true
	case job.Status.Active > 0 && restore.Status.Phase != postgresv1alpha1.RestorePhaseRunning:
		restore.Status.Phase = postgresv1alpha1.RestorePhaseRunning
		changed = true
	}

	return changed
}

func (r *RestoreReconciler) markFailed(ctx context.Context, restore *postgresv1alpha1.Restore, cause error) (ctrl.Result, error) {
	now := metav1.Now()
	restore.Status.Phase = postgresv1alpha1.RestorePhaseFailed
	restore.Status.CompletionTime = &now
	meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonReconcileError,
		Message:            cause.Error(),
		ObservedGeneration: restore.Generation,
		LastTransitionTime: now,
	})
	if err := r.Status().Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// --- shared helpers ---

func secretEnv(name, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
			},
		},
	}
}

func restorePodSecurityContext(uid int64) *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   func() *bool { b := true; return &b }(),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		RunAsUser:      &uid,
		RunAsGroup:     &uid,
		FSGroup:        &uid,
	}
}

func restoreContainerSecurityContext(readOnlyRoot bool) *corev1.SecurityContext {
	sc := &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(bool),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capDropALL}},
	}
	if readOnlyRoot {
		sc.ReadOnlyRootFilesystem = func() *bool { b := true; return &b }()
	}
	return sc
}

// s3EnvVarsForDestination mirrors the backup controller's S3 credential wiring
// for a given DestinationSpec.
func s3EnvVarsForDestination(dest postgresv1alpha1.DestinationSpec) []corev1.EnvVar {
	if dest.Type != postgresv1alpha1.DestinationTypeS3 || dest.S3 == nil {
		return nil
	}
	s3 := dest.S3
	vars := []corev1.EnvVar{{Name: "AWS_DEFAULT_REGION", Value: s3.Region}}
	if s3.CredentialsSecretRef != nil {
		vars = append(vars,
			corev1.EnvVar{
				Name: envAWSAccessKeyID,
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: *s3.CredentialsSecretRef, Key: envAWSAccessKeyID,
				}},
			},
			corev1.EnvVar{
				Name: envAWSSecretAccessKey,
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: *s3.CredentialsSecretRef, Key: envAWSSecretAccessKey,
				}},
			},
		)
	}
	if s3.Endpoint != "" {
		vars = append(vars, corev1.EnvVar{Name: "AWS_ENDPOINT_URL", Value: s3.Endpoint})
	}
	return vars
}

func endpointFlagForDestination(dest postgresv1alpha1.DestinationSpec) string {
	if dest.Type != postgresv1alpha1.DestinationTypeS3 || dest.S3 == nil || dest.S3.Endpoint == "" {
		return ""
	}
	return fmt.Sprintf("--endpoint-url %s", dest.S3.Endpoint)
}

// SetupWithManager sets up the controller with the Manager.
func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&postgresv1alpha1.Restore{}).
		Owns(&batchv1.Job{}).
		Named("restore").
		Complete(r)
}
