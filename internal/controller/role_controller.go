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
	"slices"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=databases,verbs=get;list;watch

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
				if res, err := r.dropPostgresRole(ctx, cluster, role); err != nil || !res.IsZero() {
					return res, err
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

	// A reserved name (postgres, pgop_*, pg_*) is never touched, and of two
	// Roles of a Cluster that resolve to the same PostgreSQL name only the
	// older one is reconciled.
	pgName := role.PostgresName()
	if err := r.checkRoleName(ctx, role, pgName); err != nil {
		return r.updateStatus(ctx, role, "", err)
	}

	// Get the referenced cluster
	cluster, err := r.getCluster(ctx, role)
	if err != nil {
		log.Error(err, "Failed to get Cluster")
		return r.updateStatus(ctx, role, "", err)
	}
	policy := cluster.Spec.RolePolicy

	// Check if cluster is ready
	if !cluster.Status.Ready {
		log.Info("Cluster not ready, requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Get operator credentials
	pgClient, err := newOperatorClient(ctx, r.Client, cluster, defaultDatabaseName)
	if err != nil {
		log.Error(err, "Failed to create PostgreSQL client")
		return r.updateStatus(ctx, role, "", err)
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

	// Only a role this Role created (it carries its signed ownership marker)
	// is altered: taking over an existing role would hand its password to
	// the Role writer.
	signer, marker, createOnly, err := r.roleOwnership(ctx, pgClient, cluster, role, pgName, policy)
	if err != nil {
		log.Info("Not reconciling the role", "reason", err.Error())
		return r.updateStatus(ctx, role, "", err)
	}

	// Resolve the password. NOLOGIN (group) roles get no password and no
	// credentials Secret.
	now := time.Now()
	var dp desiredPassword
	var existingSecret *corev1.Secret
	if role.Spec.IsLogin() {
		if existingSecret, err = r.getCredentialsSecret(ctx, role, cluster); err != nil {
			log.Error(err, "Failed to get credentials secret")
			return r.updateStatus(ctx, role, "", err)
		}
		if dp, existingSecret, err = r.resolvePassword(ctx, role, existingSecret, now); err != nil {
			log.Error(err, "Failed to resolve role password")
			return r.updateStatus(ctx, role, "", err)
		}
	}

	// Create or update the role. It is never a superuser, and gets the
	// privileged attributes (createRole, replication, bypassRLS) only when
	// the Cluster's rolePolicy allows all those it requests; otherwise it
	// gets none of them and policyErr reports why. The password (sent as a
	// SCRAM-SHA-256 verifier, never in plaintext) is only sent to an existing
	// role when it differs from the one last applied.
	opts, policyErr := desiredRoleOptions(&role.Spec, policy)
	opts.Password = dp.value
	opts.KeepExistingPassword = dp.applied && !dp.force
	opts.Comment = marker
	// A role pgop decided to create is only ever created: if someone created
	// it in the meantime, CREATE fails instead of pgop altering their role.
	opts.CreateOnly = createOnly

	if err := createOrAlterRole(ctx, pgClient, pgName, opts); err != nil {
		log.Error(err, "Failed to create/update role")
		return r.updateStatus(ctx, role, role.Status.SecretName, err)
	}
	// The settings ledger only counts for the role it was written for.
	clearStaleRoleSettings(role, pgName, string(cluster.UID))
	// Record the PostgreSQL name that now exists so deletion drops exactly it.
	role.Status.RoleName = pgName
	role.Status.ClusterUID = string(cluster.UID)

	// Write the credentials Secret after PostgreSQL accepted the password: if
	// the write fails, the next reconcile sets the password again (or, for a
	// rotation, rotates again) instead of handing out a password PostgreSQL
	// does not know.
	var secretName string
	if role.Spec.IsLogin() {
		if secretName, err = r.reconcileCredentialsSecret(ctx, role, cluster, existingSecret, dp.value, !opts.KeepExistingPassword); err != nil {
			log.Error(err, "Failed to reconcile credentials secret")
			return r.updateStatus(ctx, role, role.Status.SecretName, err)
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

	// Handle role memberships, parameter privileges and settings.
	pendingSettings, err := r.reconcileGrantsAndSettings(ctx, pgClient, role, cluster, pgName, policy, signer, policyErr)
	if err != nil {
		log.Error(err, "Failed to reconcile role")
		return r.updateStatus(ctx, role, secretName, err)
	}

	log.Info("Role reconciled successfully")
	return r.readyResult(ctx, role, secretName, pendingSettings)
}

// reconcileGrantsAndSettings reconciles the role's memberships, parameter
// privileges and settings, and joins their errors with policyErr (the
// role's attributes violate the Cluster's policy). Memberships and
// parameter privileges are reconciled even under a policy violation, so
// forbidden memberships pgop granted earlier are revoked; settings are only
// applied to a role within the policy (a Role reporting RolePolicyViolation
// gets none, and keeps its ledger). pending lists the databases
// databaseSettings wait for.
func (r *RoleReconciler) reconcileGrantsAndSettings(ctx context.Context, pgClient *postgres.Client, role *postgresv1alpha1.Role,
	cluster *postgresv1alpha1.Cluster, pgName string, policy *postgresv1alpha1.RolePolicySpec, signer markerSigner,
	policyErr error) (pending []string, err error) {
	var settingsErr error
	if policyErr == nil {
		pending, settingsErr = reconcileRoleSettings(ctx, pgClient, role, pgName)
	}
	return pending, errors.Join(policyErr, r.reconcileRoleGrants(ctx, pgClient, role, cluster, pgName, policy, signer), settingsErr)
}

// readyResult records a ready Role and schedules the next reconcile: the
// next password rotation, or sooner while databaseSettings wait for the
// databases in pendingSettings.
func (r *RoleReconciler) readyResult(ctx context.Context, role *postgresv1alpha1.Role, secretName string,
	pendingSettings []string) (ctrl.Result, error) {
	note := ""
	if len(pendingSettings) > 0 {
		note = "databaseSettings are pending until these databases exist: " + strings.Join(pendingSettings, ", ")
	}
	result, err := r.writeStatus(ctx, role, true, secretName, nil, note)
	if err != nil {
		return result, err
	}
	if next := nextRotationIn(role, time.Now()); next > 0 {
		result.RequeueAfter = next
	}
	if len(pendingSettings) > 0 && (result.RequeueAfter == 0 || result.RequeueAfter > pendingSettingsRequeue) {
		result.RequeueAfter = pendingSettingsRequeue
	}
	return result, nil
}

// pendingSettingsRequeue is how often a Role whose databaseSettings name a
// missing database is reconciled again (the Database watch usually wakes it
// up sooner).
const pendingSettingsRequeue = 30 * time.Second

// dropPostgresRole drops the Role's PostgreSQL role during deletion. Privileges
// held by the role block DROP ROLE, so it first revokes the parameter grants
// pgop made and every database and schema privilege the role holds on the
// cluster (with CASCADE: the role is going away). Anything else that still
// depends on the role (objects it owns, table privileges) is reported as an
// Available=False condition with reason RoleDropBlocked and PostgreSQL's
// list of dependents, and the drop is retried periodically. A non-zero
// result or an error means the role has not been dropped yet.
func (r *RoleReconciler) dropPostgresRole(ctx context.Context, cluster *postgresv1alpha1.Cluster, role *postgresv1alpha1.Role) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	pgClient, err := newOperatorClient(ctx, r.Client, cluster, defaultDatabaseName)
	if err != nil {
		log.Error(err, "Failed to create PostgreSQL client during deletion")
		return ctrl.Result{}, err
	}
	defer func() { _ = pgClient.Close() }()
	// Drop exactly the role recorded in status on this Cluster: the one this
	// Role created or took over there. Without it (pgop refused to take over
	// an existing role, never got to create one, or recorded it on a Cluster
	// that has since been re-created) nothing is dropped, so deleting a Role
	// can never drop a role pgop does not manage.
	pgName := role.Status.RoleName
	recorded, err := r.roleRecorded(ctx, role, cluster, pgName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pgName == "" || !recorded {
		log.Info("No PostgreSQL role recorded for this Role on this Cluster, nothing to drop")
		return ctrl.Result{}, nil
	}
	if reservedRoleName(pgName) != "" {
		log.Info("Not dropping a reserved PostgreSQL role", "role", pgName)
		return ctrl.Result{}, nil
	}
	if err := r.checkRoleName(ctx, role, pgName); err != nil {
		log.Info("Not dropping a role another Role manages", "reason", err.Error())
		return ctrl.Result{}, nil
	}
	exists, comment, err := pgClient.RoleComment(ctx, pgName)
	if err != nil || !exists {
		return ctrl.Result{}, err
	}
	// Drop only a role carrying this Role's signed marker, or (as it is
	// recorded in status) one without a comment or with an unsigned v1
	// marker, which an earlier pgop created. Without the key only the
	// latter can be recognized.
	signer, err := loadMarkerSigner(ctx, r.Client, cluster)
	if err != nil {
		log.Info("Ownership-marker key not available; only unmarked roles can be dropped", "reason", err.Error())
	}
	if o := decideOwnership(signer, markerKindRole, role.Name, true, comment, true, false); o != owned && o != ownedRemark {
		log.Info("Not dropping a role this Role does not own", "role", pgName)
		return ctrl.Result{}, nil
	}

	// REVOKE is idempotent, so a retry after a partial failure is safe.
	if err := revokeManagedParameterGrants(ctx, pgClient, role, pgName); err != nil {
		log.Error(err, "Failed to revoke parameter grants")
		return ctrl.Result{}, err
	}
	if err := pgClient.RevokeAllDatabasePrivileges(ctx, pgName); err != nil {
		log.Error(err, "Failed to revoke database privileges")
		return ctrl.Result{}, err
	}
	dbs, err := pgClient.DatabasesWithSchemaPrivileges(ctx, pgName)
	if err != nil {
		return ctrl.Result{}, err
	}
	// A database that does not accept connections (its owner can turn them
	// off) or cannot be reached is skipped instead of holding up the
	// deletion: if privileges there still block DROP ROLE, the condition
	// names the database.
	var skipped []string
	for _, db := range dbs {
		if !db.AllowConns {
			skipped = append(skipped, db.Name+" (does not allow connections)")
			continue
		}
		if err := revokeSchemaPrivilegesIn(ctx, r.Client, cluster, db.Name, pgName); err != nil {
			log.Error(err, "Failed to revoke schema privileges; skipping the database", "database", db.Name)
			skipped = append(skipped, db.Name+" ("+err.Error()+")")
		}
	}

	err = pgClient.DropRole(ctx, pgName)
	if depErr, ok := errors.AsType[*postgres.DependentObjectsError](err); ok {
		msg := depErr.Error()
		if len(skipped) > 0 {
			msg += "; schema privileges could not be revoked in: " + strings.Join(skipped, ", ")
		}
		log.Info("Role cannot be dropped yet", "reason", msg)
		meta.SetStatusCondition(&role.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeAvailable,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: role.Generation,
			Reason:             ReasonRoleDropBlocked,
			Message:            msg,
		})
		if statusErr := r.Status().Update(ctx, role); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if err != nil {
		log.Error(err, "Failed to drop role")
	}
	return ctrl.Result{}, err
}

// revokeSchemaPrivilegesIn revokes every schema privilege role holds in the
// database db.
func revokeSchemaPrivilegesIn(ctx context.Context, c client.Client, cluster *postgresv1alpha1.Cluster, db, role string) error {
	dbClient, err := newOperatorClient(ctx, c, cluster, db)
	if err != nil {
		return err
	}
	defer func() { _ = dbClient.Close() }()
	return dbClient.RevokeAllSchemaPrivileges(ctx, role)
}

// createOrAlterRole runs CreateRole and reports a role someone else created
// while a create-only CREATE ROLE was running with reason RoleNotManaged.
func createOrAlterRole(ctx context.Context, pg *postgres.Client, pgName string, opts postgres.RoleOptions) error {
	err := pg.CreateRole(ctx, pgName, opts)
	if errors.Is(err, postgres.ErrObjectExists) {
		return &conditionError{reason: ReasonRoleNotManaged, err: fmt.Errorf(
			"the PostgreSQL role %s was created by someone else while pgop was creating it; "+
				"pgop does not take it over: %w", pgName, err)}
	}
	return err
}

// roleOwnershipClient is the subset of *postgres.Client used to decide
// whether a Role owns its PostgreSQL role.
type roleOwnershipClient interface {
	RoleComment(ctx context.Context, name string) (exists bool, comment string, err error)
	MembershipClosure(ctx context.Context, name string) ([]postgres.ReachableRole, error)
}

// checkRoleOwnership decides whether role may create or alter the
// PostgreSQL role pgName (see decideOwnership). It returns the ownership
// marker to store with the role ("" when it already carries it), or an error
// with reason RoleNotManaged when the role exists and is neither recorded in
// status.roleName nor allowlisted in the Cluster's rolePolicy.adoptableRoles,
// or RolePolicyViolation when an allowlisted role fails the privilege checks
// (adoptionProblem).
func (r *RoleReconciler) checkRoleOwnership(ctx context.Context, pg roleOwnershipClient, role *postgresv1alpha1.Role,
	cluster *postgresv1alpha1.Cluster, pgName string, policy *postgresv1alpha1.RolePolicySpec, signer markerSigner,
	recorded bool) (marker string, createOnly bool, err error) {
	exists, comment, err := pg.RoleComment(ctx, pgName)
	if err != nil {
		return "", false, err
	}
	switch decideOwnership(signer, markerKindRole, role.Name, exists, comment, recorded, policy.AllowsAdoptingRole(pgName)) {
	case owned:
		return "", false, nil
	case ownedAbsent:
		return signer.marker(markerKindRole, role.Name), true, nil
	case ownedRemark:
		return signer.marker(markerKindRole, role.Name), false, nil
	case adoptable:
		if err := r.checkRoleAdoption(ctx, pg, role, cluster, pgName, policy, signer); err != nil {
			return "", false, err
		}
		return signer.marker(markerKindRole, role.Name), false, nil
	}
	return "", false, &conditionError{reason: ReasonRoleNotManaged, err: errors.New(notManagedMessage("role", pgName, recorded))}
}

// roleRecorded reports whether role's status records pgName as created or
// adopted on cluster, the Cluster it references now: status.clusterUID must
// be that Cluster's UID (a Cluster deleted and re-created under the same name
// has a new one). A status written before clusterUID existed counts only
// when the Role's credentials Secret for this Cluster
// (<cluster>-<role>-credentials) exists and is controlled by the Role.
func (r *RoleReconciler) roleRecorded(ctx context.Context, role *postgresv1alpha1.Role, cluster *postgresv1alpha1.Cluster,
	pgName string) (bool, error) {
	if pgName == "" || role.Status.RoleName != pgName {
		return false, nil
	}
	if role.Status.ClusterUID != "" {
		return role.Status.ClusterUID == string(cluster.UID), nil
	}
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: credentialsSecretName(role, cluster), Namespace: role.Namespace}, secret)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return metav1.IsControlledBy(secret, role), nil
}

// roleOwnership loads the Cluster's marker signer and runs
// checkRoleOwnership.
func (r *RoleReconciler) roleOwnership(ctx context.Context, pg roleOwnershipClient, cluster *postgresv1alpha1.Cluster,
	role *postgresv1alpha1.Role, pgName string, policy *postgresv1alpha1.RolePolicySpec) (markerSigner, string, bool, error) {
	signer, err := loadMarkerSigner(ctx, r.Client, cluster)
	if err != nil {
		return markerSigner{}, "", false, err
	}
	recorded, err := r.roleRecorded(ctx, role, cluster, pgName)
	if err != nil {
		return markerSigner{}, "", false, err
	}
	marker, createOnly, err := r.checkRoleOwnership(ctx, pg, role, cluster, pgName, policy, signer, recorded)
	return signer, marker, createOnly, err
}

// checkRoleAdoption checks an existing role a Cluster editor allowlisted for
// adoption: it must not be a superuser or reach roles the policy does not
// allow (its privileged attributes are altered down).
func (r *RoleReconciler) checkRoleAdoption(ctx context.Context, pg roleOwnershipClient, role *postgresv1alpha1.Role,
	cluster *postgresv1alpha1.Cluster, pgName string, policy *postgresv1alpha1.RolePolicySpec, signer markerSigner) error {
	closure, err := pg.MembershipClosure(ctx, pgName)
	if err != nil {
		return err
	}
	managed, err := r.managedRoles(ctx, role, cluster, signer)
	if err != nil {
		return err
	}
	if p := adoptionProblem(pgName, closure, policy, managed); p != "" {
		return &conditionError{reason: ReasonRolePolicyViolation, err: errors.New(p)}
	}
	return nil
}

// clusterRoles returns the Roles of role's Cluster (in its namespace), oldest
// first.
func (r *RoleReconciler) clusterRoles(ctx context.Context, role *postgresv1alpha1.Role) ([]*postgresv1alpha1.Role, error) {
	roles := &postgresv1alpha1.RoleList{}
	if err := r.List(ctx, roles, client.InNamespace(role.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list Roles: %w", err)
	}
	out := make([]*postgresv1alpha1.Role, 0, len(roles.Items))
	for i := range roles.Items {
		if roles.Items[i].Spec.ClusterRef.Name == role.Spec.ClusterRef.Name {
			out = append(out, &roles.Items[i])
		}
	}
	slices.SortFunc(out, func(a, b *postgresv1alpha1.Role) int {
		if createdBefore(a, b) {
			return -1
		}
		if createdBefore(b, a) {
			return 1
		}
		return 0
	})
	return out, nil
}

// managedRoles returns the PostgreSQL roles the Roles of role's Cluster
// manage, with the marker each must carry (the oldest Role wins a name).
func (r *RoleReconciler) managedRoles(ctx context.Context, role *postgresv1alpha1.Role, cluster *postgresv1alpha1.Cluster,
	signer markerSigner) (managedRoles, error) {
	roles, err := r.clusterRoles(ctx, role)
	if err != nil {
		return nil, err
	}
	out := make(managedRoles, len(roles))
	for _, other := range roles {
		// Only a role the Role has actually created or adopted on this
		// Cluster counts: a Role whose spec merely names a role (for example
		// re-created with roleName pointing at a role someone else built and
		// marked) does not make that role managed.
		name := other.PostgresName()
		if other.Status.RoleName != name || other.Status.ClusterUID != string(cluster.UID) {
			continue
		}
		if _, taken := out[name]; !taken {
			out[name] = signer.marker(markerKindRole, other.Name)
		}
	}
	return out, nil
}

// checkRoleName returns a ReservedName error for a reserved PostgreSQL name
// and a DuplicateRoleName error when an older Role of the same Cluster
// resolves to the same PostgreSQL name.
func (r *RoleReconciler) checkRoleName(ctx context.Context, role *postgresv1alpha1.Role, pgName string) error {
	if reason := reservedRoleName(pgName); reason != "" {
		return &conditionError{reason: ReasonReservedName, err: errors.New(reason)}
	}
	roles, err := r.clusterRoles(ctx, role)
	if err != nil {
		return err
	}
	for _, other := range roles {
		if other.UID != role.UID && other.PostgresName() == pgName && createdBefore(other, role) {
			return &conditionError{reason: ReasonDuplicateRoleName, err: fmt.Errorf(
				"the Role %q already manages the PostgreSQL role %s on Cluster %q; pick another roleName",
				other.Name, pgName, role.Spec.ClusterRef.Name)}
		}
	}
	return nil
}

// createdBefore reports whether a was created before b (by creation time,
// then name), deciding which of two resources with the same PostgreSQL name
// wins.
func createdBefore(a, b metav1.Object) bool {
	ta, tb := a.GetCreationTimestamp(), b.GetCreationTimestamp()
	if !ta.Equal(&tb) {
		return ta.Before(&tb)
	}
	return a.GetName() < b.GetName()
}

// reconcileRoleGrants brings the role's memberships (grant, update options,
// revoke removed and forbidden ones) and its privileges on configuration
// parameters (PostgreSQL 15+) to the declared state. A problem with one does
// not hold up the other.
func (r *RoleReconciler) reconcileRoleGrants(ctx context.Context, pgClient *postgres.Client, role *postgresv1alpha1.Role,
	cluster *postgresv1alpha1.Cluster, pgName string,
	policy *postgresv1alpha1.RolePolicySpec, signer markerSigner) error {
	managed, err := r.managedRoles(ctx, role, cluster, signer)
	if err != nil {
		return err
	}
	return errors.Join(
		reconcileMemberships(ctx, pgClient, role, pgName, policy, managed),
		reconcileParameterGrants(ctx, pgClient, role, pgName),
	)
}

// credentialsSecretName returns the name of the Role's credentials Secret.
func credentialsSecretName(role *postgresv1alpha1.Role, cluster *postgresv1alpha1.Cluster) string {
	return cluster.Name + "-" + role.Name + "-credentials"
}

// getCredentialsSecret returns the Role's credentials Secret, or nil when it
// does not exist yet. A Secret missing from the cache is looked up uncached
// before it is reported missing: the cache may not show a Secret created by
// the previous reconcile yet, and treating it as missing would generate and
// set a new password that then cannot be stored (the create fails).
func (r *RoleReconciler) getCredentialsSecret(ctx context.Context, role *postgresv1alpha1.Role, cluster *postgresv1alpha1.Cluster) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: credentialsSecretName(role, cluster), Namespace: role.Namespace}
	err := r.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) && r.APIReader != nil {
		err = r.APIReader.Get(ctx, key, secret)
	}
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
func (r *RoleReconciler) reconcileCredentialsSecret(ctx context.Context, role *postgresv1alpha1.Role, cluster *postgresv1alpha1.Cluster, existing *corev1.Secret, password string, passwordSent bool) (string, error) {
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
	// Secret is re-read (uncached when possible) and the change applied again.
	// passwordSent tells whether this reconcile set password in PostgreSQL.
	// If it did not, password was taken from the Secret this reconcile read;
	// should the re-read show a different password (the cached Secret was
	// stale, e.g. it predates a rotation), writing password would put the
	// Secret out of step with PostgreSQL for good. Give up instead and let
	// the next reconcile start from the current Secret.
	basePassword := string(existing.Data[SecretKeyPassword])
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	secret := existing.DeepCopy()
	return secretName, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if !passwordSent && string(secret.Data[SecretKeyPassword]) != basePassword {
			return errCredentialsSecretChanged
		}
		if !convergeRoleSecret(secret, role, password, host, port, t) {
			return nil
		}
		err := r.Update(ctx, secret)
		if apierrors.IsConflict(err) {
			if getErr := reader.Get(ctx, client.ObjectKeyFromObject(secret), secret); getErr != nil {
				return getErr
			}
		}
		return err
	})
}

