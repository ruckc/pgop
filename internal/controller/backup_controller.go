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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	backupFinalizer = "pgop.ruck.io/backup-finalizer"

	pgbackrestImage = "pgbackrest/pgbackrest:2.54.2"
	awsCLIImage     = "amazon/aws-cli:2.27.46"

	appNameBackup     = "pgop-backup"
	capDropALL        = "ALL"
	shBin             = "/bin/sh"
	backupVolumeName  = "backup"
	backupVolumeMount = "/backup"
	labelBackupType   = "pgop.ruck.io/backup-type"
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
			controllerutil.RemoveFinalizer(backup, backupFinalizer)
			if err := r.Update(ctx, backup); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(backup, backupFinalizer) {
		controllerutil.AddFinalizer(backup, backupFinalizer)
		if err := r.Update(ctx, backup); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch backup.Spec.Type {
	case postgresv1alpha1.BackupTypeLogical:
		if err := r.reconcileLogicalBackup(ctx, backup); err != nil {
			log.Error(err, "Failed to reconcile logical backup")
			return ctrl.Result{}, err
		}
	case postgresv1alpha1.BackupTypePhysical:
		if err := r.reconcilePhysicalBackup(ctx, backup); err != nil {
			log.Error(err, "Failed to reconcile physical backup")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
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

	existing := &batchv1.CronJob{}
	err := r.Get(ctx, types.NamespacedName{Name: cronName, Namespace: backup.Namespace}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

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
FILENAME=$(date +%%Y%%m%%dT%%H%%M%%S).dump
pg_dump -h $PGHOST -p $PGPORT -U $PGUSER -d %s %s -Fc -f /backup/$FILENAME
echo "dump_file=$FILENAME" > /backup/metadata
`, database.Name, dumpFlag)

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
									Env: []corev1.EnvVar{
										{
											Name: "PGUSER",
											ValueFrom: &corev1.EnvVarSource{
												SecretKeyRef: &corev1.SecretKeySelector{
													LocalObjectReference: corev1.LocalObjectReference{Name: clusterSecretName},
													Key:                  "username",
												},
											},
										},
										{
											Name: "PGPASSWORD",
											ValueFrom: &corev1.EnvVarSource{
												SecretKeyRef: &corev1.SecretKeySelector{
													LocalObjectReference: corev1.LocalObjectReference{Name: clusterSecretName},
													Key:                  "password",
												},
											},
										},
										{
											Name:  "PGHOST",
											Value: fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, backup.Namespace),
										},
										{
											Name:  "PGPORT",
											Value: fmt.Sprintf("%d", pgPort),
										},
									},
									VolumeMounts: []corev1.VolumeMount{
										{Name: backupVolumeName, MountPath: backupVolumeMount},
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

	return r.Create(ctx, cronjob)
}

// reconcilePhysicalBackup injects pgBackRest CronJobs for the cluster.
func (r *BackupReconciler) reconcilePhysicalBackup(ctx context.Context, backup *postgresv1alpha1.Backup) error {
	if backup.Spec.ClusterRef == nil {
		return fmt.Errorf("clusterRef is required for physical backups")
	}

	cluster := &postgresv1alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: backup.Spec.ClusterRef.Name, Namespace: backup.Namespace}, cluster); err != nil {
		return fmt.Errorf("failed to get cluster %s: %w", backup.Spec.ClusterRef.Name, err)
	}

	cfg := backup.Spec.Physical
	if cfg == nil {
		cfg = &postgresv1alpha1.PhysicalBackupConfig{
			FullSchedule:        "0 2 * * 0",
			IncrementalSchedule: "0 2 * * 1-6",
		}
	}

	pgbackrestConf := r.buildPgbackrestConfig(backup)

	if err := r.reconcilePgbackrestConfigMap(ctx, backup, pgbackrestConf); err != nil {
		return err
	}

	type runEntry struct {
		runType  postgresv1alpha1.BackupRunType
		schedule string
	}

	for _, bt := range []runEntry{
		{postgresv1alpha1.BackupRunTypeFull, cfg.FullSchedule},
		{postgresv1alpha1.BackupRunTypeIncremental, cfg.IncrementalSchedule},
	} {
		if err := r.reconcilePhysicalCronJob(ctx, backup, cluster, bt.runType, bt.schedule); err != nil {
			return err
		}
	}

	return nil
}

func (r *BackupReconciler) reconcilePgbackrestConfigMap(ctx context.Context, backup *postgresv1alpha1.Backup, conf string) error {
	cmName := fmt.Sprintf("%s-pgbackrest", backup.Name)
	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: backup.Namespace}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: backup.Namespace,
			Labels: map[string]string{
				LabelAppName:      appNameBackup,
				LabelAppInstance:  backup.Name,
				LabelAppManagedBy: LabelValuePgop,
			},
		},
		Data: map[string]string{
			"pgbackrest.conf": conf,
		},
	}

	if err := controllerutil.SetControllerReference(backup, cm, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, cm)
}

func (r *BackupReconciler) reconcilePhysicalCronJob(
	ctx context.Context,
	backup *postgresv1alpha1.Backup,
	cluster *postgresv1alpha1.Cluster,
	runType postgresv1alpha1.BackupRunType,
	schedule string,
) error {
	cronName := fmt.Sprintf("%s-%s", backup.Name, string(runType))
	existing := &batchv1.CronJob{}
	err := r.Get(ctx, types.NamespacedName{Name: cronName, Namespace: backup.Namespace}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	backupType := "full"
	if runType == postgresv1alpha1.BackupRunTypeIncremental {
		backupType = "incr"
	}

	pgPort := cluster.Spec.Port
	if pgPort == 0 {
		pgPort = 5432
	}
	pgHost := fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, backup.Namespace)

	envVars := r.buildS3EnvVars(backup)
	ttlSeconds := int32(300)
	parallelism := int32(1)
	completions := int32(1)
	backoffLimit := int32(3)
	successHistory := int32(3)
	failureHistory := int32(3)

	backupScript := fmt.Sprintf(
		"pgbackrest --config=/etc/pgbackrest/pgbackrest.conf --stanza=main --pg1-host=%s --pg1-port=%d backup --type=%s",
		pgHost, pgPort, backupType,
	)

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
								RunAsUser:  func() *int64 { i := int64(2000); return &i }(),
								RunAsGroup: func() *int64 { i := int64(2000); return &i }(),
								FSGroup:    func() *int64 { i := int64(2000); return &i }(),
							},
							Containers: []corev1.Container{
								{
									Name:  "pgbackrest",
									Image: pgbackrestImage,
									SecurityContext: &corev1.SecurityContext{
										AllowPrivilegeEscalation: new(bool),
										Capabilities: &corev1.Capabilities{
											Drop: []corev1.Capability{capDropALL},
										},
										ReadOnlyRootFilesystem: func() *bool { b := true; return &b }(),
									},
									Command: []string{shBin, "-c", backupScript},
									Env:     envVars,
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      volPgbackrestConfig,
											MountPath: "/etc/pgbackrest",
											ReadOnly:  true,
										},
										{
											Name:      volPgbackrestTmp,
											MountPath: "/tmp",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: volPgbackrestConfig,
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: fmt.Sprintf("%s-pgbackrest", backup.Name),
											},
										},
									},
								},
								{
									Name: volPgbackrestTmp,
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
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

	return r.Create(ctx, cronjob)
}

func (r *BackupReconciler) buildPgbackrestConfig(backup *postgresv1alpha1.Backup) string {
	dest := backup.Spec.Destination
	s3 := dest.S3
	if s3 == nil {
		return ""
	}

	prefix := s3.Prefix
	if prefix == "" {
		prefix = backup.Name
	}

	conf := fmt.Sprintf("[global]\nrepo1-type=s3\nrepo1-s3-bucket=%s\nrepo1-s3-region=%s\nrepo1-path=/%s\n",
		s3.Bucket, s3.Region, prefix)

	if s3.Endpoint != "" {
		conf += fmt.Sprintf("repo1-s3-endpoint=%s\nrepo1-s3-uri-style=path\n", s3.Endpoint)
	}

	conf += "\n[main]\npg1-path=/var/lib/postgresql/data\n"

	return conf
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
		Named("backup").
		Complete(r)
}
