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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

const (
	backupFinalizer = "pgop.ruck.io/backup-finalizer"

	awsCLIImage = "amazon/aws-cli:2.27.46"

	appNameBackup     = "pgop-backup"
	capDropALL        = "ALL"
	shBin             = "/bin/sh"
	backupVolumeName  = "backup"
	backupVolumeMount = "/backup"
	labelBackupType   = "pgop.ruck.io/backup-type"

	// annotationSpecHash records the hash of the operator-generated CronJob
	// spec (see applyCronJob).
	annotationSpecHash = "pgop.ruck.io/spec-hash"
)

// BackupReconciler reconciles Backup objects
type BackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backups/finalizers,verbs=update
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backupruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backupruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	backup := &postgresv1alpha1.Backup{}
	if err := r.Get(ctx, req.NamespacedName, backup); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if backup.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(backup, backupFinalizer) {
			base := backup.DeepCopy()
			controllerutil.RemoveFinalizer(backup, backupFinalizer)
			if err := r.Patch(ctx, backup, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(backup, backupFinalizer) {
		base := backup.DeepCopy()
		controllerutil.AddFinalizer(backup, backupFinalizer)
		if err := r.Patch(ctx, backup, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}

	before := backup.Status.DeepCopy()
	var reconcileErr error
	switch backup.Spec.Type {
	case postgresv1alpha1.BackupTypeLogical:
		if reconcileErr = r.reconcileLogicalBackup(ctx, backup); reconcileErr != nil {
			log.Error(reconcileErr, "Failed to reconcile logical backup")
		}
	case postgresv1alpha1.BackupTypePhysical:
		if reconcileErr = r.reconcilePhysicalBackup(ctx, backup); reconcileErr != nil {
			log.Error(reconcileErr, "Failed to reconcile physical backup")
		}
	}

	cond := metav1.Condition{
		Type:               ConditionTypeAvailable,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: backup.Generation,
		Reason:             "Scheduled",
		Message:            "Backup CronJobs are scheduled",
	}
	if reconcileErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = ReasonReconcileError
		cond.Message = reconcileErr.Error()
	}
	meta.SetStatusCondition(&backup.Status.Conditions, cond)
	if !apiequality.Semantic.DeepEqual(before, &backup.Status) {
		if err := r.Status().Update(ctx, backup); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, reconcileErr
}

// reconcileLogicalBackup creates CronJobs for schema and data pg_dump backups.
func (r *BackupReconciler) reconcileLogicalBackup(ctx context.Context, backup *postgresv1alpha1.Backup) error {
	if backup.Spec.DatabaseRef == nil {
		return fmt.Errorf("databaseRef is required for logical backups")
	}

	schedule := backup.Spec.Schedule
	if schedule == "" {
		schedule = "0 2 * * *"
	}

	database := &postgresv1alpha1.Database{}
	if err := r.Get(ctx, types.NamespacedName{Name: backup.Spec.DatabaseRef.Name, Namespace: backup.Namespace}, database); err != nil {
		return fmt.Errorf("failed to get database %s: %w", backup.Spec.DatabaseRef.Name, err)
	}

	cluster := &postgresv1alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: database.Spec.ClusterRef.Name, Namespace: backup.Namespace}, cluster); err != nil {
		return fmt.Errorf("failed to get cluster %s: %w", database.Spec.ClusterRef.Name, err)
	}

	clusterSecretName := cluster.Name + "-credentials"

	for _, runType := range []postgresv1alpha1.BackupRunType{
		postgresv1alpha1.BackupRunTypeSchema,
		postgresv1alpha1.BackupRunTypeData,
	} {
		if err := r.reconcileLogicalCronJob(ctx, backup, cluster, database, clusterSecretName, schedule, runType); err != nil {
			return err
		}
	}

	return nil
}

