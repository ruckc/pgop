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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

const defaultBackupRunTTL = 168 * time.Hour // 7 days

// BackupRunReconciler reconciles BackupRun objects
type BackupRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads the pods of backup Jobs, which the manager's pod cache
	// (PostgreSQL pods only) does not hold. Optional; defaults to Client.
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backupruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backupruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backupruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func (r *BackupRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	run := &postgresv1alpha1.BackupRun{}
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if requeue, err := r.recordLateLocation(ctx, run); err != nil || requeue {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	// Enforce TTL: delete the BackupRun record once it has expired
	if run.Status.CompletionTime != nil {
		ttl := r.ttlFor(run)
		age := time.Since(run.Status.CompletionTime.Time)
		if age >= ttl {
			log.Info("BackupRun TTL expired, deleting", "name", run.Name, "age", age)
			if err := r.Delete(ctx, run); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		// Requeue just before expiry
		return ctrl.Result{RequeueAfter: ttl - age}, nil
	}

	// Sync status from the referenced Job if one exists
	if run.Status.JobName != "" {
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: run.Status.JobName, Namespace: run.Namespace}, job)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err == nil {
			updated := r.syncFromJob(run, job)
			if r.recordLocation(ctx, run, job) {
				updated = true
			}
			if updated {
				if err := r.Status().Update(ctx, run); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
	}

	// Initialise phase for new runs
	if run.Status.Phase == "" {
		run.Status.Phase = postgresv1alpha1.BackupRunPhasePending
		metaCondition := metav1.Condition{
			Type:               ConditionTypeAvailable,
			Status:             metav1.ConditionFalse,
			Reason:             "Pending",
			Message:            "BackupRun is waiting to be scheduled",
			ObservedGeneration: run.Generation,
			LastTransitionTime: metav1.Now(),
		}
		meta.SetStatusCondition(&run.Status.Conditions, metaCondition)
		if err := r.Status().Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
	}

	if run.Status.Phase == postgresv1alpha1.BackupRunPhaseRunning ||
		(run.Status.Phase == postgresv1alpha1.BackupRunPhasePending && run.Status.JobName != "") {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *BackupRunReconciler) syncFromJob(run *postgresv1alpha1.BackupRun, job *batchv1.Job) bool {
	changed := false

	if job.Status.StartTime != nil && run.Status.StartTime == nil {
		run.Status.StartTime = job.Status.StartTime
		changed = true
	}

	if run.Status.Phase != postgresv1alpha1.BackupRunPhaseSucceeded &&
		run.Status.Phase != postgresv1alpha1.BackupRunPhaseFailed {

		if job.Status.Succeeded > 0 {
			run.Status.Phase = postgresv1alpha1.BackupRunPhaseSucceeded
			now := metav1.Now()
			run.Status.CompletionTime = &now
			meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeAvailable,
				Status:             metav1.ConditionTrue,
				Reason:             reasonSucceeded,
				Message:            "BackupRun completed successfully",
				ObservedGeneration: run.Generation,
				LastTransitionTime: now,
			})
			changed = true
		} else if job.Status.Failed > 0 {
			conds := job.Status.Conditions
			msg := "backup job failed"
			for _, c := range conds {
				if c.Type == batchv1.JobFailed {
					msg = c.Message
					break
				}
			}
			run.Status.Phase = postgresv1alpha1.BackupRunPhaseFailed
			now := metav1.Now()
			run.Status.CompletionTime = &now
			meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
				Type:               ConditionTypeAvailable,
				Status:             metav1.ConditionFalse,
				Reason:             reasonFailed,
				Message:            msg,
				ObservedGeneration: run.Generation,
				LastTransitionTime: now,
			})
			changed = true
		} else if job.Status.Active > 0 && run.Status.Phase != postgresv1alpha1.BackupRunPhaseRunning {
			run.Status.Phase = postgresv1alpha1.BackupRunPhaseRunning
			changed = true
		}
	}

	return changed
}

// recordLocation records where a successful physical backup Job stored its
// backup (see physicalBackupLocation). Returns whether the status changed.
func (r *BackupRunReconciler) recordLocation(ctx context.Context, run *postgresv1alpha1.BackupRun, job *batchv1.Job) bool {
	if run.Status.Location != "" || job.Status.Succeeded == 0 || !isPhysicalRun(run) {
		return false
	}
	backup := &postgresv1alpha1.Backup{}
	if err := r.Get(ctx, types.NamespacedName{Name: run.Spec.BackupRef.Name, Namespace: run.Namespace}, backup); err != nil {
		return false
	}
	pods := &corev1.PodList{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.List(ctx, pods, client.InNamespace(run.Namespace), client.MatchingLabels{labelJobName: job.Name}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list the pods of a backup Job", "job", job.Name)
		return false
	}
	run.Status.Location = physicalBackupLocation(backup, pods.Items)
	return run.Status.Location != ""
}

// recordLateLocation records the location of a physical backup whose run
// already completed without one: the location is read from the Job's pod,
// whose status can arrive after the Job's. Returns whether to look again.
func (r *BackupRunReconciler) recordLateLocation(ctx context.Context, run *postgresv1alpha1.BackupRun) (bool, error) {
	if run.Status.CompletionTime == nil || run.Status.Phase != postgresv1alpha1.BackupRunPhaseSucceeded ||
		run.Status.Location != "" || run.Status.JobName == "" || !isPhysicalRun(run) {
		return false, nil
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: run.Status.JobName, Namespace: run.Namespace}, job); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if r.recordLocation(ctx, run, job) {
		return false, r.Status().Update(ctx, run)
	}
	return time.Since(run.Status.CompletionTime.Time) < time.Minute, nil
}

// isPhysicalRun reports whether the run is a pgBackRest backup.
func isPhysicalRun(run *postgresv1alpha1.BackupRun) bool {
	return run.Spec.Type == postgresv1alpha1.BackupRunTypeFull || run.Spec.Type == postgresv1alpha1.BackupRunTypeIncremental
}

func (r *BackupRunReconciler) ttlFor(run *postgresv1alpha1.BackupRun) time.Duration {
	if run.Spec.TTL != nil {
		return run.Spec.TTL.Duration
	}
	return defaultBackupRunTTL
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&postgresv1alpha1.BackupRun{}).
		// A BackupRun recorded by the Backup controller has the name of its
		// Job.
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(backupRunForJob)).
		Named("backuprun").
		Complete(r)
}

// backupRunForJob maps a backup Job to the BackupRun of the same name.
func backupRunForJob(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()[LabelAppName] != appNameBackup {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(obj)}}
}
