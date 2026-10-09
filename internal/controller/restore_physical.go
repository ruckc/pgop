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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// Physical restores.
//
// A physical Restore replaces the data directory of the Cluster the Backup
// was taken from:
//
//  1. it sets the restore-in-progress annotation on the Cluster, and the
//     Cluster controller scales the StatefulSet to 0;
//  2. once no PostgreSQL pod runs, it deletes the standbys' data volumes
//     (their data no longer matches the restored primary; they are cloned
//     again) and runs "pgbackrest restore" in a Job that mounts the
//     primary's data volume;
//  3. when the Job succeeds it removes the annotation: the Cluster starts,
//     PostgreSQL recovers from the WAL archive (restore_command, written by
//     pgBackRest) up to the target and is promoted.
//
// When the Job fails the Cluster stays stopped (its data directory may be
// half restored); deleting the Restore starts it again.
const (
	restoreFinalizer = "pgop.ruck.io/restore-finalizer"

	reasonStoppingCluster = "StoppingCluster"
	reasonRestoring       = "Restoring"
	reasonWaitingForOther = "WaitingForRestore"

	restorePollInterval = 5 * time.Second
)

// errRestoreInvalid marks a Restore that can never succeed as specified.
var errRestoreInvalid = errors.New("invalid physical restore")

// physicalRestoreTarget resolves what a physical Restore restores into.
type physicalRestoreTarget struct {
	backup  *postgresv1alpha1.Backup
	cluster *postgresv1alpha1.Cluster
	layout  postgresLayout
	// label is the backup set of the BackupRun ("" for the latest).
	label string
}

func (r *RestoreReconciler) resolvePhysicalRestore(ctx context.Context, restore *postgresv1alpha1.Restore) (*physicalRestoreTarget, error) {
	backupRun := &postgresv1alpha1.BackupRun{}
	if err := r.Get(ctx, types.NamespacedName{Name: restore.Spec.BackupRunRef.Name, Namespace: restore.Namespace}, backupRun); err != nil {
		return nil, fmt.Errorf("failed to get backupRun %s: %w", restore.Spec.BackupRunRef.Name, err)
	}
	backup := &postgresv1alpha1.Backup{}
	if err := r.Get(ctx, types.NamespacedName{Name: backupRun.Spec.BackupRef.Name, Namespace: restore.Namespace}, backup); err != nil {
		return nil, fmt.Errorf("failed to get backup %s: %w", backupRun.Spec.BackupRef.Name, err)
	}
	if backup.Spec.Type != postgresv1alpha1.BackupTypePhysical {
		return nil, fmt.Errorf("%w: backup %s is not a physical backup", errRestoreInvalid, backup.Name)
	}
	if err := validatePhysicalBackup(backup); err != nil {
		return nil, fmt.Errorf("%w: %w", errRestoreInvalid, err)
	}
	if backup.Spec.ClusterRef.Name != restore.Spec.ClusterRef.Name {
		return nil, fmt.Errorf("%w: a physical restore must target the Cluster the Backup was taken from (%s)",
			errRestoreInvalid, backup.Spec.ClusterRef.Name)
	}
	label, err := backupLabelFromLocation(backupRun.Status.Location)
	if err != nil {
		return nil, fmt.Errorf("%w: backupRun %s: %w", errRestoreInvalid, backupRun.Name, err)
	}
	cluster := &postgresv1alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: restore.Spec.ClusterRef.Name, Namespace: restore.Namespace}, cluster); err != nil {
		return nil, fmt.Errorf("failed to get cluster %s: %w", restore.Spec.ClusterRef.Name, err)
	}
	layout, err := resolvePostgresLayout(cluster)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errRestoreInvalid, err)
	}
	return &physicalRestoreTarget{backup: backup, cluster: cluster, layout: layout, label: label}, nil
}

