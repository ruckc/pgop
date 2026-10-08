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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

const (
	roleFinalizer = "pgop.ruck.io/role-finalizer"
)

// RoleReconciler reconciles a Role object
type RoleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits Kubernetes Events for the Role (password rotations).
	// Optional: when nil (as in tests) no Events are emitted.
	Recorder events.EventRecorder
	// APIReader reads the Role uncached before a password rotation, so a
	// stale cache cannot cause a second rotation. Optional: when nil the
	// cached Role is used.
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=pgop.ruck.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=roles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=roles/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *RoleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the Role instance
	role := &postgresv1alpha1.Role{}
	err := r.Get(ctx, req.NamespacedName, role)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion before attempting cluster connection
	if role.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(role, roleFinalizer) {
			cluster, err := r.getCluster(ctx, role)
			if err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to get Cluster during deletion")
				return ctrl.Result{}, err
			}
			if err == nil {
				pgClient, err := newOperatorClient(ctx, r.Client, cluster, defaultDatabaseName)
				if err != nil {
					log.Error(err, "Failed to create PostgreSQL client during deletion")
					return ctrl.Result{}, err
				}
				defer func() { _ = pgClient.Close() }()
				// Prefer the name recorded in status so we drop what was actually created.
				pgName := role.Status.RoleName
				if pgName == "" {
					pgName = role.PostgresName()
				}
				if err := pgClient.DropRole(ctx, pgName); err != nil {
					log.Error(err, "Failed to drop role")
					return ctrl.Result{}, err
				}
			} else {
				log.Info("Cluster not found during deletion, skipping PG cleanup")
			}

			// Secret will be garbage collected due to owner reference
			base := role.DeepCopy()
			controllerutil.RemoveFinalizer(role, roleFinalizer)
			if err := r.Patch(ctx, role, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Get the referenced cluster
	cluster, err := r.getCluster(ctx, role)
	if err != nil {
		log.Error(err, "Failed to get Cluster")
		return r.updateStatus(ctx, role, false, "", err)
	}

	// Check if cluster is ready
	if !cluster.Status.Ready {
		log.Info("Cluster not ready, requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Get operator credentials
	pgClient, err := newOperatorClient(ctx, r.Client, cluster, defaultDatabaseName)
	if err != nil {
		log.Error(err, "Failed to create PostgreSQL client")
		return r.updateStatus(ctx, role, false, "", err)
	}
	defer func() { _ = pgClient.Close() }()

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(role, roleFinalizer) {
		base := role.DeepCopy()
		controllerutil.AddFinalizer(role, roleFinalizer)
		if err := r.Patch(ctx, role, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Resolve the password. NOLOGIN (group) roles get no password and no
	// credentials Secret.
	now := time.Now()
	var dp desiredPassword
	var existingSecret *corev1.Secret
	if role.Spec.IsLogin() {
		if existingSecret, err = r.getCredentialsSecret(ctx, role, cluster); err != nil {
			log.Error(err, "Failed to get credentials secret")
			return r.updateStatus(ctx, role, false, "", err)
		}
		if dp, existingSecret, err = r.resolvePassword(ctx, role, existingSecret, now); err != nil {
			log.Error(err, "Failed to resolve role password")
			return r.updateStatus(ctx, role, false, "", err)
		}
	}

	// Create or update the role. The password (sent as a SCRAM-SHA-256
	// verifier, never in plaintext) is only sent to an existing role when it
	// differs from the one last applied.
	opts := postgres.RoleOptions{
		Login:                role.Spec.IsLogin(),
		Superuser:            role.Spec.Superuser,
		CreateDB:             role.Spec.CreateDB,
		CreateRole:           role.Spec.CreateRole,
		Inherit:              role.Spec.IsInherit(),
		Replication:          role.Spec.Replication,
		BypassRLS:            role.Spec.BypassRLS,
		ConnectionLimit:      role.Spec.GetConnectionLimit(),
		Password:             dp.value,
		KeepExistingPassword: dp.applied && !dp.force,
	}

	pgName := role.PostgresName()
	if err := pgClient.CreateRole(ctx, pgName, opts); err != nil {
		log.Error(err, "Failed to create/update role")
		return r.updateStatus(ctx, role, false, role.Status.SecretName, err)
	}
	// Record the PostgreSQL name that now exists so deletion drops exactly it.
	role.Status.RoleName = pgName

	// Write the credentials Secret after PostgreSQL accepted the password: if
	// the write fails, the next reconcile sets the password again (or, for a
	// rotation, rotates again) instead of handing out a password PostgreSQL
	// does not know.
	var secretName string
	if role.Spec.IsLogin() {
		if secretName, err = r.reconcileCredentialsSecret(ctx, role, cluster, existingSecret, dp.value); err != nil {
			log.Error(err, "Failed to reconcile credentials secret")
			return r.updateStatus(ctx, role, false, role.Status.SecretName, err)
		}
		recordPassword(role, dp, now)
		if dp.rotated {
			log.Info("Rotated role password")
			if r.Recorder != nil {
				r.Recorder.Eventf(role, nil, corev1.EventTypeNormal, ReasonPasswordRotated, "RotatePassword",
					"Rotated the password of role %q", pgName)
			}
		}
	} else {
		role.Status.PasswordHash = ""
		role.Status.PasswordRotatedAt = nil
	}

	// Handle role memberships (grant, update options, revoke removed ones)
	if err := reconcileMemberships(ctx, pgClient, role, pgName); err != nil {
		log.Error(err, "Failed to reconcile role memberships")
		return r.updateStatus(ctx, role, false, secretName, err)
	}

	log.Info("Role reconciled successfully")
	result, err := r.updateStatus(ctx, role, true, secretName, nil)
	if err == nil {
		if next := nextRotationIn(role, time.Now()); next > 0 {
			result.RequeueAfter = next
		}
	}
	return result, err
}

// credentialsSecretName returns the name of the Role's credentials Secret.
func credentialsSecretName(role *postgresv1alpha1.Role, cluster *postgresv1alpha1.Cluster) string {
	return cluster.Name + "-" + role.Name + "-credentials"
}

// getCredentialsSecret returns the Role's credentials Secret, or nil when it
// does not exist yet.
func (r *RoleReconciler) getCredentialsSecret(ctx context.Context, role *postgresv1alpha1.Role, cluster *postgresv1alpha1.Cluster) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: credentialsSecretName(role, cluster), Namespace: role.Namespace}, secret)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return secret, nil
}

// roleSecretLabels are the labels of a role credentials Secret. The Database
// controller uses them to map the Secret back to the Role.
func roleSecretLabels(role *postgresv1alpha1.Role) map[string]string {
	return map[string]string{
		LabelAppName:      AppNamePostgresqlRole,
		LabelAppInstance:  role.Name,
		LabelAppManagedBy: LabelValuePgop,
		LabelCluster:      role.Spec.ClusterRef.Name,
	}
}

// reconcileCredentialsSecret creates or updates the Role's credentials Secret
// so it holds password and the current connection info (host, port, sslmode,
// ca.crt and a uri built from the same password). existing is the current
// Secret, or nil. It returns the Secret's name.
func (r *RoleReconciler) reconcileCredentialsSecret(ctx context.Context, role *postgresv1alpha1.Role, cluster *postgresv1alpha1.Cluster, existing *corev1.Secret, password string) (string, error) {
	secretName := credentialsSecretName(role, cluster)

	t, err := clusterClientTLS(ctx, r.Client, cluster)
	if err != nil {
		return "", err
	}
	host, port := clusterHost(cluster), clusterPort(cluster)

	if existing == nil {
		data := map[string][]byte{
			SecretKeyUsername: []byte(role.PostgresName()),
			SecretKeyPassword: []byte(password),
		}
		applyConnectionInfo(data, host, port, defaultDatabaseName, t)
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        secretName,
				Namespace:   role.Namespace,
				Labels:      roleSecretLabels(role),
				Annotations: map[string]string{AnnotationPasswordFingerprint: passwordFingerprint(role, password)},
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		}
		// Set owner reference so secret is garbage collected when Role is deleted
		if err := controllerutil.SetControllerReference(role, secret, r.Scheme); err != nil {
			return "", err
		}
		return secretName, r.Create(ctx, secret)
	}

	// The Secret exists: keep its username and converge password, labels and
	// connection info (host, port, sslmode, uri, ca.crt). On a conflict the
	// Secret is re-read and the change applied again.
	secret := existing.DeepCopy()
	return secretName, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if !convergeRoleSecret(secret, role, password, host, port, t) {
			return nil
		}
		err := r.Update(ctx, secret)
		if apierrors.IsConflict(err) {
			if getErr := r.Get(ctx, client.ObjectKeyFromObject(secret), secret); getErr != nil {
				return getErr
			}
		}
		return err
	})
}

