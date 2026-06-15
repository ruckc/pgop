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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

const defaultBackupRunTTL = 168 * time.Hour // 7 days

// BackupRunReconciler reconciles BackupRun objects
type BackupRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backupruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backupruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=backupruns/finalizers,verbs=update

func (r *BackupRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	run := &postgresv1alpha1.BackupRun{}
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
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

	if run.Status.Phase == postgresv1alpha1.BackupRunPhaseRunning {
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
				Reason:             "Succeeded",
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
				Reason:             "Failed",
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
		Named("backuprun").
		Complete(r)
}