// reconcilePhysicalRestore drives a physical Restore (see above).
func (r *RestoreReconciler) reconcilePhysicalRestore(ctx context.Context, restore *postgresv1alpha1.Restore) (ctrl.Result, error) {
	if restore.DeletionTimestamp != nil {
		if !controllerutil.ContainsFinalizer(restore, restoreFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.resumeCluster(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
		base := restore.DeepCopy()
		controllerutil.RemoveFinalizer(restore, restoreFinalizer)
		return ctrl.Result{}, r.Patch(ctx, restore, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}
	if restore.Status.Phase == postgresv1alpha1.RestorePhaseSucceeded || restore.Status.Phase == postgresv1alpha1.RestorePhaseFailed {
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(restore, restoreFinalizer) {
		base := restore.DeepCopy()
		controllerutil.AddFinalizer(restore, restoreFinalizer)
		if err := r.Patch(ctx, restore, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}

	if restore.Status.JobName != "" {
		return r.followPhysicalRestoreJob(ctx, restore)
	}

	target, err := r.resolvePhysicalRestore(ctx, restore)
	if err != nil {
		if errors.Is(err, errRestoreInvalid) || apierrors.IsNotFound(err) {
			return r.failPhysicalRestore(ctx, restore, err)
		}
		return ctrl.Result{}, err
	}
	cluster := target.cluster

	// 1. Stop the Cluster.
	if holder := cluster.Annotations[AnnotationRestoreInProgress]; holder != restore.Name {
		if holder != "" && r.restoreActive(ctx, restore.Namespace, holder) {
			return r.setPhysicalRestoreProgress(ctx, restore, reasonWaitingForOther,
				fmt.Sprintf("Waiting for Restore %q to finish restoring Cluster %q", holder, cluster.Name), 10*time.Second)
		}
		base := cluster.DeepCopy()
		if cluster.Annotations == nil {
			cluster.Annotations = map[string]string{}
		}
		cluster.Annotations[AnnotationRestoreInProgress] = restore.Name
		if err := r.Patch(ctx, cluster, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
		logf.FromContext(ctx).Info("Stopping the Cluster for a physical restore", "cluster", cluster.Name)
		return r.setPhysicalRestoreProgress(ctx, restore, reasonStoppingCluster,
			fmt.Sprintf("Stopping Cluster %q", cluster.Name), restorePollInterval)
	}

	// 2. Restore the data directory once the Cluster is stopped.
	stopped, err := r.clusterStopped(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !stopped {
		return r.setPhysicalRestoreProgress(ctx, restore, reasonStoppingCluster,
			fmt.Sprintf("Waiting for the PostgreSQL pods of Cluster %q to stop", cluster.Name), restorePollInterval)
	}
	if _, err := r.deleteStandbyVolumes(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: dataPVCName(cluster), Namespace: cluster.Namespace}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failPhysicalRestore(ctx, restore, fmt.Errorf("the data volume %s of Cluster %q does not exist; "+
				"the Cluster stays stopped, delete the Restore to start it again", dataPVCName(cluster), cluster.Name))
		}
		return ctrl.Result{}, err
	}
	job := r.buildPhysicalRestoreJob(restore, target)
	if err := controllerutil.SetControllerReference(restore, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	restore.Status.JobName = job.Name
	if restore.Status.StartTime == nil {
		now := metav1.Now()
		restore.Status.StartTime = &now
	}
	return r.setPhysicalRestoreProgress(ctx, restore, reasonRestoring,
		fmt.Sprintf("Restoring the data directory of Cluster %q", cluster.Name), 10*time.Second)
}

// followPhysicalRestoreJob follows the restore Job and starts the Cluster
// again once it succeeded.
func (r *RestoreReconciler) followPhysicalRestoreJob(ctx context.Context, restore *postgresv1alpha1.Restore) (ctrl.Result, error) {
	cluster := &postgresv1alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: restore.Spec.ClusterRef.Name, Namespace: restore.Namespace}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failPhysicalRestore(ctx, restore, fmt.Errorf("cluster %s was deleted", restore.Spec.ClusterRef.Name))
		}
		return ctrl.Result{}, err
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: restore.Status.JobName, Namespace: restore.Namespace}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failPhysicalRestore(ctx, restore, fmt.Errorf("restore Job %s disappeared; the Cluster stays stopped, "+
				"delete the Restore to start it again", restore.Status.JobName))
		}
		return ctrl.Result{}, err
	}
	switch {
	case jobConditionTrue(job, batchv1.JobComplete) || job.Status.Succeeded > 0:
		// The standbys' volumes must be gone before the StatefulSet
		// scales up, so they are cloned from the restored primary.
		remaining, err := r.deleteStandbyVolumes(ctx, cluster)
		if err != nil {
			return ctrl.Result{}, err
		}
		if remaining > 0 {
			return r.setPhysicalRestoreProgress(ctx, restore, reasonRestoring,
				"Waiting for the standby data volumes to be deleted", restorePollInterval)
		}
		if err := r.resumeCluster(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
		now := metav1.Now()
		restore.Status.Phase = postgresv1alpha1.RestorePhaseSucceeded
		restore.Status.CompletionTime = &now
		meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             reasonSucceeded,
			Message:            fmt.Sprintf("Data directory restored; Cluster %q is starting and recovers from the WAL archive", cluster.Name),
			ObservedGeneration: restore.Generation,
		})
		logf.FromContext(ctx).Info("Physical restore complete; starting the Cluster", "cluster", cluster.Name)
		return ctrl.Result{}, r.Status().Update(ctx, restore)
	case jobConditionTrue(job, batchv1.JobFailed):
		msg := "the restore Job failed"
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Message != "" {
				msg = "the restore Job failed: " + c.Message
			}
		}
		return r.failPhysicalRestore(ctx, restore, fmt.Errorf("%s; Cluster %q stays stopped, delete the Restore to start it again",
			msg, cluster.Name))
	}
	return r.setPhysicalRestoreProgress(ctx, restore, reasonRestoring,
		fmt.Sprintf("Restoring the data directory of Cluster %q", cluster.Name), 10*time.Second)
}

