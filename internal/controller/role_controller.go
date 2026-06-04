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
	"fmt"
	"time"

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
	"github.com/ruckc/pgop/internal/postgres"
)

const (
	roleFinalizer = "pgop.ruck.io/role-finalizer"
)

// RoleReconciler reconciles a Role object
type RoleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pgop.ruck.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=roles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=roles/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

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
	pgClient, err := r.getPostgresClient(ctx, cluster)
	if err != nil {
		log.Error(err, "Failed to create PostgreSQL client")
		return r.updateStatus(ctx, role, false, "", err)
	}
	defer func() { _ = pgClient.Close() }()

	// Handle deletion
	if role.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(role, roleFinalizer) {
			// Delete the role from PostgreSQL
			if err := pgClient.DropRole(ctx, role.Name); err != nil {
				log.Error(err, "Failed to drop role")
				return ctrl.Result{}, err
			}

			// Secret will be garbage collected due to owner reference

			controllerutil.RemoveFinalizer(role, roleFinalizer)
			if err := r.Update(ctx, role); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(role, roleFinalizer) {
		controllerutil.AddFinalizer(role, roleFinalizer)
		if err := r.Update(ctx, role); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Reconcile credentials secret and get password
	password, secretName, err := r.reconcileCredentialsSecret(ctx, role, cluster)
	if err != nil {
		log.Error(err, "Failed to reconcile credentials secret")
		return r.updateStatus(ctx, role, false, "", err)
	}

	// Create or update the role
	opts := postgres.RoleOptions{
		Login:           role.Spec.Login,
		Superuser:       role.Spec.Superuser,
		CreateDB:        role.Spec.CreateDB,
		CreateRole:      role.Spec.CreateRole,
		Inherit:         role.Spec.Inherit,
		Replication:     role.Spec.Replication,
		BypassRLS:       role.Spec.BypassRLS,
		ConnectionLimit: role.Spec.ConnectionLimit,
		Password:        password,
	}

	if err := pgClient.CreateRole(ctx, role.Name, opts); err != nil {
		log.Error(err, "Failed to create/update role")
		return r.updateStatus(ctx, role, false, secretName, err)
	}

	// Handle role memberships
	for _, memberOf := range role.Spec.MemberOf {
		if err := pgClient.GrantRole(ctx, memberOf, role.Name); err != nil {
			log.Error(err, "Failed to grant role membership", "role", memberOf)
			return r.updateStatus(ctx, role, false, secretName, err)
		}
	}

	log.Info("Role reconciled successfully")
	return r.updateStatus(ctx, role, true, secretName, nil)
}

func (r *RoleReconciler) reconcileCredentialsSecret(ctx context.Context, role *postgresv1alpha1.Role, cluster *postgresv1alpha1.Cluster) (string, string, error) {
	secretName := role.Name + "-credentials"

	// Check if secret already exists
	existingSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: role.Namespace}, existingSecret)
	if err == nil {
		// Secret exists, return the password from it
		password := string(existingSecret.Data["password"])
		return password, secretName, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", "", err
	}

	// Generate a new password
	password, err := generateRolePassword(32)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate password: %w", err)
	}

	// Get port for connection string
	port := cluster.Spec.Port
	if port == 0 {
		port = 5432
	}

	// Create the credentials secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: role.Namespace,
			Labels: map[string]string{
				LabelAppName:           "postgresql-role",
				LabelAppInstance:       role.Name,
				LabelAppManagedBy:      LabelValuePgop,
				"pgop.ruck.io/cluster": role.Spec.ClusterRef.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			SecretKeyUsername: role.Name,
			SecretKeyPassword: password,
			"host":            fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
			"port":            fmt.Sprintf("%d", port),
		},
	}

	// Set owner reference so secret is garbage collected when Role is deleted
	if err := controllerutil.SetControllerReference(role, secret, r.Scheme); err != nil {
		return "", "", err
	}

	if err := r.Create(ctx, secret); err != nil {
		return "", "", err
	}

	return password, secretName, nil
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

func (r *RoleReconciler) getPostgresClient(ctx context.Context, cluster *postgresv1alpha1.Cluster) (*postgres.Client, error) {
	// Get the credentials secret
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      cluster.Status.SecretName,
		Namespace: cluster.Namespace,
	}, secret)
	if err != nil {
		return nil, fmt.Errorf("failed to get credentials secret: %w", err)
	}

	user := string(secret.Data["username"])
	password := string(secret.Data["password"])

	port := cluster.Spec.Port
	if port == 0 {
		port = 5432
	}

	return postgres.NewClient(postgres.ConnectionConfig{
		Host:     fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
		Port:     port,
		User:     user,
		Password: password,
		Database: "postgres",
	})
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
		Named("role").
		Complete(r)
}
