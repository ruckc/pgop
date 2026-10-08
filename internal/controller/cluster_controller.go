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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

const (
	clusterFinalizer = "pgop.ruck.io/cluster-finalizer"
)

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder emits Kubernetes Events for the Cluster. Optional: when nil
	// (e.g. in tests that construct the reconciler directly) no events are
	// recorded.
	Recorder events.EventRecorder

	// ProbeServerCertificate returns the DER certificate the server at addr
	// presents. Optional; defaults to a real SSLRequest + TLS handshake. Tests
	// override it since envtest runs no PostgreSQL.
	ProbeServerCertificate func(ctx context.Context, addr, serverName string) ([]byte, error)

	// ReloadServerConfig runs pg_reload_conf() using cfg. Optional; defaults
	// to a real PostgreSQL connection. Tests override it.
	ReloadServerConfig func(ctx context.Context, cfg postgres.ConnectionConfig) error

	// Now returns the current time. Optional; tests override it to exercise
	// certificate renewal and CA rotation.
	Now func() time.Time

	// SelfManagedCertPolicy overrides the lifetimes of the self-managed CA and
	// server certificate. Optional; defaults to defaultSelfManagedCertPolicy.
	SelfManagedCertPolicy *selfManagedCertPolicy

	// certManagerSeen records that the cert-manager Certificate API exists,
	// so cleanupTLSResources only looks for Certificates when they can exist
	// (a lookup of an unknown API triggers API discovery every time).
	certManagerSeen atomic.Bool
}

