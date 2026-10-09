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
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

const (
	// labelJobName is set by the Job controller on the pods of a Job.
	labelJobName = "batch.kubernetes.io/job-name"

	// terminationLabelPrefix prefixes the backup label in the termination
	// message of a physical backup Job.
	terminationLabelPrefix = "label="
)

// physicalBackupScript runs in the backup Job: it creates the stanza if the
// sidecar has not yet (idempotent), takes the backup ($PGOP_BACKUP_TYPE:
// full or incr; pgBackRest takes a full backup when there is none yet) and
// reports the backup label through the termination message, from where the
// operator records it in the BackupRun.
const physicalBackupScript = `set -eu
echo "pgop: $PGOP_BACKUP_TYPE backup of $PGBACKREST_PG1_HOST to repository $PGBACKREST_REPO1_PATH"
` + pgbackrestBin + ` stanza-create
` + pgbackrestBin + ` backup --type="$PGOP_BACKUP_TYPE"
label="$(` + pgbackrestBin + ` info --output=json | grep -o '"label":"[^"]*"' | tail -n 1 | cut -d '"' -f 4)"
echo "pgop: backup $label complete"
printf '` + terminationLabelPrefix + `%s\n' "$label" > /dev/termination-log
`

// reconcilePhysicalBackup creates the full and incremental pgBackRest
// CronJobs, and records the Jobs they run as BackupRuns. The Cluster side
// (WAL archiving, TLS server, stanza) is set up by the Cluster controller.
func (r *BackupReconciler) reconcilePhysicalBackup(ctx context.Context, backup *postgresv1alpha1.Backup) error {
	if err := validatePhysicalBackup(backup); err != nil {
		return fmt.Errorf("%w: %w", errBackupInvalid, err)
	}
	cluster := &postgresv1alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: backup.Spec.ClusterRef.Name, Namespace: backup.Namespace}, cluster); err != nil {
		return fmt.Errorf("failed to get cluster %s: %w", backup.Spec.ClusterRef.Name, err)
	}
	// Nothing is scheduled (and the Cluster controller leaves the pod alone)
	// while the Cluster cannot archive for this Backup.
	if err := validatePhysicalBackupFor(backup, cluster); err != nil {
		return fmt.Errorf("%w: %w", errBackupInvalid, err)
	}
	if owner, err := physicalBackupFor(ctx, r.Client, cluster); err != nil {
		return err
	} else if owner != nil && owner.Name != backup.Name {
		return fmt.Errorf("%w: cluster %s already archives its WAL for physical Backup %s; only one physical Backup per Cluster is supported",
			errBackupInvalid, cluster.Name, owner.Name)
	}
	mirrorArchivingCondition(backup, cluster)
	layout, err := resolvePostgresLayout(cluster)
	if err != nil {
		return err
	}

	cfg := backup.Spec.Physical
	if cfg == nil {
		cfg = &postgresv1alpha1.PhysicalBackupConfig{}
	}
	schedules := []struct {
		runType  postgresv1alpha1.BackupRunType
		schedule string
	}{
		{postgresv1alpha1.BackupRunTypeFull, defaultString(cfg.FullSchedule, "0 2 * * 0")},
		{postgresv1alpha1.BackupRunTypeIncremental, defaultString(cfg.IncrementalSchedule, "0 2 * * 1-6")},
	}
	for _, s := range schedules {
		cj := r.physicalCronJob(backup, cluster, layout, s.runType, s.schedule)
		if err := controllerutil.SetControllerReference(backup, cj, r.Scheme); err != nil {
			return err
		}
		if err := r.applyCronJob(ctx, cj); err != nil {
			return err
		}
	}
	if err := r.deleteLegacyPgbackrestConfigMap(ctx, backup); err != nil {
		return err
	}
	return r.recordPhysicalRuns(ctx, backup)
}

// errBackupInvalid marks a Backup spec that cannot work as it is; it is
// reported in the Available condition (reason Invalid) and not retried.
var errBackupInvalid = errors.New("invalid Backup")