func jobConditionTrue(job *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// setPhysicalRestoreProgress records a Running Restore's progress and
// requeues it after d.
func (r *RestoreReconciler) setPhysicalRestoreProgress(ctx context.Context, restore *postgresv1alpha1.Restore,
	reason, message string, d time.Duration) (ctrl.Result, error) {
	restore.Status.Phase = postgresv1alpha1.RestorePhaseRunning
	meta.SetStatusCondition(&restore.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: restore.Generation,
	})
	if err := r.Status().Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: d}, nil
}

// failPhysicalRestore marks the Restore Failed. A Cluster it stopped stays
// stopped until the Restore is deleted.
func (r *RestoreReconciler) failPhysicalRestore(ctx context.Context, restore *postgresv1alpha1.Restore, cause error) (ctrl.Result, error) {
	logf.FromContext(ctx).Error(cause, "Physical restore failed")
	return r.markFailed(ctx, restore, cause)
}

// restoreActive reports whether the named Restore exists and has not
// finished.
func (r *RestoreReconciler) restoreActive(ctx context.Context, namespace, name string) bool {
	other := &postgresv1alpha1.Restore{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, other); err != nil {
		return !apierrors.IsNotFound(err)
	}
	return other.DeletionTimestamp == nil &&
		other.Status.Phase != postgresv1alpha1.RestorePhaseSucceeded &&
		other.Status.Phase != postgresv1alpha1.RestorePhaseFailed
}

// resumeCluster removes the restore-in-progress annotation from the Cluster
// if this Restore set it.
func (r *RestoreReconciler) resumeCluster(ctx context.Context, restore *postgresv1alpha1.Restore) error {
	cluster := &postgresv1alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: restore.Spec.ClusterRef.Name, Namespace: restore.Namespace}, cluster); err != nil {
		return client.IgnoreNotFound(err)
	}
	if cluster.Annotations[AnnotationRestoreInProgress] != restore.Name {
		return nil
	}
	base := cluster.DeepCopy()
	delete(cluster.Annotations, AnnotationRestoreInProgress)
	return r.Patch(ctx, cluster, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// clusterStopped reports whether the Cluster's StatefulSet is scaled to 0 and
// none of its PostgreSQL pods exists any more.
func (r *RestoreReconciler) clusterStopped(ctx context.Context, cluster *postgresv1alpha1.Cluster) (bool, error) {
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, sts)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil && (sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 || sts.Status.Replicas != 0) {
		return false, nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		LabelAppName:     AppNamePostgresql,
		LabelAppInstance: cluster.Name,
	}); err != nil {
		return false, err
	}
	return len(pods.Items) == 0, nil
}