// now returns r.Now() or time.Now().
func (r *ClusterReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=pgop.ruck.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the Cluster instance
	cluster := &postgresv1alpha1.Cluster{}
	err := r.Get(ctx, req.NamespacedName, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Cluster resource not found, ignoring")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get Cluster")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if cluster.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(cluster, clusterFinalizer) {
			// Perform cleanup
			log.Info("Cleaning up Cluster resources")
			// Resources will be garbage collected due to owner references
			base := cluster.DeepCopy()
			controllerutil.RemoveFinalizer(cluster, clusterFinalizer)
			if err := r.Patch(ctx, cluster, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(cluster, clusterFinalizer) {
		base := cluster.DeepCopy()
		controllerutil.AddFinalizer(cluster, clusterFinalizer)
		if err := r.Patch(ctx, cluster, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Reconcile Secret
	secret, err := r.reconcileSecret(ctx, cluster)
	if err != nil {
		log.Error(err, "Failed to reconcile Secret")
		return r.updateStatus(ctx, cluster, false, err)
	}

	// Reconcile Service
	if err := r.reconcileService(ctx, cluster); err != nil {
		log.Error(err, "Failed to reconcile Service")
		return r.updateStatus(ctx, cluster, false, err)
	}

	// Provision (issuerRef, self-managed) and validate the TLS Secret before
	// touching the pod: a Secret that is missing, incomplete or unusable for
	// verify-full would otherwise roll the pod into a broken (or unreachable)
	// state. On failure the StatefulSet is left exactly as it is and
	// TLSReady=False explains why.
	var material *tlsMaterial
	var tlsRecheckAt time.Time
	if cluster.Spec.TLS != nil {
		tlsRecheckAt, err = r.provisionTLSSecret(ctx, cluster)
		if err == nil {
			if material, err = loadTLSMaterial(ctx, r.Client, cluster, r.now()); err != nil {
				err = &tlsNotReadyError{Reason: ReasonInvalidTLSSecret, Message: err.Error()}
			}
		}
		if notReady, ok := errors.AsType[*tlsNotReadyError](err); ok {
			return r.reportTLSNotReady(ctx, cluster, secret, notReady)
		}
		if err != nil {
			log.Error(err, "Failed to provision the TLS Secret")
			return r.updateStatus(ctx, cluster, false, err)
		}
	} else {
		meta.RemoveStatusCondition(&cluster.Status.Conditions, ConditionTypeTLSReady)
		cluster.Status.TLSSecretHash = ""
	}

	// The pg_hba ConfigMap must exist before a pod that mounts it starts.
	if err := r.reconcileHBAConfigMap(ctx, cluster); err != nil {
		log.Error(err, "Failed to reconcile pg_hba ConfigMap")
		return r.updateStatus(ctx, cluster, false, err)
	}

	// Reconcile StatefulSet
	if err := r.reconcileStatefulSet(ctx, cluster, secret); err != nil {
		log.Error(err, "Failed to reconcile StatefulSet")
		return r.updateStatus(ctx, cluster, false, err)
	}

	// Remove a no longer needed pg_hba ConfigMap and TLS resources only after
	// the StatefulSet stopped referencing them.
	if err := r.cleanupHBAConfigMap(ctx, cluster); err != nil {
		log.Error(err, "Failed to clean up pg_hba ConfigMap")
		return r.updateStatus(ctx, cluster, false, err)
	}
	if err := r.cleanupTLSResources(ctx, cluster); err != nil {
		log.Error(err, "Failed to clean up TLS resources")
		return r.updateStatus(ctx, cluster, false, err)
	}

	// Check if StatefulSet is ready
	ready, err := r.isStatefulSetReady(ctx, cluster)
	if err != nil {
		log.Error(err, "Failed to check StatefulSet status")
		return r.updateStatus(ctx, cluster, false, err)
	}

	tlsPending := false
	if material != nil {
		tlsPending = r.reconcileTLSState(ctx, cluster, material, ready)
	}

	// Converge the client-facing connection info last, once the TLS state of
	// this reconcile is known.
	var caPEM []byte
	if material != nil {
		caPEM = material.CAPEM
	}
	if err := r.convergeSecretConnectionInfo(ctx, cluster, secret, caPEM); err != nil {
		log.Error(err, "Failed to update credentials Secret connection info")
		return r.updateStatus(ctx, cluster, false, err)
	}

	result, err := r.updateStatus(ctx, cluster, ready, nil)
	if err != nil {
		return result, err
	}
	if tlsPending {
		result.RequeueAfter = shorterRequeue(result.RequeueAfter, 10*time.Second)
	}
	if !tlsRecheckAt.IsZero() {
		// Self-managed certificates: come back when they are due for renewal.
		result.RequeueAfter = shorterRequeue(result.RequeueAfter,
			min(max(tlsRecheckAt.Sub(r.now()), time.Second), maxTLSRecheckInterval))
	}
	return result, nil
}

// maxTLSRecheckInterval caps how long the operator waits before looking at
// self-managed certificates again, so a renewal is never missed by much.
const maxTLSRecheckInterval = 12 * time.Hour

// shorterRequeue returns the shorter of two RequeueAfter values, where 0 means
// no requeue.
func shorterRequeue(cur, d time.Duration) time.Duration {
	if cur == 0 || d < cur {
		return d
	}
	return cur
}

// provisionTLSSecret makes sure the server certificate Secret exists: for
// issuerRef it reconciles the cert-manager Certificate, for the self-managed
// CA it issues and renews the certificates. With spec.tls.secretName there is
// nothing to provision. It returns when self-managed certificates must be
// looked at again (zero otherwise), and a *tlsNotReadyError when the Secret
// cannot be provisioned yet.
func (r *ClusterReconciler) provisionTLSSecret(ctx context.Context, cluster *postgresv1alpha1.Cluster) (time.Time, error) {
	t := cluster.Spec.TLS
	switch {
	case t.SecretName != "":
		return time.Time{}, nil
	case t.IssuerRef != nil:
		err := r.reconcileCertificate(ctx, cluster)
		notReady, isNotReady := errors.AsType[*tlsNotReadyError](err)
		if err == nil || (isNotReady && notReady.Reason != ReasonCertManagerUnavailable) {
			r.certManagerSeen.Store(true)
		}
		return time.Time{}, err
	default:
		return r.reconcileSelfManagedTLS(ctx, cluster)
	}
}

// reportTLSNotReady records that the TLS Secret cannot be used, leaving the
// StatefulSet untouched, and still converges the credentials Secret.
func (r *ClusterReconciler) reportTLSNotReady(ctx context.Context, cluster *postgresv1alpha1.Cluster, secret *corev1.Secret, notReady *tlsNotReadyError) (ctrl.Result, error) {
	logf.FromContext(ctx).Info("TLS Secret is not usable; leaving the StatefulSet unchanged",
		"reason", notReady.Reason, "message", notReady.Message)
	prev := meta.FindStatusCondition(cluster.Status.Conditions, ConditionTypeTLSReady)
	changed := prev == nil || prev.Reason != notReady.Reason || prev.Message != notReady.Message
	r.setTLSCondition(cluster, metav1.ConditionFalse, notReady.Reason, notReady.Message)
	cluster.Status.TLSSecretHash = ""
	if r.Recorder != nil && changed {
		eventType := corev1.EventTypeWarning
		if notReady.Reason == ReasonCertificatePending {
			eventType = corev1.EventTypeNormal
		}
		r.Recorder.Eventf(cluster, nil, eventType, notReady.Reason, "ValidateTLSSecret", "%s", notReady.Message)
	}
	ready, err := r.isStatefulSetReady(ctx, cluster)
	if err != nil {
		return r.updateStatus(ctx, cluster, false, err)
	}
	if err := r.convergeSecretConnectionInfo(ctx, cluster, secret, nil); err != nil {
		return r.updateStatus(ctx, cluster, false, err)
	}
	// The TLS Secret is watched; RequeueAfter covers what is not (cert-manager
	// being installed).
	result, err := r.updateStatus(ctx, cluster, ready, nil)
	if err == nil && notReady.RequeueAfter > 0 {
		result.RequeueAfter = shorterRequeue(result.RequeueAfter, notReady.RequeueAfter)
	}
	return result, err
}

// setTLSCondition sets the TLSReady condition on the in-memory Cluster.
func (r *ClusterReconciler) setTLSCondition(cluster *postgresv1alpha1.Cluster, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeTLSReady,
		Status:             status,
		ObservedGeneration: cluster.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// reconcileTLSState checks that the running server presents the certificate
// from the TLS Secret and sets TLSReady accordingly. When the server still
// presents an older certificate (the Secret was rotated), it asks PostgreSQL to
// reload its configuration, which re-reads the certificate files without a
// restart. Kubelet propagates Secret updates into the pod with a delay, so this
// is retried until the server presents the new certificate. Returns whether
// TLS is still pending and the Cluster should be requeued.
func (r *ClusterReconciler) reconcileTLSState(ctx context.Context, cluster *postgresv1alpha1.Cluster, material *tlsMaterial, ready bool) bool {
	if !ready {
		r.setTLSCondition(cluster, metav1.ConditionFalse, ReasonWaitingForServer, "Waiting for the PostgreSQL pod to become ready")
		return true
	}

	host := clusterHost(cluster)
	probe := r.ProbeServerCertificate
	if probe == nil {
		probe = probeServerCertificate
	}
	presented, err := probe(ctx, net.JoinHostPort(host, strconv.Itoa(int(clusterPort(cluster)))), host)
	if err != nil {
		r.setTLSCondition(cluster, metav1.ConditionFalse, ReasonWaitingForServer,
			fmt.Sprintf("Server is not serving TLS yet: %v", err))
		return true
	}

	if bytes.Equal(presented, material.Leaf.Raw) {
		r.setTLSCondition(cluster, metav1.ConditionTrue, ReasonTLSActive,
			fmt.Sprintf("Server presents the certificate from Secret %q", tlsSecretName(cluster)))
		cluster.Status.TLSSecretHash = material.Hash
		return false
	}

	// The server presents a different (older) certificate.
	if !certVerifies(presented, material.CAPEM, host, r.now()) {
		// It does not chain to the current CA bundle: the CA was replaced
		// without an overlap (the self-managed CA always overlaps), or the old
		// certificate expired. The server cannot be reached over verify-full
		// to reload it, and the operator never falls back to an unverified
		// connection, so the pod is restarted to load the new certificate.
		msg := "Server presents a certificate that the current CA does not verify (the CA changed); " +
			"restarting the PostgreSQL pod to load the new certificate"
		if err := r.restartForTLS(ctx, cluster, material.Hash); err != nil {
			msg = fmt.Sprintf("Server presents a certificate that the current CA does not verify (the CA changed) "+
				"and restarting the pod failed: %v", err)
		}
		r.setTLSCondition(cluster, metav1.ConditionFalse, ReasonCertificateReloading, msg)
		return true
	}

	// Reload over a connection verified against the current CA bundle.
	msg := "Server presents an outdated certificate; requested a configuration reload"
	if err := r.reloadServerConfig(ctx, cluster, material.CAPEM); err != nil {
		msg = fmt.Sprintf("Server presents an outdated certificate and the reload failed: %v", err)
	}
	r.setTLSCondition(cluster, metav1.ConditionFalse, ReasonCertificateReloading, msg)
	return true
}

// restartForTLS restarts the PostgreSQL pod by setting the tls-restart pod
// template annotation to hash (the certificate material to load). The pod is
// restarted at most once per certificate: when the annotation already has
// this value nothing is done, so a server that keeps presenting an
// unexpected certificate never causes a restart loop.
func (r *ClusterReconciler) restartForTLS(ctx context.Context, cluster *postgresv1alpha1.Cluster, hash string) error {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, sts); err != nil {
		return err
	}
	if sts.Spec.Template.Annotations[AnnotationTLSRestart] == hash {
		return nil
	}
	base := sts.DeepCopy()
	if sts.Spec.Template.Annotations == nil {
		sts.Spec.Template.Annotations = map[string]string{}
	}
	sts.Spec.Template.Annotations[AnnotationTLSRestart] = hash
	if err := r.Patch(ctx, sts, client.MergeFrom(base)); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Restarting the PostgreSQL pod to load a certificate from a new CA")
	if r.Recorder != nil {
		r.Recorder.Eventf(cluster, sts, corev1.EventTypeNormal, ReasonCertificateReloading, "RestartForTLS",
			"Restarting the PostgreSQL pod: the new certificate is from a different CA and cannot be loaded with a reload")
	}
	return nil
}

// reloadServerConfig runs pg_reload_conf() as the operator over a verify-full
// connection using caPEM.
func (r *ClusterReconciler) reloadServerConfig(ctx context.Context, cluster *postgresv1alpha1.Cluster, caPEM []byte) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: cluster.Name + "-credentials", Namespace: cluster.Namespace}, secret); err != nil {
		return fmt.Errorf("failed to get credentials secret: %w", err)
	}
	cfg := postgres.ConnectionConfig{
		Host:        clusterHost(cluster),
		Port:        clusterPort(cluster),
		User:        string(secret.Data[SecretKeyUsername]),
		Password:    string(secret.Data[SecretKeyPassword]),
		Database:    defaultDatabaseName,
		SSLMode:     postgres.SSLModeVerifyFull,
		RootCertPEM: caPEM,
	}
	if r.ReloadServerConfig != nil {
		return r.ReloadServerConfig(ctx, cfg)
	}
	pgClient, err := postgres.NewClient(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = pgClient.Close() }()
	return pgClient.ReloadConfig(ctx)
}

// reconcileHBAConfigMap creates or updates the operator-managed pg_hba
// ConfigMap while spec.tls.requireTLS is in effect.
func (r *ClusterReconciler) reconcileHBAConfigMap(ctx context.Context, cluster *postgresv1alpha1.Cluster) error {
	if cluster.Spec.TLS == nil || !cluster.Spec.TLS.IsRequireTLS() {
		return nil
	}
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: hbaConfigMapName(cluster), Namespace: cluster.Namespace}, cm)
	if err == nil {
		if cm.Data[hbaFileName] == managedPgHBA {
			return nil
		}
		cm.Data = map[string]string{hbaFileName: managedPgHBA}
		return r.Update(ctx, cm)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hbaConfigMapName(cluster),
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				LabelAppName:      AppNamePostgresql,
				LabelAppInstance:  cluster.Name,
				LabelAppManagedBy: LabelValuePgop,
			},
		},
		Data: map[string]string{hbaFileName: managedPgHBA},
	}
	if err := controllerutil.SetControllerReference(cluster, cm, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, cm)
}