func (r *BackupReconciler) reconcileLogicalCronJob(
	ctx context.Context,
	backup *postgresv1alpha1.Backup,
	cluster *postgresv1alpha1.Cluster,
	database *postgresv1alpha1.Database,
	clusterSecretName string,
	schedule string,
	runType postgresv1alpha1.BackupRunType,
) error {
	cronName := fmt.Sprintf("%s-%s", backup.Name, string(runType))

	dumpFlag := "--schema-only"
	if runType == postgresv1alpha1.BackupRunTypeData {
		dumpFlag = "--data-only"
	}

	s3Path := r.buildS3Path(backup, string(runType))
	envVars := r.buildS3EnvVars(backup)

	ttlSeconds := int32(300)
	parallelism := int32(1)
	completions := int32(1)
	backoffLimit := int32(3)
	successHistory := int32(3)
	failureHistory := int32(3)

	pgPort := cluster.Spec.Port
	if pgPort == 0 {
		pgPort = 5432
	}

	dumpScript := fmt.Sprintf(`
set -e
echo "pg_dump: host=$PGHOST sslmode=${PGSSLMODE:-prefer}"
FILENAME=$(date +%%Y%%m%%dT%%H%%M%%S).dump
pg_dump -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" %s -Fc -f /backup/$FILENAME
echo "dump_file=$FILENAME" > /backup/metadata
`, dumpFlag)

	uploadScript := fmt.Sprintf(`
set -e
source /backup/metadata
DEST="%s/${dump_file}"
aws s3 cp /backup/${dump_file} "$DEST" %s
echo "Uploaded to $DEST"
`, s3Path, r.buildEndpointFlag(backup))

	cronjob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cronName,
			Namespace: backup.Namespace,
			Labels: map[string]string{
				LabelAppName:      appNameBackup,
				LabelAppInstance:  backup.Name,
				LabelAppManagedBy: LabelValuePgop,
				labelBackupType:   string(runType),
			},
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   schedule,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: &successHistory,
			FailedJobsHistoryLimit:     &failureHistory,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					TTLSecondsAfterFinished: &ttlSeconds,
					Parallelism:             &parallelism,
					Completions:             &completions,
					BackoffLimit:            &backoffLimit,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyOnFailure,
							SecurityContext: &corev1.PodSecurityContext{
								RunAsNonRoot: func() *bool { b := true; return &b }(),
								SeccompProfile: &corev1.SeccompProfile{
									Type: corev1.SeccompProfileTypeRuntimeDefault,
								},
								RunAsUser:  func() *int64 { i := int64(70); return &i }(),
								RunAsGroup: func() *int64 { i := int64(70); return &i }(),
								FSGroup:    func() *int64 { i := int64(70); return &i }(),
							},
							Volumes: []corev1.Volume{
								{
									Name: backupVolumeName,
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
								jobTLSVolume(clusterSecretName),
							},
							InitContainers: []corev1.Container{
								{
									Name:  "pg-dump",
									Image: cluster.Spec.Image,
									SecurityContext: &corev1.SecurityContext{
										AllowPrivilegeEscalation: new(bool),
										Capabilities: &corev1.Capabilities{
											Drop: []corev1.Capability{capDropALL},
										},
										ReadOnlyRootFilesystem: func() *bool { b := true; return &b }(),
									},
									Command: []string{shBin, "-c", dumpScript},
									Env: append([]corev1.EnvVar{
										secretEnv("PGUSER", clusterSecretName, SecretKeyUsername),
										secretEnv("PGPASSWORD", clusterSecretName, SecretKeyPassword),
										{
											// The FQDN of the Service that routes to
											// the primary only, and the name the server
											// certificate is validated for (verify-full).
											Name:  envPGHost,
											Value: clusterHost(cluster),
										},
										{
											Name:  envPGPort,
											Value: fmt.Sprintf("%d", pgPort),
										},
										{
											// Passed via env rather than interpolated into the
											// script so the name never needs shell quoting.
											Name:  envPGDatabase,
											Value: database.PostgresName(),
										},
									}, jobTLSEnv(clusterSecretName)...),
									VolumeMounts: []corev1.VolumeMount{
										{Name: backupVolumeName, MountPath: backupVolumeMount},
										jobTLSVolumeMount(),
									},
								},
							},
							Containers: []corev1.Container{
								{
									Name:  "s3-upload",
									Image: awsCLIImage,
									SecurityContext: &corev1.SecurityContext{
										AllowPrivilegeEscalation: new(bool),
										Capabilities: &corev1.Capabilities{
											Drop: []corev1.Capability{capDropALL},
										},
									},
									Command: []string{shBin, "-c", uploadScript},
									Env:     envVars,
									VolumeMounts: []corev1.VolumeMount{
										{Name: backupVolumeName, MountPath: backupVolumeMount},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(backup, cronjob, r.Scheme); err != nil {
		return err
	}

	return r.applyCronJob(ctx, cronjob)
}

// applyCronJob creates the CronJob, or converges an existing one onto the
// desired spec. A hash of the desired spec is kept in an annotation, so the
// CronJob is only rewritten when what the operator generates changes (for
// example after an operator upgrade, or a Cluster image/port change), never
// because of API-server defaulting. spec.suspend is left as the user set it.
func (r *BackupReconciler) applyCronJob(ctx context.Context, desired *batchv1.CronJob) error {
	hash, err := cronJobSpecHash(desired)
	if err != nil {
		return err
	}
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	desired.Annotations[annotationSpecHash] = hash

	existing := &batchv1.CronJob{}
	err = r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if existing.Annotations[annotationSpecHash] == hash {
		return nil
	}

	suspend := existing.Spec.Suspend
	existing.Spec = desired.Spec
	existing.Spec.Suspend = suspend
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	maps.Copy(existing.Labels, desired.Labels)
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[annotationSpecHash] = hash
	logf.FromContext(ctx).Info("Updating backup CronJob", "cronjob", existing.Name)
	return r.Update(ctx, existing)
}

// cronJobSpecHash hashes the operator-generated CronJob spec.
func cronJobSpecHash(cj *batchv1.CronJob) (string, error) {
	b, err := json.Marshal(cj.Spec)
	if err != nil {
		return "", fmt.Errorf("failed to hash CronJob spec: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16]), nil
}

func (r *BackupReconciler) buildS3Path(backup *postgresv1alpha1.Backup, suffix string) string {
	dest := backup.Spec.Destination
	if dest.Type != postgresv1alpha1.DestinationTypeS3 || dest.S3 == nil {
		return ""
	}
	s3 := dest.S3
	prefix := s3.Prefix
	if prefix != "" {
		prefix = prefix + "/"
	}
	return fmt.Sprintf("s3://%s/%s%s", s3.Bucket, prefix, suffix)
}

func (r *BackupReconciler) buildEndpointFlag(backup *postgresv1alpha1.Backup) string {
	dest := backup.Spec.Destination
	if dest.Type != postgresv1alpha1.DestinationTypeS3 || dest.S3 == nil {
		return ""
	}
	if dest.S3.Endpoint != "" {
		return fmt.Sprintf("--endpoint-url %s", dest.S3.Endpoint)
	}
	return ""
}

func (r *BackupReconciler) buildS3EnvVars(backup *postgresv1alpha1.Backup) []corev1.EnvVar {
	dest := backup.Spec.Destination
	if dest.Type != postgresv1alpha1.DestinationTypeS3 || dest.S3 == nil {
		return nil
	}
	s3 := dest.S3

	vars := []corev1.EnvVar{
		{Name: "AWS_DEFAULT_REGION", Value: s3.Region},
	}

	if s3.CredentialsSecretRef != nil {
		vars = append(vars,
			corev1.EnvVar{
				Name: envAWSAccessKeyID,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: *s3.CredentialsSecretRef,
						Key:                  envAWSAccessKeyID,
					},
				},
			},
			corev1.EnvVar{
				Name: envAWSSecretAccessKey,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: *s3.CredentialsSecretRef,
						Key:                  envAWSSecretAccessKey,
					},
				},
			},
		)
	}

	if s3.Endpoint != "" {
		vars = append(vars, corev1.EnvVar{
			Name:  "AWS_ENDPOINT_URL",
			Value: s3.Endpoint,
		})
	}

	return vars
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&postgresv1alpha1.Backup{}).
		Owns(&batchv1.CronJob{}).
		Owns(&corev1.ConfigMap{}).
		// Physical backup Jobs (owned by the CronJobs) are recorded as
		// BackupRuns and update the Backup status.
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(backupForJob)).
		// Spec changes (image, port, ...) of a Cluster flow into its backup
		// CronJobs. TLS state needs no watch: Jobs read it from the
		// credentials Secret at run time (see job_tls.go).
		Watches(&postgresv1alpha1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.backupsForCluster),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("backup").
		Complete(r)
}

// backupsForCluster maps a Cluster to the Backups that back it up: physical
// Backups through clusterRef, logical Backups through their Database.
func (r *BackupReconciler) backupsForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	backups := &postgresv1alpha1.BackupList{}
	if err := r.List(ctx, backups, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Backups for Cluster", "cluster", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range backups.Items {
		b := &backups.Items[i]
		if backupCluster(ctx, r, b) == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(b)})
		}
	}
	return requests
}

// backupCluster returns the name of the Cluster a Backup reads from, or ""
// when it cannot be resolved.
func backupCluster(ctx context.Context, c client.Reader, b *postgresv1alpha1.Backup) string {
	switch {
	case b.Spec.Type == postgresv1alpha1.BackupTypePhysical && b.Spec.ClusterRef != nil:
		return b.Spec.ClusterRef.Name
	case b.Spec.Type == postgresv1alpha1.BackupTypeLogical && b.Spec.DatabaseRef != nil:
		db := &postgresv1alpha1.Database{}
		if err := c.Get(ctx, types.NamespacedName{Name: b.Spec.DatabaseRef.Name, Namespace: b.Namespace}, db); err != nil {
			return ""
		}
		return db.Spec.ClusterRef.Name
	}
	return ""
}
