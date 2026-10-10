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
	"slices"
	"strings"
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
				if err := r.dropPostgresDatabase(ctx, cluster, database); err != nil {
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

	// The maintenance and template databases are never managed (owning
	// template1 would let the owner plant objects in every future database),
	// and of two Databases of a Cluster that resolve to the same PostgreSQL
	// name only the older one is reconciled.
	pgName := database.PostgresName()
	if err := r.checkDatabaseName(ctx, database, pgName); err != nil {
		return r.updateStatus(ctx, database, false, nil, nil, err)
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

	// Create the database, or update the owner of one this Database owns
	// (it carries the Database's signed ownership marker).
	signer, err := loadMarkerSigner(ctx, r.Client, cluster)
	if err != nil {
		return r.updateStatus(ctx, database, false, nil, nil, err)
	}
	recorded, err := r.databaseRecorded(ctx, database, cluster, pgName)
	if err != nil {
		return r.updateStatus(ctx, database, false, nil, nil, err)
	}
	if err := ensureDatabase(ctx, adminClient, database, cluster, signer, pgName, ownerPGName, recorded); err != nil {
		log.Error(err, "Failed to create database")
		return r.updateStatus(ctx, database, false, nil, nil, err)
	}

	// Settings and database grants. A problem with them (a grantee that does
	// not exist yet, a setting that is not allowed) does not hold up the
	// extensions, schemas and credentials below: it is reported once those
	// are done, and the Database stays not ready until it is fixed.
	accessErr := r.reconcileSettingsAndGrants(ctx, adminClient, database, pgName)
	if accessErr != nil {
		log.Error(accessErr, "Failed to reconcile database settings or grants")
	}

	// Get a connection to the new database to install extensions and create
	// schemas. Its owner can turn connections off, for superusers too.
	if allow, err := adminClient.DatabaseAllowsConnections(ctx, pgName); err != nil || !allow {
		if err == nil {
			err = &conditionError{reason: ReasonDatabaseNotConnectable, err: fmt.Errorf(
				"the database %s does not allow connections (ALTER DATABASE %s WITH ALLOW_CONNECTIONS true); "+
					"its extensions and schemas cannot be reconciled", pgName, pgName)}
		}
		return r.updateStatus(ctx, database, false, nil, nil, errors.Join(err, accessErr))
	}
	dbClient, err := newOperatorClient(ctx, r.Client, cluster, pgName)
	if err != nil {
		log.Error(err, "Failed to create PostgreSQL database client")
		return r.updateStatus(ctx, database, false, nil, nil, err)
	}
	defer func() { _ = dbClient.Close() }()

	// Install extensions (only trusted ones and those the Cluster's
	// rolePolicy allows), then create the schemas and apply their grants.
	// Refused extensions and schemas are reported once the rest is done.
	installedExtensions, refusedErr, err := installExtensions(ctx, dbClient, cluster.Spec.RolePolicy, database)
	if err != nil {
		log.Error(err, "Failed to create extension")
		return r.updateStatus(ctx, database, false, installedExtensions, nil, err)
	}
	accessErr = errors.Join(refusedErr, accessErr)
	createdSchemas, refusedErr, err := reconcileSchemas(ctx, dbClient, database)
	if err != nil {
		log.Error(err, "Failed to reconcile schemas")
		return r.updateStatus(ctx, database, false, installedExtensions, createdSchemas, err)
	}
	accessErr = errors.Join(refusedErr, accessErr)

	if err := r.reconcileCredentialsSecret(ctx, database, ownerRole, cluster); err != nil {
		log.Error(err, "Failed to reconcile database credentials secret")
		return r.updateStatus(ctx, database, false, installedExtensions, createdSchemas, err)
	}

	if accessErr != nil {
		return r.updateStatus(ctx, database, false, installedExtensions, createdSchemas, accessErr)
	}

	log.Info("Database reconciled successfully")
	return r.updateStatus(ctx, database, true, installedExtensions, createdSchemas, nil)
}

// installExtensions installs the Database's extensions that the policy
// allows. It returns the installed extensions, an ExtensionNotAllowed error
// listing the refused ones (nil when none) and the error that stopped it.
func installExtensions(ctx context.Context, pg *postgres.Client, policy *postgresv1alpha1.RolePolicySpec,
	database *postgresv1alpha1.Database) (installed []string, refusedErr, err error) {
	installed = make([]string, 0, len(database.Spec.Extensions))
	var refused []string
	for _, ext := range database.Spec.Extensions {
		allowed, err := extensionAllowed(ctx, pg, policy, ext)
		if err != nil {
			return installed, nil, err
		}
		if !allowed {
			refused = append(refused, ext.Name)
			continue
		}
		if err := pg.CreateExtension(ctx, ext.Name, ext.Schema, ext.Version); err != nil {
			return installed, nil, fmt.Errorf("extension %q: %w", ext.Name, err)
		}
		installed = append(installed, ext.Name)
	}
	if len(refused) > 0 {
		refusedErr = &conditionError{reason: ReasonExtensionNotAllowed, err: fmt.Errorf(
			"extensions not installed: %s: not marked trusted by the server (or not available) and not listed in "+
				"the Cluster's spec.rolePolicy.allowedExtensions", strings.Join(refused, ", "))}
	}
	return installed, refusedErr, nil
}

// reconcileSchemas creates the Database's schemas and applies their grants.
// System schemas are refused. It returns the created schemas, a
// SchemaNotAllowed error listing the refused ones (nil when none) and the
// error that stopped it.
func reconcileSchemas(ctx context.Context, pg *postgres.Client, database *postgresv1alpha1.Database) (created []string, refusedErr, err error) {
	created = make([]string, 0, len(database.Spec.Schemas))
	var refused []string
	for _, schema := range database.Spec.Schemas {
		if systemSchemaName(schema.Name) {
			refused = append(refused, schema.Name)
			continue
		}
		if err := pg.CreateSchema(ctx, schema.Name, schema.Owner); err != nil {
			return created, nil, fmt.Errorf("schema %q: %w", schema.Name, err)
		}
		created = append(created, schema.Name)
		for _, grant := range schema.Grants {
			if err := pg.GrantSchemaPrivileges(ctx, schema.Name, grant.Role, grant.Privileges, grant.WithGrantOption); err != nil {
				return created, nil, fmt.Errorf("schema %q, role %q: %w", schema.Name, grant.Role, err)
			}
		}
	}
	if len(refused) > 0 {
		refusedErr = &conditionError{reason: ReasonSchemaNotAllowed, err: fmt.Errorf(
			"system schemas cannot be managed: %s", strings.Join(refused, ", "))}
	}
	return created, refusedErr, nil
}

// databaseOwnershipClient is the subset of *postgres.Client used to create a
// database and keep track of who owns it.
type databaseOwnershipClient interface {
	DatabaseComment(ctx context.Context, name string) (exists bool, comment string, err error)
	CommentOnDatabase(ctx context.Context, name, comment string) error
	CreateDatabase(ctx context.Context, name, owner string, createOnly bool) error
}

// ensureDatabase creates the PostgreSQL database pgName with the Database's
// signed ownership marker, or updates the owner of a database the Database
// owns (see decideOwnership): one recorded in status.databaseName, or one a
// Cluster editor allowlisted in rolePolicy.adoptableDatabases. Any other
// existing database is left alone (reason DatabaseNotManaged). It records
// pgName in status.databaseName once the database exists.
func ensureDatabase(ctx context.Context, pg databaseOwnershipClient, database *postgresv1alpha1.Database,
	cluster *postgresv1alpha1.Cluster, signer markerSigner, pgName, owner string, recorded bool) error {
	marker := signer.marker(markerKindDatabase, database.Name)
	exists, comment, err := pg.DatabaseComment(ctx, pgName)
	if err != nil {
		return err
	}
	decision := decideOwnership(signer, markerKindDatabase, database.Name, exists, comment, recorded,
		cluster.Spec.RolePolicy.AllowsAdoptingDatabase(pgName))
	if decision == notOwned {
		return &conditionError{reason: ReasonDatabaseNotManaged, err: errors.New(notManagedMessage("database", pgName, recorded))}
	}
	// A database pgop decided to create is only ever created: if someone
	// created it in the meantime, pgop does not change its owner.
	if err := pg.CreateDatabase(ctx, pgName, owner, decision == ownedAbsent); err != nil {
		if errors.Is(err, postgres.ErrObjectExists) {
			return &conditionError{reason: ReasonDatabaseNotManaged, err: fmt.Errorf(
				"the PostgreSQL database %s was created by someone else while pgop was creating it; "+
					"pgop does not take it over: %w", pgName, err)}
		}
		return err
	}
	// Record the name (and Cluster) now, so a failed COMMENT is retried as
	// a recorded database and deletion still finds it.
	database.Status.DatabaseName = pgName
	database.Status.ClusterUID = string(cluster.UID)
	if comment != marker {
		return pg.CommentOnDatabase(ctx, pgName, marker)
	}
	return nil
}

// databaseRecorded reports whether database's status records pgName as
// created or adopted on cluster, the Cluster it references now (see
// roleRecorded). A status written before clusterUID existed counts only when
// the Database's credentials Secret exists, is controlled by the Database and
// points at this Cluster's host.
func (r *DatabaseReconciler) databaseRecorded(ctx context.Context, database *postgresv1alpha1.Database,
	cluster *postgresv1alpha1.Cluster, pgName string) (bool, error) {
	if pgName == "" || database.Status.DatabaseName != pgName {
		return false, nil
	}
	if database.Status.ClusterUID != "" {
		return database.Status.ClusterUID == string(cluster.UID), nil
	}
	if database.Spec.Owner == "" {
		return false, nil
	}
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: database.Name + "-" + database.Spec.Owner + "-credentials",
		Namespace: database.Namespace}, secret)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return metav1.IsControlledBy(secret, database) && string(secret.Data[SecretKeyHost]) == clusterHost(cluster), nil
}