// cleanupHBAConfigMap deletes the operator-managed pg_hba ConfigMap once TLS
// or requireTLS is turned off. Without hba_file the server falls back to the
// pg_hba.conf in its data directory (the image default), which was never
// modified.
func (r *ClusterReconciler) cleanupHBAConfigMap(ctx context.Context, cluster *postgresv1alpha1.Cluster) error {
	if cluster.Spec.TLS != nil && cluster.Spec.TLS.IsRequireTLS() {
		return nil
	}
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: hbaConfigMapName(cluster), Namespace: cluster.Namespace}, cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(cm, cluster) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, cm))
}

func (r *ClusterReconciler) reconcileSecret(ctx context.Context, cluster *postgresv1alpha1.Cluster) (*corev1.Secret, error) {
	secretName := cluster.Name + "-credentials"
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: cluster.Namespace}, secret)
	if err == nil {
		// Connection info is converged at the end of the reconcile, once the
		// TLS state is known (convergeSecretConnectionInfo).
		return secret, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	// Generate random password
	password, err := generatePassword(32)
	if err != nil {
		return nil, fmt.Errorf("failed to generate password: %w", err)
	}

	data := map[string][]byte{
		SecretKeyUsername: []byte(DefaultOperatorUsername),
		SecretKeyPassword: []byte(password),
		SecretKeyDatabase: []byte(defaultDatabaseName),
	}
	applyConnectionInfo(data, clusterHost(cluster), clusterPort(cluster), defaultDatabaseName, clientTLSFor(cluster, nil))

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				LabelAppName:      AppNamePostgresql,
				LabelAppInstance:  cluster.Name,
				LabelAppManagedBy: LabelValuePgop,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}

	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return nil, err
	}

	if err := r.Create(ctx, secret); err != nil {
		return nil, err
	}

	return secret, nil
}