// mirrorArchivingCondition copies the Cluster's WALArchiving condition
// (pg_stat_archiver, see archive_health.go) to the Backup.
func mirrorArchivingCondition(backup *postgresv1alpha1.Backup, cluster *postgresv1alpha1.Cluster) {
	c := meta.FindStatusCondition(cluster.Status.Conditions, ConditionTypeWALArchiving)
	if c == nil {
		meta.RemoveStatusCondition(&backup.Status.Conditions, ConditionTypeWALArchiving)
		return
	}
	meta.SetStatusCondition(&backup.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeWALArchiving,
		Status:             c.Status,
		ObservedGeneration: backup.Generation,
		Reason:             c.Reason,
		Message:            fmt.Sprintf("Cluster %s: %s", cluster.Name, c.Message),
	})
}

func defaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// physicalJobLabels label the CronJob, its Jobs and their pods.
func physicalJobLabels(backup *postgresv1alpha1.Backup, runType postgresv1alpha1.BackupRunType) map[string]string {
	return map[string]string{
		LabelAppName:      appNameBackup,
		LabelAppInstance:  backup.Name,
		LabelAppManagedBy: LabelValuePgop,
		labelBackupType:   string(runType),
	}
}

// physicalCronJob builds the CronJob of one physical backup type. Its Jobs
// run pgBackRest as the repository host, connecting to the primary's
// pgBackRest TLS server with the client certificate.
func (r *BackupReconciler) physicalCronJob(backup *postgresv1alpha1.Backup, cluster *postgresv1alpha1.Cluster,
	layout postgresLayout, runType postgresv1alpha1.BackupRunType, schedule string) *batchv1.CronJob {
	backupType := "full"
	if runType == postgresv1alpha1.BackupRunTypeIncremental {
		backupType = "incr"
	}
	env := pgbackrestJobEnv(backup, cluster, layout)
	env = append(env, pgbackrestRetentionEnv(backup)...)
	env = append(env, envVar("PGOP_BACKUP_TYPE", backupType))
	volumes, mounts := pgbackrestJobVolumes(backup, cluster)
	labels := physicalJobLabels(backup, runType)

	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", backup.Name, string(runType)),
			Namespace: backup.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   schedule,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: new(int32(3)),
			FailedJobsHistoryLimit:     new(int32(3)),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					TTLSecondsAfterFinished: new(int32(300)),
					Parallelism:             new(int32(1)),
					Completions:             new(int32(1)),
					BackoffLimit:            new(int32(3)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							// Never: a failed attempt's pod (and its log) is
							// kept; the Job retries in a new pod.
							RestartPolicy:   corev1.RestartPolicyNever,
							SecurityContext: pgbackrestPodSecurityContext(),
							Containers: []corev1.Container{{
								Name:            pgbackrestContainerName,
								Image:           pgbackrestImage(backup),
								SecurityContext: restoreContainerSecurityContext(true),
								Command:         []string{shBin, "-c", physicalBackupScript},
								Env:             env,
								VolumeMounts:    mounts,
							}},
							Volumes: volumes,
						},
					},
				},
			},
		},
	}
}

// deleteLegacyPgbackrestConfigMap removes the "<backup>-pgbackrest"
// ConfigMap older operators created; pgBackRest is configured through the
// environment now.
func (r *BackupReconciler) deleteLegacyPgbackrestConfigMap(ctx context.Context, backup *postgresv1alpha1.Backup) error {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: backup.Name + "-pgbackrest", Namespace: backup.Namespace}, cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(cm, backup) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, cm))
}

// recordPhysicalRuns creates a BackupRun (named after the Job) for every
// physical backup Job of the Backup, scheduled or created by hand from the
// CronJob, and records the completion time of the latest full and
// incremental backups in the Backup status. The BackupRun controller follows
// each Job and records the backup label.
func (r *BackupReconciler) recordPhysicalRuns(ctx context.Context, backup *postgresv1alpha1.Backup) error {
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(backup.Namespace), client.MatchingLabels{
		LabelAppName:     appNameBackup,
		LabelAppInstance: backup.Name,
	}); err != nil {
		return err
	}
	var ttl *metav1.Duration
	if d, err := time.ParseDuration(backup.Spec.BackupRunTTL); err == nil && d > 0 {
		ttl = &metav1.Duration{Duration: d}
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		runType := postgresv1alpha1.BackupRunType(job.Labels[labelBackupType])
		if runType != postgresv1alpha1.BackupRunTypeFull && runType != postgresv1alpha1.BackupRunTypeIncremental {
			continue
		}
		if err := r.ensureBackupRun(ctx, backup, job, runType, ttl); err != nil {
			return err
		}
		if job.Status.Succeeded == 0 || job.Status.CompletionTime == nil {
			continue
		}
		last := &backup.Status.LastFullBackupTime
		if runType == postgresv1alpha1.BackupRunTypeIncremental {
			last = &backup.Status.LastIncrementalBackupTime
		}
		if *last == nil || job.Status.CompletionTime.After((*last).Time) {
			*last = job.Status.CompletionTime.DeepCopy()
		}
	}
	return nil
}

