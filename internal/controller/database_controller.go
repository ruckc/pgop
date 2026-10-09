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
	"slices"
	"time"

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
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=roles,verbs=get;list;watch

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
				adminClient, err := newOperatorClient(ctx, r.Client, cluster, defaultDatabaseName)
				if err != nil {
					log.Error(err, "Failed to create PostgreSQL admin client during deletion")
					return ctrl.Result{}, err
				}
				defer func() { _ = adminClient.Close() }()
				// Prefer the name recorded in status so we drop what was actually created.
				pgName := database.Status.DatabaseName
				if pgName == "" {
					pgName = database.PostgresName()
				}
				if err := adminClient.DropDatabase(ctx, pgName); err != nil {
					log.Error(err, "Failed to drop database")
					return ctrl.Result{}, err
				}
			} else {
				log.Info("Cluster not found during deletion, skipping PG cleanup")
			}

			base := database.DeepCopy()
			controllerutil.RemoveFinalizer(database, databaseFinalizer)
			if err := r.Patch(ctx, database, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
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

	// Resolve spec.owner (a Role resource name) to its PostgreSQL role name.
	// Done before connecting so a missing/unready owner is reported cheaply.
	ownerRole, err := r.resolveOwnerRole(ctx, database)
	if err != nil {
		log.Info("Owner role not available", "reason", err.Error())
		return r.updateStatus(ctx, database, false, nil, nil, err)
	}
	ownerPGName := ""
	if ownerRole != nil {
		ownerPGName = ownerRole.PostgresName()
	}

	// Get operator credentials for admin connection
	adminClient, err := newOperatorClient(ctx, r.Client, cluster, defaultDatabaseName)
	if err != nil {
		log.Error(err, "Failed to create PostgreSQL admin client")
		return r.updateStatus(ctx, database, false, nil, nil, err)
	}
	defer func() { _ = adminClient.Close() }()

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(database, databaseFinalizer) {
		base := database.DeepCopy()
		controllerutil.AddFinalizer(database, databaseFinalizer)
		if err := r.Patch(ctx, database, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Create the database
	pgName := database.PostgresName()
	if err := adminClient.CreateDatabase(ctx, pgName, ownerPGName); err != nil {
		log.Error(err, "Failed to create database")
		return r.updateStatus(ctx, database, false, nil, nil, err)
	}
	// Record the PostgreSQL name that now exists so deletion drops exactly it.
	database.Status.DatabaseName = pgName

	// Per-database parameter defaults (ALTER DATABASE ... SET/RESET). They
	// only affect new sessions, so they are applied before the connection
	// below is opened.
	if err := reconcileDatabaseSettings(ctx, adminClient, database, pgName); err != nil {
		log.Error(err, "Failed to reconcile database settings")
		return r.updateStatus(ctx, database, false, database.Status.InstalledExtensions, database.Status.CreatedSchemas, err)
	}

	// Database-level privileges (GRANT/REVOKE ... ON DATABASE)
	if err := reconcileDatabaseGrants(ctx, adminClient, database, pgName); err != nil {
		log.Error(err, "Failed to reconcile database grants")
		return r.updateStatus(ctx, database, false, database.Status.InstalledExtensions, database.Status.CreatedSchemas, err)
	}

	// Get a connection to the new database to install extensions and create schemas
	dbClient, err := newOperatorClient(ctx, r.Client, cluster, pgName)
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

	if err := r.reconcileCredentialsSecret(ctx, database, ownerRole, cluster); err != nil {
		log.Error(err, "Failed to reconcile database credentials secret")
		return r.updateStatus(ctx, database, false, installedExtensions, createdSchemas, err)
	}

	log.Info("Database reconciled successfully")
	return r.updateStatus(ctx, database, true, installedExtensions, createdSchemas, nil)
}

// resolveOwnerRole returns the Role named by spec.owner, or nil when no owner
// is set. It returns an error when the Role does not exist or is not Ready yet,
// since CREATE DATABASE ... OWNER requires the PostgreSQL role to exist.
func (r *DatabaseReconciler) resolveOwnerRole(ctx context.Context, database *postgresv1alpha1.Database) (*postgresv1alpha1.Role, error) {
	if database.Spec.Owner == "" {
		return nil, nil
	}
	role := &postgresv1alpha1.Role{}
	if err := r.Get(ctx, types.NamespacedName{Name: database.Spec.Owner, Namespace: database.Namespace}, role); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("owner Role %q not found in namespace %q", database.Spec.Owner, database.Namespace)
		}
		return nil, fmt.Errorf("failed to get owner Role %q: %w", database.Spec.Owner, err)
	}
	if !role.Status.Ready {
		return nil, fmt.Errorf("owner Role %q is not ready", database.Spec.Owner)
	}
	return role, nil
}