// convergeSecretConnectionInfo updates the connection-info keys of the
// credentials Secret (host, port, sslmode, uri, ca.crt) when the Cluster's
// port or TLS state changes. username, password and database are never
// touched, so this never regenerates the password. caPEM is the CA from the
// validated TLS Secret (nil when TLS is off or the Secret is invalid).
func (r *ClusterReconciler) convergeSecretConnectionInfo(ctx context.Context, cluster *postgresv1alpha1.Cluster, secret *corev1.Secret, caPEM []byte) error {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	database := string(secret.Data[SecretKeyDatabase])
	if database == "" {
		database = defaultDatabaseName
	}
	if !applyConnectionInfo(secret.Data, clusterHost(cluster), clusterPort(cluster), database, clientTLSFor(cluster, caPEM)) {
		return nil
	}
	return r.Update(ctx, secret)
}

func (r *ClusterReconciler) reconcileService(ctx context.Context, cluster *postgresv1alpha1.Cluster) error {
	serviceName := cluster.Name
	service := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: cluster.Namespace}, service)
	if err == nil {
		return r.convergeService(ctx, cluster, service)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	port := cluster.Spec.Port
	if port == 0 {
		port = 5432
	}

	service = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				LabelAppName:      AppNamePostgresql,
				LabelAppInstance:  cluster.Name,
				LabelAppManagedBy: LabelValuePgop,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				LabelAppName:     AppNamePostgresql,
				LabelAppInstance: cluster.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       AppNamePostgresql,
					Port:       port,
					TargetPort: intstr.FromInt32(port),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	if err := controllerutil.SetControllerReference(cluster, service, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, service)
}

