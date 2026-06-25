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
	"strconv"
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
	databaseFinalizer = "pgop.ruck.io/database-finalizer"
)

// DatabaseReconciler reconciles a Database object
type DatabaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pgop.ruck.io,resources=databases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=databases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=databases/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch

func (r *DatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the Database instance
	database := &postgresv1alpha1.Database{}
	err := r.Get(ctx, req.NamespacedName, database)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion before attempting cluster connection
	if database.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(database, databaseFinalizer) {
			cluster, err := r.getCluster(ctx, database)
			if err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to get Cluster during deletion")
				return ctrl.Result{}, err
			}
			if err == nil {
				adminClient, err := r.getPostgresClient(ctx, cluster, "postgres")
				if err != nil {
					log.Error(err, "Failed to create PostgreSQL admin client during deletion")
					return ctrl.Result{}, err
				}
				defer func() { _ = adminClient.Close() }()
				if err := adminClient.DropDatabase(ctx, database.Name); err != nil {
					log.Error(err, "Failed to drop database")
					return ctrl.Result{}, err
				}
			} else {
				log.Info("Cluster not found during deletion, skipping PG cleanup")
			}

			controllerutil.RemoveFinalizer(database, databaseFinalizer)
			if err := r.Update(ctx, database); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Get the referenced cluster
	cluster, err := r.getCluster(ctx, database)
	if err != nil {
		log.Error(err, "Failed to get Cluster")
		return r.updateStatus(ctx, database, false, nil, nil, err)
	}

	// Check if cluster is ready
	if !cluster.Status.Ready {
		log.Info("Cluster not ready, requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Get operator credentials for admin connection
	adminClient, err := r.getPostgresClient(ctx, cluster, "postgres")
	if err != nil {
		log.Error(err, "Failed to create PostgreSQL admin client")
		return r.updateStatus(ctx, database, false, nil, nil, err)
	}
	defer func() { _ = adminClient.Close() }()

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(database, databaseFinalizer) {
		controllerutil.AddFinalizer(database, databaseFinalizer)
		if err := r.Update(ctx, database); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Create the database
	if err := adminClient.CreateDatabase(ctx, database.Name, database.Spec.Owner); err != nil {
		log.Error(err, "Failed to create database")
		return r.updateStatus(ctx, database, false, nil, nil, err)
	}

	// Get a connection to the new database to install extensions and create schemas
	dbClient, err := r.getPostgresClient(ctx, cluster, database.Name)
	if err != nil {
		log.Error(err, "Failed to create PostgreSQL database client")
		return r.updateStatus(ctx, database, false, nil, nil, err)
	}
	defer func() { _ = dbClient.Close() }()

	// Install extensions
	installedExtensions := make([]string, 0, len(database.Spec.Extensions))
	for _, ext := range database.Spec.Extensions {
		if err := dbClient.CreateExtension(ctx, ext.Name, ext.Schema, ext.Version); err != nil {
			log.Error(err, "Failed to create extension", "extension", ext.Name)
			return r.updateStatus(ctx, database, false, installedExtensions, nil, err)
		}
		installedExtensions = append(installedExtensions, ext.Name)
	}

	// Create schemas and apply grants
	createdSchemas := make([]string, 0, len(database.Spec.Schemas))
	for _, schema := range database.Spec.Schemas {
		if err := dbClient.CreateSchema(ctx, schema.Name, schema.Owner); err != nil {
			log.Error(err, "Failed to create schema", "schema", schema.Name)
			return r.updateStatus(ctx, database, false, installedExtensions, createdSchemas, err)
		}
		createdSchemas = append(createdSchemas, schema.Name)

		// Apply grants
		for _, grant := range schema.Grants {
			if err := dbClient.GrantSchemaPrivileges(ctx, schema.Name, grant.Role, grant.Privileges, grant.WithGrantOption); err != nil {
				log.Error(err, "Failed to grant privileges", "schema", schema.Name, "role", grant.Role)
				return r.updateStatus(ctx, database, false, installedExtensions, createdSchemas, err)
			}
		}
	}

	if err := r.reconcileCredentialsSecret(ctx, database, cluster); err != nil {
		log.Error(err, "Failed to reconcile database credentials secret")
		return r.updateStatus(ctx, database, false, installedExtensions, createdSchemas, err)
	}

	log.Info("Database reconciled successfully")
	return r.updateStatus(ctx, database, true, installedExtensions, createdSchemas, nil)
}

func (r *DatabaseReconciler) reconcileCredentialsSecret(ctx context.Context, database *postgresv1alpha1.Database, cluster *postgresv1alpha1.Cluster) error {
	// Read the role's credentials secret to obtain the password.
	roleSecretName := cluster.Name + "-" + database.Spec.Owner + "-credentials"
	roleSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: roleSecretName, Namespace: database.Namespace}, roleSecret); err != nil {
		return fmt.Errorf("failed to get role credentials secret %s: %w", roleSecretName, err)
	}

	port := cluster.Spec.Port
	if port == 0 {
		port = 5432
	}

	secretName := database.Name + "-" + database.Spec.Owner + "-credentials"
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: database.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			SecretKeyUsername: string(roleSecret.Data[SecretKeyUsername]),
			SecretKeyPassword: string(roleSecret.Data[SecretKeyPassword]),
			SecretKeyHost:     fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace),
			SecretKeyPort:     strconv.Itoa(int(port)),
			SecretKeyDatabase: database.Name,
		},
	}
	if err := controllerutil.SetControllerReference(database, desired, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on credentials secret: %w", err)
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: database.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("failed to get credentials secret: %w", err)
	}

	existing.StringData = desired.StringData
	return r.Update(ctx, existing)
}

func (r *DatabaseReconciler) getCluster(ctx context.Context, database *postgresv1alpha1.Database) (*postgresv1alpha1.Cluster, error) {
	cluster := &postgresv1alpha1.Cluster{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      database.Spec.ClusterRef.Name,
		Namespace: database.Namespace,
	}, cluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster: %w", err)
	}

	return cluster, nil
}

func (r *DatabaseReconciler) getPostgresClient(ctx context.Context, cluster *postgresv1alpha1.Cluster, dbName string) (*postgres.Client, error) {
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
		Database: dbName,
	})
}

func (r *DatabaseReconciler) updateStatus(ctx context.Context, database *postgresv1alpha1.Database, ready bool, extensions, schemas []string, reconcileErr error) (ctrl.Result, error) {
	database.Status.Ready = ready
	database.Status.InstalledExtensions = extensions
	database.Status.CreatedSchemas = schemas

	condition := metav1.Condition{
		Type:               ConditionTypeAvailable,
		ObservedGeneration: database.Generation,
		LastTransitionTime: metav1.Now(),
	}

	if ready {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "DatabaseReady"
		condition.Message = "Database has been created with extensions and schemas"
	} else if reconcileErr != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = ReasonReconcileError
		condition.Message = reconcileErr.Error()
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "DatabaseNotReady"
		condition.Message = "Database is not ready yet"
	}

	meta.SetStatusCondition(&database.Status.Conditions, condition)

	if err := r.Status().Update(ctx, database); err != nil {
		return ctrl.Result{}, err
	}

	if reconcileErr != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&postgresv1alpha1.Database{}).
		Owns(&corev1.Secret{}).
		Named("database").
		Complete(r)
}