// convergeRoleSecret sets the password, labels and connection info of an
// existing role credentials Secret and reports whether anything changed.
func convergeRoleSecret(secret *corev1.Secret, role *postgresv1alpha1.Role, password, host string, port int32, t clientTLS) bool {
	changed := false
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if string(secret.Data[SecretKeyPassword]) != password {
		secret.Data[SecretKeyPassword] = []byte(password)
		changed = true
	}
	// Only called once PostgreSQL has the password, so the fingerprint
	// records what PostgreSQL has.
	if fp := passwordFingerprint(role, password); secret.Annotations[AnnotationPasswordFingerprint] != fp {
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[AnnotationPasswordFingerprint] = fp
		changed = true
	}
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	for k, v := range roleSecretLabels(role) {
		if secret.Labels[k] != v {
			secret.Labels[k] = v
			changed = true
		}
	}
	// applyConnectionInfo rebuilds uri from the (possibly new) password.
	if applyConnectionInfo(secret.Data, host, port, defaultDatabaseName, t) {
		changed = true
	}
	return changed
}

func generateRolePassword(length int) (string, error) {
	bytes := make([]byte, length/2)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func (r *RoleReconciler) getCluster(ctx context.Context, role *postgresv1alpha1.Role) (*postgresv1alpha1.Cluster, error) {
	cluster := &postgresv1alpha1.Cluster{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      role.Spec.ClusterRef.Name,
		Namespace: role.Namespace,
	}, cluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster: %w", err)
	}

	return cluster, nil
}