// convergeService updates an existing Service's port when the Cluster's port
// changes. Selector and Type are fixed for the lifetime of the cluster, so
// only Ports is compared.
func (r *ClusterReconciler) convergeService(ctx context.Context, cluster *postgresv1alpha1.Cluster, service *corev1.Service) error {
	port := cluster.Spec.Port
	if port == 0 {
		port = 5432
	}

	desiredPorts := []corev1.ServicePort{
		{
			Name:       AppNamePostgresql,
			Port:       port,
			TargetPort: intstr.FromInt32(port),
			Protocol:   corev1.ProtocolTCP,
		},
	}

	if apiequality.Semantic.DeepEqual(service.Spec.Ports, desiredPorts) {
		return nil
	}

	service.Spec.Ports = desiredPorts
	return r.Update(ctx, service)
}

func (r *ClusterReconciler) reconcileStatefulSet(ctx context.Context, cluster *postgresv1alpha1.Cluster, secret *corev1.Secret) error {
	stsName := cluster.Name

	// Set defaults
	image := cluster.Spec.Image
	if image == "" {
		image = DefaultPostgresImage
	}

	// Resolve the data-directory layout for this image's major version. This
	// determines both where the PVC is mounted and the explicit PGDATA, so the
	// data directory always lands inside the PVC regardless of the image default.
	layout, err := resolvePostgresLayout(cluster)
	if err != nil {
		return err
	}
	replicas := cluster.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	port := cluster.Spec.Port
	if port == 0 {
		port = 5432
	}

	labels := map[string]string{
		LabelAppName:      AppNamePostgresql,
		LabelAppInstance:  cluster.Name,
		LabelAppManagedBy: LabelValuePgop,
	}

	container := buildPostgresContainer(secret, image, port, cluster.Spec.Resources, layout)

	// TLS (spec.tls): extra volumes, mounts and server args. All empty when
	// TLS is disabled, so StatefulSets of non-TLS Clusters are unchanged.
	volumes, tlsMounts := postgresTLSVolumes(cluster)
	container.VolumeMounts = append(container.VolumeMounts, tlsMounts...)
	container.Args = postgresTLSArgs(cluster.Spec.TLS)

	sts := &appsv1.StatefulSet{}
	err = r.Get(ctx, types.NamespacedName{Name: stsName, Namespace: cluster.Namespace}, sts)
	if err == nil {
		return r.convergeStatefulSet(ctx, sts, replicas, labels, container, volumes, desiredPVCRetentionPolicy(cluster))
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	if err := r.detectExistingVolume(ctx, cluster); err != nil {
		return err
	}

	storageSize := cluster.Spec.Storage.Size
	if storageSize == "" {
		storageSize = "1Gi"
	}

	sts = &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stsName,
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:                          cluster.Name,
			Replicas:                             &replicas,
			PersistentVolumeClaimRetentionPolicy: desiredPVCRetentionPolicy(cluster),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelAppName:     AppNamePostgresql,
					LabelAppInstance: cluster.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					SecurityContext: postgresPodSecurityContext(),
					Containers:      []corev1.Container{container},
					Volumes:         volumes,
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: dataVolumeName,
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse(storageSize),
							},
						},
						StorageClassName: cluster.Spec.Storage.StorageClassName,
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(cluster, sts, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, sts)
}