// checkDatabaseName returns a ReservedName error for a reserved PostgreSQL
// database name and a DuplicateDatabaseName error when an older Database of
// the same Cluster resolves to the same PostgreSQL name.
func (r *DatabaseReconciler) checkDatabaseName(ctx context.Context, database *postgresv1alpha1.Database, pgName string) error {
	if reason := reservedDatabaseName(pgName); reason != "" {
		return &conditionError{reason: ReasonReservedName, err: errors.New(reason)}
	}
	databases := &postgresv1alpha1.DatabaseList{}
	if err := r.List(ctx, databases, client.InNamespace(database.Namespace)); err != nil {
		return fmt.Errorf("failed to list Databases: %w", err)
	}
	for i := range databases.Items {
		other := &databases.Items[i]
		if other.UID != database.UID && other.Spec.ClusterRef.Name == database.Spec.ClusterRef.Name &&
			other.PostgresName() == pgName && createdBefore(other, database) {
			return &conditionError{reason: ReasonDuplicateDatabaseName, err: fmt.Errorf(
				"the Database %q already manages the PostgreSQL database %s on Cluster %q; pick another databaseName",
				other.Name, pgName, database.Spec.ClusterRef.Name)}
		}
	}
	return nil
}

// dropPostgresDatabase drops the Database's PostgreSQL database during
// deletion: exactly the one recorded in status.databaseName, and only when it
// carries this Database's ownership marker (or no comment, for a database an
// earlier pgop created). Nothing is dropped for a reserved name, for a
// Database that lost a name collision, or for a database someone else owns.
func (r *DatabaseReconciler) dropPostgresDatabase(ctx context.Context, cluster *postgresv1alpha1.Cluster, database *postgresv1alpha1.Database) error {
	log := logf.FromContext(ctx)
	pgName := database.Status.DatabaseName
	recorded, err := r.databaseRecorded(ctx, database, cluster, pgName)
	if err != nil {
		return err
	}
	if pgName == "" || !recorded {
		log.Info("No PostgreSQL database recorded for this Database on this Cluster, nothing to drop")
		return nil
	}
	if err := r.checkDatabaseName(ctx, database, pgName); err != nil {
		log.Info("Not dropping the database", "reason", err.Error())
		return nil
	}
	adminClient, err := newOperatorClient(ctx, r.Client, cluster, defaultDatabaseName)
	if err != nil {
		return err
	}
	defer func() { _ = adminClient.Close() }()
	exists, comment, err := adminClient.DatabaseComment(ctx, pgName)
	if err != nil || !exists {
		return err
	}
	signer, err := loadMarkerSigner(ctx, r.Client, cluster)
	if err != nil {
		log.Info("Ownership-marker key not available; only unmarked databases can be dropped", "reason", err.Error())
	}
	if o := decideOwnership(signer, markerKindDatabase, database.Name, true, comment, true, false); o != owned && o != ownedRemark {
		log.Info("Not dropping a database this Database does not own", "database", pgName)
		return nil
	}
	return adminClient.DropDatabase(ctx, pgName)
}