// errCredentialsSecretChanged reports that the credentials Secret's password
// changed while the reconcile that read it was running.
var errCredentialsSecretChanged = errors.New("the credentials Secret's password changed concurrently; retrying from the current Secret")

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

// updateStatus records a Role that is not ready (yet), with reconcileErr as
// the reason when set.
func (r *RoleReconciler) updateStatus(ctx context.Context, role *postgresv1alpha1.Role, secretName string, reconcileErr error) (ctrl.Result, error) {
	return r.writeStatus(ctx, role, false, secretName, reconcileErr, "")
}

// writeStatus persists the Role's status and Available condition; note is
// appended to the message of a ready Role's condition.
func (r *RoleReconciler) writeStatus(ctx context.Context, role *postgresv1alpha1.Role, ready bool, secretName string,
	reconcileErr error, note string) (ctrl.Result, error) {
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
		if note != "" {
			condition.Message += "; " + note
		}
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
			builder.WithPredicates(clusterConnectionOrPolicyChanged)).
		Watches(&postgresv1alpha1.Database{},
			handler.EnqueueRequestsFromMapFunc(r.rolesForDatabase),
			builder.WithPredicates(databaseAppeared)).
		Named("role").
		Complete(r)
}

// rolesForCluster maps a Cluster to the Roles in its namespace that reference
// it, so their credentials Secrets follow port and TLS changes and their
// attributes and memberships follow spec.rolePolicy.
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