// desiredPVCRetentionPolicy maps the Cluster's storage.retainPolicy onto the
// StatefulSet persistentVolumeClaimRetentionPolicy. whenDeleted follows the
// Cluster setting (Retain by default); whenScaled is always Retain so that a
// scale-down can never destroy data. With whenDeleted=Delete the StatefulSet
// controller adds an ownerReference from the StatefulSet to each PVC, so the
// existing Cluster -> StatefulSet ownership cascade removes the PVC through
// normal garbage collection.
func desiredPVCRetentionPolicy(cluster *postgresv1alpha1.Cluster) *appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy {
	whenDeleted := appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	if cluster.Spec.Storage.RetainPolicy == postgresv1alpha1.StorageRetainPolicyDelete {
		whenDeleted = appsv1.DeletePersistentVolumeClaimRetentionPolicyType
	}
	return &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: whenDeleted,
		WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
	}
}

// dataPVCName returns the name of the PVC the StatefulSet controller creates
// for the given Cluster's first (and only) replica.
func dataPVCName(cluster *postgresv1alpha1.Cluster) string {
	return fmt.Sprintf("%s-%s-0", dataVolumeName, cluster.Name)
}

// detectExistingVolume is called right before the StatefulSet is created. If
// the data PVC already exists (typically retained from a previously deleted
// Cluster of the same name), the new StatefulSet will adopt it and PostgreSQL
// starts on the old data directory. That is surfaced via the ExistingVolume
// condition and a Warning event. The condition is only computed here: once the
// StatefulSet exists the operator can no longer tell the two cases apart.
// The postStart password-sync hook already handles the credential mismatch,
// so this is informational only.
func (r *ClusterReconciler) detectExistingVolume(ctx context.Context, cluster *postgresv1alpha1.Cluster) error {
	pvcName := dataPVCName(cluster)
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: cluster.Namespace}, pvc)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if apierrors.IsNotFound(err) {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeExistingVolume,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: cluster.Generation,
			Reason:             ReasonNewVolume,
			Message:            fmt.Sprintf("StatefulSet created without a pre-existing data PVC; %s will be provisioned fresh", pvcName),
		})
		return nil
	}

	msg := fmt.Sprintf("Starting on pre-existing PersistentVolumeClaim %s; PostgreSQL will reuse its existing data", pvcName)
	logf.FromContext(ctx).Info(msg, "pvc", pvcName)
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeExistingVolume,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cluster.Generation,
		Reason:             ReasonPreExistingPVC,
		Message:            msg,
	})
	if r.Recorder != nil {
		r.Recorder.Eventf(cluster, pvc, corev1.EventTypeWarning, ReasonPreExistingPVC, "CreateStatefulSet", "%s", msg)
	}
	return nil
}

// postgresPodSecurityContext returns the pod-level SecurityContext applied to
// every PostgreSQL StatefulSet pod.
func postgresPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: func() *bool { b := true; return &b }(),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
		RunAsUser:  func() *int64 { i := int64(999); return &i }(),
		RunAsGroup: func() *int64 { i := int64(999); return &i }(),
		FSGroup:    func() *int64 { i := int64(999); return &i }(),
	}
}

// buildPostgresContainer builds the desired postgresql container spec from
// the Cluster's current settings. It is used both when creating a new
// StatefulSet and when converging an existing one onto spec changes (image
// bumps, resource changes, etc).
func buildPostgresContainer(secret *corev1.Secret, image string, port int32, resources corev1.ResourceRequirements, layout postgresLayout) corev1.Container {
	return corev1.Container{
		Name:  AppNamePostgresql,
		Image: image,
		Ports: []corev1.ContainerPort{
			{
				Name:          AppNamePostgresql,
				ContainerPort: port,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: func() *bool { b := false; return &b }(),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
		Env: []corev1.EnvVar{
			{
				Name: "POSTGRES_USER",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: secret.Name,
						},
						Key: "username",
					},
				},
			},
			{
				Name: "POSTGRES_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: secret.Name,
						},
						Key: "password",
					},
				},
			},
			{
				// Pin PGDATA explicitly so the data directory
				// does not depend on the image's own default,
				// which differs between PG <=17 and PG >=18.
				Name:  "PGDATA",
				Value: layout.PGDATA,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      dataVolumeName,
				MountPath: layout.MountPath,
			},
		},
		Lifecycle: operatorPasswordSyncLifecycle(),
		Resources: resources,
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"pg_isready", "-U", DefaultOperatorUsername},
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"pg_isready", "-U", DefaultOperatorUsername},
				},
			},
			InitialDelaySeconds: 30,
			PeriodSeconds:       10,
		},
	}
}