// extensionClient is the subset of *postgres.Client used to check the
// extension policy.
type extensionClient interface {
	ExtensionTrusted(ctx context.Context, name, version string) (trusted, available bool, err error)
}

// extensionAllowed reports whether pgop may install ext under policy: the
// Cluster lists it in spec.rolePolicy.allowedExtensions, or the server marks
// the requested version (its default version when unset) as trusted, so a
// non-superuser with CREATE on the database could install it as well.
func extensionAllowed(ctx context.Context, pg extensionClient, policy *postgresv1alpha1.RolePolicySpec,
	ext postgresv1alpha1.ExtensionSpec) (bool, error) {
	if policy.AllowsExtension(ext.Name) {
		return true, nil
	}
	trusted, _, err := pg.ExtensionTrusted(ctx, ext.Name, ext.Version)
	return trusted, err
}

// reconcileSettingsAndGrants applies the per-database settings (ALTER
// DATABASE ... SET/RESET; they only affect new sessions, so they are applied
// before the database connection used for extensions is opened) and the
// database-level grants. Both are attempted even when the other fails.
func (r *DatabaseReconciler) reconcileSettingsAndGrants(ctx context.Context, pg databaseGrantClient,
	database *postgresv1alpha1.Database, pgName string) error {
	deleting, err := r.deletingRoleNames(ctx, database)
	if err != nil {
		return err
	}
	return errors.Join(
		reconcileDatabaseSettings(ctx, pg, database, pgName),
		reconcileDatabaseGrants(ctx, pg, database, pgName, deleting),
	)
}

// deletingRoleNames returns the PostgreSQL names of the Roles on the
// Database's cluster that are being deleted, so their grants are not
// re-applied while the Role controller revokes them and drops the role.
func (r *DatabaseReconciler) deletingRoleNames(ctx context.Context, database *postgresv1alpha1.Database) (map[string]bool, error) {
	if len(database.Spec.Grants) == 0 {
		return nil, nil
	}
	roles := &postgresv1alpha1.RoleList{}
	if err := r.List(ctx, roles, client.InNamespace(database.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list Roles: %w", err)
	}
	deleting := map[string]bool{}
	for i := range roles.Items {
		role := &roles.Items[i]
		if role.DeletionTimestamp != nil && role.Spec.ClusterRef.Name == database.Spec.ClusterRef.Name {
			deleting[role.PostgresName()] = true
		}
	}
	return deleting, nil
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
		if ce, ok := errors.AsType[*conditionError](reconcileErr); ok {
			condition.Reason = ce.reason
		}
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
			builder.WithPredicates(clusterConnectionOrPolicyChanged)).
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
