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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

const (
	clusterFinalizer = "pgop.ruck.io/cluster-finalizer"
)

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pgop.ruck.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgop.ruck.io,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

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
			controllerutil.RemoveFinalizer(cluster, clusterFinalizer)
			if err := r.Update(ctx, cluster); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(cluster, clusterFinalizer) {
		controllerutil.AddFinalizer(cluster, clusterFinalizer)
		if err := r.Update(ctx, cluster); err != nil {
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

	// Reconcile StatefulSet
	if err := r.reconcileStatefulSet(ctx, cluster, secret); err != nil {
		log.Error(err, "Failed to reconcile StatefulSet")
		return r.updateStatus(ctx, cluster, false, err)
	}

	// Check if StatefulSet is ready
	ready, err := r.isStatefulSetReady(ctx, cluster)
	if err != nil {
		log.Error(err, "Failed to check StatefulSet status")
		return r.updateStatus(ctx, cluster, false, err)
	}

	return r.updateStatus(ctx, cluster, ready, nil)
}

func (r *ClusterReconciler) reconcileSecret(ctx context.Context, cluster *postgresv1alpha1.Cluster) (*corev1.Secret, error) {
	secretName := cluster.Name + "-credentials"
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: cluster.Namespace}, secret)
	if err == nil {
		return r.convergeSecret(ctx, cluster, secret)
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	// Generate random password
	password, err := generatePassword(32)
	if err != nil {
		return nil, fmt.Errorf("failed to generate password: %w", err)
	}

	port := cluster.Spec.Port
	if port == 0 {
		port = 5432
	}

	username := DefaultOperatorUsername
	host := fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace)

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
		StringData: map[string]string{
			SecretKeyUsername: username,
			SecretKeyPassword: password,
			SecretKeyHost:     host,
			SecretKeyPort:     fmt.Sprintf("%d", port),
			SecretKeyDatabase: "postgres",
		},
	}

	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return nil, err
	}

	if err := r.Create(ctx, secret); err != nil {
		return nil, err
	}

	return secret, nil
}

// convergeSecret updates the connection-info fields (host, port) of an
// existing credentials Secret when the Cluster's port changes. It only ever
// sets those two keys in StringData: the Secret API merges StringData into
// Data on write, so the existing username/password/database keys are left
// completely untouched — this never regenerates or exposes the password.
func (r *ClusterReconciler) convergeSecret(ctx context.Context, cluster *postgresv1alpha1.Cluster, secret *corev1.Secret) (*corev1.Secret, error) {
	port := cluster.Spec.Port
	if port == 0 {
		port = 5432
	}

	host := fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace)
	wantPort := fmt.Sprintf("%d", port)

	if string(secret.Data[SecretKeyHost]) == host && string(secret.Data[SecretKeyPort]) == wantPort {
		return secret, nil
	}

	secret.StringData = map[string]string{
		SecretKeyHost: host,
		SecretKeyPort: wantPort,
	}
	if err := r.Update(ctx, secret); err != nil {
		return nil, err
	}

	return secret, nil
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

	sts := &appsv1.StatefulSet{}
	err = r.Get(ctx, types.NamespacedName{Name: stsName, Namespace: cluster.Namespace}, sts)
	if err == nil {
		return r.convergeStatefulSet(ctx, sts, replicas, labels, container)
	}
	if !apierrors.IsNotFound(err) {
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
			ServiceName: cluster.Name,
			Replicas:    &replicas,
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
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "data",
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
				Name:      "data",
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
// Cluster spec (image, resources, replicas, port, and the data-directory
// layout), and backfills the password-sync lifecycle hook on StatefulSets
// created before it existed (see https://github.com/ruckc/pgop/issues/7).
// It only issues an Update when something actually differs, so reconciling
// an already-converged cluster is a no-op and does not trigger spurious
// pod rollouts.
func (r *ClusterReconciler) convergeStatefulSet(ctx context.Context, sts *appsv1.StatefulSet, replicas int32, labels map[string]string, desired corev1.Container) error {
	changed := false

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
// the Cluster spec (image, ports, env, volume mounts, resources, security
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
	// Backfill only: never remove a lifecycle hook that's already present,
	// but always ensure the password-sync postStart hook exists.
	if existing.Lifecycle == nil || existing.Lifecycle.PostStart == nil {
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
	bytes := make([]byte, length/2)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&postgresv1alpha1.Cluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Named("cluster").
		Complete(r)
}