// convergeStatefulSet patches an existing StatefulSet onto the current
// Cluster spec (image, resources, replicas, port, the data-directory layout,
// TLS volumes and args, and the PVC retention policy), and backfills the
// password-sync lifecycle hook on StatefulSets
// created before it existed (see https://github.com/ruckc/pgop/issues/7).
// It only issues an Update when something actually differs, so reconciling
// an already-converged cluster is a no-op and does not trigger spurious
// pod rollouts.
func (r *ClusterReconciler) convergeStatefulSet(ctx context.Context, sts *appsv1.StatefulSet, replicas int32, labels map[string]string, desired corev1.Container, volumes []corev1.Volume, retention *appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy) error {
	changed := false

	// Pod volumes are entirely operator-owned (the data volume comes from
	// volumeClaimTemplates), so they are replaced wholesale. Semantic.DeepEqual
	// treats nil and empty as equal, so non-TLS StatefulSets never diff here.
	if !apiequality.Semantic.DeepEqual(sts.Spec.Template.Spec.Volumes, volumes) {
		sts.Spec.Template.Spec.Volumes = volumes
		changed = true
	}

	// Compare the two fields explicitly: the API server defaults the struct
	// (to Retain/Retain), so a nil-vs-defaulted DeepEqual would diff forever.
	if cur := sts.Spec.PersistentVolumeClaimRetentionPolicy; cur == nil ||
		cur.WhenDeleted != retention.WhenDeleted || cur.WhenScaled != retention.WhenScaled {
		sts.Spec.PersistentVolumeClaimRetentionPolicy = retention
		changed = true
	}

	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != replicas {
		sts.Spec.Replicas = &replicas
		changed = true
	}

	if !apiequality.Semantic.DeepEqual(sts.Spec.Template.Labels, labels) {
		sts.Spec.Template.Labels = labels
		changed = true
	}

	if !apiequality.Semantic.DeepEqual(sts.Spec.Template.Spec.SecurityContext, postgresPodSecurityContext()) {
		sts.Spec.Template.Spec.SecurityContext = postgresPodSecurityContext()
		changed = true
	}

	if len(sts.Spec.Template.Spec.Containers) == 0 {
		sts.Spec.Template.Spec.Containers = []corev1.Container{desired}
		changed = true
	} else if convergeContainer(&sts.Spec.Template.Spec.Containers[0], desired) {
		changed = true
	}

	if !changed {
		return nil
	}

	return r.Update(ctx, sts)
}

// convergeContainer overwrites the fields of existing that pgop derives from
// the Cluster spec (image, args, ports, env, volume mounts, resources, security
// context, probes, and the password-sync lifecycle hook) with the desired
// values. Fields the API server defaults on its own (ImagePullPolicy,
// TerminationMessagePath, etc.) are left untouched so reconciling an
// unchanged cluster does not perpetually diff against server-side defaults.
// Returns whether anything was actually changed.
func convergeContainer(existing *corev1.Container, desired corev1.Container) bool {
	changed := false

	if existing.Image != desired.Image {
		existing.Image = desired.Image
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.Args, desired.Args) {
		existing.Args = desired.Args
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.Ports, desired.Ports) {
		existing.Ports = desired.Ports
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.Env, desired.Env) {
		existing.Env = desired.Env
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.VolumeMounts, desired.VolumeMounts) {
		existing.VolumeMounts = desired.VolumeMounts
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.Resources, desired.Resources) {
		existing.Resources = desired.Resources
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.SecurityContext, desired.SecurityContext) {
		existing.SecurityContext = desired.SecurityContext
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.ReadinessProbe, desired.ReadinessProbe) {
		existing.ReadinessProbe = desired.ReadinessProbe
		changed = true
	}
	if !apiequality.Semantic.DeepEqual(existing.LivenessProbe, desired.LivenessProbe) {
		existing.LivenessProbe = desired.LivenessProbe
		changed = true
	}
	// Converge the lifecycle unconditionally so that changes to the
	// password-sync postStart hook (e.g. bug fixes) roll out to existing
	// StatefulSets on operator upgrade rather than only applying to newly
	// created ones.
	if !apiequality.Semantic.DeepEqual(existing.Lifecycle, desired.Lifecycle) {
		existing.Lifecycle = desired.Lifecycle
		changed = true
	}

	return changed
}