func (r *RoleReconciler) updateStatus(ctx context.Context, role *postgresv1alpha1.Role, ready bool, secretName string, reconcileErr error) (ctrl.Result, error) {
	role.Status.Ready = ready
	role.Status.SecretName = secretName

	condition := metav1.Condition{
		Type:               ConditionTypeAvailable,
		ObservedGeneration: role.Generation,
		LastTransitionTime: metav1.Now(),
	}

	if ready {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "RoleReady"
		condition.Message = "Role has been created in PostgreSQL"
	} else if reconcileErr != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = ReasonReconcileError
		if ce, ok := errors.AsType[*conditionError](reconcileErr); ok {
			condition.Reason = ce.reason
		}
		condition.Message = reconcileErr.Error()
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "RoleNotReady"
		condition.Message = "Role is not ready yet"
	}

	meta.SetStatusCondition(&role.Status.Conditions, condition)

	if err := r.Status().Update(ctx, role); err != nil {
		return ctrl.Result{}, err
	}

	if reconcileErr != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RoleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&postgresv1alpha1.Role{}).
		Owns(&corev1.Secret{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.rolesForPasswordSecret)).
		Watches(&postgresv1alpha1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.rolesForCluster),
			builder.WithPredicates(clusterConnectionChanged)).
		Named("role").
		Complete(r)
}

// rolesForCluster maps a Cluster to the Roles in its namespace that reference
// it, so their credentials Secrets follow port and TLS changes.
func (r *RoleReconciler) rolesForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	roles := &postgresv1alpha1.RoleList{}
	if err := r.List(ctx, roles, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Roles for Cluster", "cluster", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range roles.Items {
		if roles.Items[i].Spec.ClusterRef.Name == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&roles.Items[i])})
		}
	}
	return requests
}