// deleteStandbyVolumes deletes the data volumes of the standbys (ordinal >=
// 1) and returns how many still exist.
func (r *RestoreReconciler) deleteStandbyVolumes(ctx context.Context, cluster *postgresv1alpha1.Cluster) (int, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		LabelAppName:     AppNamePostgresql,
		LabelAppInstance: cluster.Name,
	}); err != nil {
		return 0, err
	}
	remaining := 0
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		ordinal, ok := pvcOrdinal(cluster, pvc.Name)
		if !ok || ordinal == 0 {
			continue
		}
		remaining++
		if pvc.DeletionTimestamp != nil {
			continue
		}
		if err := r.Delete(ctx, pvc, client.Preconditions{UID: &pvc.UID}); err != nil && !apierrors.IsNotFound(err) {
			return remaining, err
		}
		logf.FromContext(ctx).Info("Deleted a standby data volume for a physical restore", "pvc", pvc.Name)
	}
	return remaining, nil
}

// physicalRestoreArgs are the pgbackrest restore options:
//
//   - with spec.targetTime: point-in-time recovery to that time, from the
//     newest backup taken before it (pgBackRest selects it);
//   - otherwise, with a BackupRun that recorded its backup: that backup set,
//     recovered just to consistency (type=immediate);
//   - otherwise: the latest backup, replaying all archived WAL.
func physicalRestoreArgs(restore *postgresv1alpha1.Restore, label string) []string {
	const promote = "--target-action=promote"
	// --delta only rewrites the files that differ from the backup.
	args := []string{"--delta"}
	switch {
	case restore.Spec.TargetTime != nil:
		t := restore.Spec.TargetTime.UTC().Format("2006-01-02 15:04:05") + "+00"
		args = append(args, "--type=time", "--target="+t, promote)
	case label != "":
		args = append(args, "--set="+label, "--type=immediate", promote)
	}
	return args
}

// buildPhysicalRestoreJob runs "pgbackrest restore" into the primary's data
// volume. The data directory is created if needed, and a postmaster.pid left
// behind by an unclean shutdown is removed (no PostgreSQL pod runs: the
// Cluster is stopped).
func (r *RestoreReconciler) buildPhysicalRestoreJob(restore *postgresv1alpha1.Restore, t *physicalRestoreTarget) *batchv1.Job {
	script := `set -eu
mkdir -p "$PGBACKREST_PG1_PATH"
chmod 0700 "$PGBACKREST_PG1_PATH"
rm -f "$PGBACKREST_PG1_PATH/postmaster.pid"
echo "pgop: restoring into $PGBACKREST_PG1_PATH from repository $PGBACKREST_REPO1_PATH: $*"
exec ` + pgbackrestBin + ` restore "$@"
`

	env := append(pgbackrestRepoEnv(t.backup), pgbackrestPGEnv(t.cluster, t.layout)...)
	caVolumes, caMounts := s3CAVolume(t.backup)

	job := r.newRestoreJob(restore, string(postgresv1alpha1.BackupTypePhysical))
	job.Spec.Template.Spec.SecurityContext = pgbackrestPodSecurityContext()
	// Never: the pod (and log) of a failed attempt is kept for inspection.
	job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	job.Spec.Template.Spec.Containers = []corev1.Container{{
		Name:            pgbackrestContainerName,
		Image:           pgbackrestImage(t.backup),
		SecurityContext: restoreContainerSecurityContext(true),
		// The restore options are passed as arguments ($@), so a target
		// time with a space needs no shell quoting.
		Command: append([]string{shBin, "-c", script, pgbackrestContainerName}, physicalRestoreArgs(restore, t.label)...),
		Env:     env,
		VolumeMounts: append([]corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: t.layout.MountPath},
			{Name: volPgbackrestTmp, MountPath: pgbackrestTmpMountPath},
		}, caMounts...),
	}}
	job.Spec.Template.Spec.Volumes = append([]corev1.Volume{
		{Name: dataVolumeName, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dataPVCName(t.cluster)},
		}},
		{Name: volPgbackrestTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}, caVolumes...)
	return job
}