// databaseAppeared passes Database events that can make a database a Role's
// databaseSettings name start (or stop) existing: creation, deletion and
// changes of status.ready or status.databaseName.
var databaseAppeared = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldDB, okOld := e.ObjectOld.(*postgresv1alpha1.Database)
		newDB, okNew := e.ObjectNew.(*postgresv1alpha1.Database)
		if !okOld || !okNew {
			return false
		}
		return oldDB.Status.Ready != newDB.Status.Ready || oldDB.Status.DatabaseName != newDB.Status.DatabaseName
	},
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// rolesForDatabase maps a Database to the Roles of its Cluster whose
// databaseSettings name its PostgreSQL database, so settings pending on a
// missing database are applied once it is created.
func (r *RoleReconciler) rolesForDatabase(ctx context.Context, obj client.Object) []reconcile.Request {
	db, ok := obj.(*postgresv1alpha1.Database)
	if !ok {
		return nil
	}
	roles := &postgresv1alpha1.RoleList{}
	if err := r.List(ctx, roles, client.InNamespace(db.Namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Roles for Database", "database", db.Name)
		return nil
	}
	pgName := db.PostgresName()
	var requests []reconcile.Request
	for i := range roles.Items {
		role := &roles.Items[i]
		if role.Spec.ClusterRef.Name != db.Spec.ClusterRef.Name {
			continue
		}
		if slices.ContainsFunc(role.Spec.DatabaseSettings, func(s postgresv1alpha1.RoleDatabaseSettings) bool {
			return s.Database == pgName
		}) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(role)})
		}
	}
	return requests
}