// operatorPasswordSyncLifecycle returns a postStart hook that converges the
// in-database operator password to the value in the credentials Secret
// (injected as $POSTGRES_PASSWORD) on every pod start.
//
// The official postgres image only applies POSTGRES_PASSWORD during initdb, so
// when a pod restarts on a retained PVC after the credentials Secret has been
// regenerated (e.g. Cluster delete/recreate or Helm redeploy) the database
// keeps the old password and the operator can no longer authenticate, failing
// every Role/Database reconcile with 28P01. Local socket connections use trust
// auth, so this ALTER ROLE needs no password itself.
// See https://github.com/ruckc/pgop/issues/7.
func operatorPasswordSyncLifecycle() *corev1.Lifecycle {
	script := `until pg_isready -q -U "$POSTGRES_USER" -d postgres 2>/dev/null; do sleep 1; done; ` +
		`psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d postgres ` +
		`-c "ALTER ROLE \"$POSTGRES_USER\" WITH PASSWORD '$POSTGRES_PASSWORD'"`
	return &corev1.Lifecycle{
		PostStart: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"sh", "-c", script},
			},
		},
	}
}

func (r *ClusterReconciler) isStatefulSetReady(ctx context.Context, cluster *postgresv1alpha1.Cluster) (bool, error) {
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, sts)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	replicas := cluster.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	return sts.Status.ReadyReplicas == replicas, nil
}

func (r *ClusterReconciler) updateStatus(ctx context.Context, cluster *postgresv1alpha1.Cluster, ready bool, reconcileErr error) (ctrl.Result, error) {
	// Update status fields
	cluster.Status.Ready = ready
	cluster.Status.SecretName = cluster.Name + "-credentials"

	port := cluster.Spec.Port
	if port == 0 {
		port = 5432
	}
	cluster.Status.Endpoint = fmt.Sprintf("%s.%s.svc.cluster.local:%d", cluster.Name, cluster.Namespace, port)

	// Set condition
	condition := metav1.Condition{
		Type:               ConditionTypeAvailable,
		ObservedGeneration: cluster.Generation,
		LastTransitionTime: metav1.Now(),
	}

	if ready {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "ClusterReady"
		condition.Message = "PostgreSQL cluster is ready"
	} else if reconcileErr != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = ReasonReconcileError
		condition.Message = reconcileErr.Error()
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ClusterNotReady"
		condition.Message = "PostgreSQL cluster is not ready yet"
	}

	meta.SetStatusCondition(&cluster.Status.Conditions, condition)

	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue if not ready
	if !ready && reconcileErr == nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{}, reconcileErr
}

func generatePassword(length int) (string, error) {
	buf := make([]byte, length/2)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&postgresv1alpha1.Cluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.clustersForTLSSecret))
	// Watch cert-manager Certificates only when cert-manager is installed: a
	// watch on a missing API would keep the controller from starting. Without
	// it, issuerRef Clusters still converge through the Secret watch and
	// periodic requeues.
	if _, err := mgr.GetRESTMapper().RESTMapping(certificateGVK.GroupKind(), certificateGVK.Version); err == nil {
		r.certManagerSeen.Store(true)
		b = b.Owns(newCertificateObject())
	} else {
		logf.Log.WithName("cluster-controller").Info(
			"cert-manager Certificate API not found; spec.tls.issuerRef will report CertManagerUnavailable until it is installed")
	}
	return b.Named("cluster").Complete(r)
}

// clustersForTLSSecret maps a Secret to the Clusters in its namespace whose
// server certificate it holds (see tlsSecretName), so creating, fixing or
// rotating the TLS Secret re-validates the certificate and reloads the server.
func (r *ClusterReconciler) clustersForTLSSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	clusters := &postgresv1alpha1.ClusterList{}
	if err := r.List(ctx, clusters, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Clusters for TLS Secret", "secret", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range clusters.Items {
		if clusters.Items[i].Spec.TLS != nil && tlsSecretName(&clusters.Items[i]) == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&clusters.Items[i])})
		}
	}
	return requests
}