// reconcileCredentialsSecret maintains a per-database Secret combining the
// owner role's credentials with the database connection details. It is
// skipped when there is no owner, or when the owner is a NOLOGIN role (which
// has no credentials Secret).
func (r *DatabaseReconciler) reconcileCredentialsSecret(ctx context.Context, database *postgresv1alpha1.Database, ownerRole *postgresv1alpha1.Role, cluster *postgresv1alpha1.Cluster) error {
	if ownerRole == nil || !ownerRole.Spec.IsLogin() {
		return nil
	}

	// Read the role's credentials secret to obtain the password.
	roleSecretName := ownerRole.Status.SecretName
	if roleSecretName == "" {
		roleSecretName = cluster.Name + "-" + ownerRole.Name + "-credentials"
	}
	roleSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: roleSecretName, Namespace: database.Namespace}, roleSecret); err != nil {
		return fmt.Errorf("failed to get role credentials secret %s: %w", roleSecretName, err)
	}

	t, err := clusterClientTLS(ctx, r.Client, cluster)
	if err != nil {
		return err
	}

	data := map[string][]byte{
		SecretKeyUsername: roleSecret.Data[SecretKeyUsername],
		SecretKeyPassword: roleSecret.Data[SecretKeyPassword],
		SecretKeyDatabase: []byte(database.PostgresName()),
	}
	applyConnectionInfo(data, clusterHost(cluster), clusterPort(cluster), database.PostgresName(), t)

	secretName := database.Name + "-" + database.Spec.Owner + "-credentials"
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: database.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if err := controllerutil.SetControllerReference(database, desired, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on credentials secret: %w", err)
	}

	existing := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: database.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("failed to get credentials secret: %w", err)
	}

	// The Secret is fully derived, so its data is replaced wholesale; this
	// also drops keys that no longer apply (ca.crt once TLS is turned off).
	if apiequality.Semantic.DeepEqual(existing.Data, data) {
		return nil
	}
	existing.Data = data
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

// databasesForOwnerRole maps a Role to the Databases in its namespace whose
// spec.owner names it, or whose spec.grants name its PostgreSQL role, so they
// reconcile when the owner becomes Ready or a grantee is created.
func (r *DatabaseReconciler) databasesForOwnerRole(ctx context.Context, obj client.Object) []reconcile.Request {
	databases := &postgresv1alpha1.DatabaseList{}
	if err := r.List(ctx, databases, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Databases for owner Role", "role", obj.GetName())
		return nil
	}
	pgName := ""
	if role, ok := obj.(*postgresv1alpha1.Role); ok {
		pgName = role.PostgresName()
	}
	grantsTo := func(db *postgresv1alpha1.Database) bool {
		return pgName != "" && slices.ContainsFunc(db.Spec.Grants, func(g postgresv1alpha1.DatabaseGrantSpec) bool {
			return g.Role == pgName
		})
	}
	var requests []reconcile.Request
	for i := range databases.Items {
		if databases.Items[i].Spec.Owner == obj.GetName() || grantsTo(&databases.Items[i]) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&databases.Items[i])})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *DatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&postgresv1alpha1.Database{}).
		Owns(&corev1.Secret{}).
		Watches(&postgresv1alpha1.Role{}, handler.EnqueueRequestsFromMapFunc(r.databasesForOwnerRole)).
		// The Database Secret copies the owner's password: follow changes to
		// the role credentials Secret (passwordSecretRef updates, rotations).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.databasesForRoleSecret),
			builder.WithPredicates(predicate.NewPredicateFuncs(isRoleCredentialsSecret))).
		Watches(&postgresv1alpha1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.databasesForCluster),
			builder.WithPredicates(clusterConnectionChanged)).
		Named("database").
		Complete(r)
}

// databasesForCluster maps a Cluster to the Databases in its namespace that
// reference it, so their credentials Secrets follow port and TLS changes.
func (r *DatabaseReconciler) databasesForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	databases := &postgresv1alpha1.DatabaseList{}
	if err := r.List(ctx, databases, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Databases for Cluster", "cluster", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range databases.Items {
		if databases.Items[i].Spec.ClusterRef.Name == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&databases.Items[i])})
		}
	}
	return requests
}

// isRoleCredentialsSecret reports whether obj is a role credentials Secret
// created by the Role controller.
func isRoleCredentialsSecret(obj client.Object) bool {
	l := obj.GetLabels()
	return l[LabelAppName] == AppNamePostgresqlRole && l[LabelAppManagedBy] == LabelValuePgop &&
		l[LabelAppInstance] != "" && l[LabelCluster] != ""
}

// databasesForRoleSecret maps a role credentials Secret to the Databases owned
// by that Role on the same Cluster, so their credentials Secrets pick up a
// changed password.
func (r *DatabaseReconciler) databasesForRoleSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	if !isRoleCredentialsSecret(obj) {
		return nil
	}
	roleName, clusterName := obj.GetLabels()[LabelAppInstance], obj.GetLabels()[LabelCluster]
	databases := &postgresv1alpha1.DatabaseList{}
	if err := r.List(ctx, databases, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Databases for role Secret", "secret", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range databases.Items {
		db := &databases.Items[i]
		if db.Spec.Owner == roleName && db.Spec.ClusterRef.Name == clusterName {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(db)})
		}
	}
	return requests
}