// ensureBackupRun creates the BackupRun of a backup Job unless it exists.
func (r *BackupReconciler) ensureBackupRun(ctx context.Context, backup *postgresv1alpha1.Backup, job *batchv1.Job,
	runType postgresv1alpha1.BackupRunType, ttl *metav1.Duration) error {
	run := &postgresv1alpha1.BackupRun{}
	err := r.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, run)
	if err == nil {
		if run.Status.JobName == "" && metav1.IsControlledBy(run, backup) {
			return r.setBackupRunJob(ctx, run, job.Name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if job.Status.CompletionTime != nil && ttl != nil && time.Since(job.Status.CompletionTime.Time) >= ttl.Duration {
		return nil // its record would expire right away
	}
	run = &postgresv1alpha1.BackupRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: job.Namespace,
			Labels: map[string]string{
				LabelAppName:      appNameBackup,
				LabelAppInstance:  backup.Name,
				LabelAppManagedBy: LabelValuePgop,
				labelBackupType:   string(runType),
			},
		},
		Spec: postgresv1alpha1.BackupRunSpec{
			BackupRef: postgresv1alpha1.ClusterReference{Name: backup.Name},
			Type:      runType,
			TTL:       ttl,
		},
	}
	if err := controllerutil.SetControllerReference(backup, run, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, run); err != nil {
		return client.IgnoreAlreadyExists(err)
	}
	logf.FromContext(ctx).Info("Recording physical backup Job", "job", job.Name, "type", runType)
	return r.setBackupRunJob(ctx, run, job.Name)
}

// setBackupRunJob records the Job of a BackupRun with a merge patch, so it
// does not conflict with the BackupRun controller initializing the status.
func (r *BackupReconciler) setBackupRunJob(ctx context.Context, run *postgresv1alpha1.BackupRun, jobName string) error {
	base := run.DeepCopy()
	run.Status.JobName = jobName
	return r.Status().Patch(ctx, run, client.MergeFrom(base))
}

// backupForJob maps a backup Job (created by a Backup's CronJob, or by hand
// from it) to the Backup.
func backupForJob(_ context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	if l[LabelAppName] != appNameBackup || l[LabelAppManagedBy] != LabelValuePgop || l[LabelAppInstance] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: l[LabelAppInstance]}}}
}

// physicalBackupLocation returns the location of a successful physical
// backup Job's backup, from the termination message of its pgbackrest
// container: s3://<bucket><repo path>/backup/<stanza>/<label>. Empty when
// it is not known.
func physicalBackupLocation(backup *postgresv1alpha1.Backup, pods []corev1.Pod) string {
	if backup.Spec.Destination.S3 == nil {
		return ""
	}
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			t := cs.State.Terminated
			if cs.Name != pgbackrestContainerName || t == nil || t.ExitCode != 0 {
				continue
			}
			for line := range strings.Lines(t.Message) {
				label, ok := strings.CutPrefix(strings.TrimSpace(line), terminationLabelPrefix)
				if ok && pgbackrestLabelPattern.MatchString(label) {
					return fmt.Sprintf("s3://%s%s", backup.Spec.Destination.S3.Bucket,
						path.Join(repoPath(backup), "backup", pgbackrestStanza, label))
				}
			}
		}
	}
	return ""
}

// backupLabelFromLocation returns the pgBackRest backup label at the end of a
// physical BackupRun location, or "" when location is empty.
func backupLabelFromLocation(location string) (string, error) {
	if location == "" {
		return "", nil
	}
	label := path.Base(location)
	if !pgbackrestLabelPattern.MatchString(label) {
		return "", fmt.Errorf("location %q does not end in a pgBackRest backup label", location)
	}
	return label, nil
}
