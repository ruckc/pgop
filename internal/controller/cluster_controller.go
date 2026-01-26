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
		return secret, nil // Secret already exists
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

	username := "pgop_operator"
	host := fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace)

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "postgresql",
				"app.kubernetes.io/instance":   cluster.Name,
				"app.kubernetes.io/managed-by": "pgop",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"username": username,
			"password": password,
			"host":     host,
			"port":     fmt.Sprintf("%d", port),
			"database": "postgres",
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

func (r *ClusterReconciler) reconcileService(ctx context.Context, cluster *postgresv1alpha1.Cluster) error {
	serviceName := cluster.Name
	service := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: cluster.Namespace}, service)
	if err == nil {
		return nil // Service already exists
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
				"app.kubernetes.io/name":       "postgresql",
				"app.kubernetes.io/instance":   cluster.Name,
				"app.kubernetes.io/managed-by": "pgop",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/name":     "postgresql",
				"app.kubernetes.io/instance": cluster.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "postgresql",
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

func (r *ClusterReconciler) reconcileStatefulSet(ctx context.Context, cluster *postgresv1alpha1.Cluster, secret *corev1.Secret) error {
	stsName := cluster.Name
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: stsName, Namespace: cluster.Namespace}, sts)
	if err == nil {
		// TODO: Update StatefulSet if needed
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	// Set defaults
	image := cluster.Spec.Image
	if image == "" {
		image = "postgres:18"
	}
	replicas := cluster.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	port := cluster.Spec.Port
	if port == 0 {
		port = 5432
	}
	storageSize := cluster.Spec.Storage.Size
	if storageSize == "" {
		storageSize = "1Gi"
	}

	labels := map[string]string{
		"app.kubernetes.io/name":       "postgresql",
		"app.kubernetes.io/instance":   cluster.Name,
		"app.kubernetes.io/managed-by": "pgop",
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
					"app.kubernetes.io/name":     "postgresql",
					"app.kubernetes.io/instance": cluster.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: func() *bool { b := true; return &b }(),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
						RunAsUser:  func() *int64 { i := int64(999); return &i }(),
						RunAsGroup: func() *int64 { i := int64(999); return &i }(),
						FSGroup:    func() *int64 { i := int64(999); return &i }(),
					},
					Containers: []corev1.Container{
						{
							Name:  "postgresql",
							Image: image,
							Ports: []corev1.ContainerPort{
								{
									Name:          "postgresql",
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
							},
							VolumeMounts: []corev1.VolumeMount{

								{
									Name:      "data",
									MountPath: "/var/lib/postgresql",
									// SubPath:   "pgdata",
								},
							},
							Resources: cluster.Spec.Resources,
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"pg_isready", "-U", "pgop_operator"},
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"pg_isready", "-U", "pgop_operator"},
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
							},
						},
					},
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
		Type:               "Available",
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
