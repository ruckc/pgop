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
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// cert-manager is used through unstructured objects so the operator neither
// depends on its Go module nor fails to start when it is not installed.
var certificateGVK = schema.GroupVersionKind{Group: certManagerGroup, Version: "v1", Kind: "Certificate"}

const (
	certManagerGroup = "cert-manager.io"
	issuerKindIssuer = "Issuer"

	// certManagerCertificateNameAnnotation is set by cert-manager on the
	// Secrets it writes.
	certManagerCertificateNameAnnotation = "cert-manager.io/certificate-name"

	// certManagerUnavailableRequeue is how often a Cluster waiting for
	// cert-manager to be installed is retried (CRD installation is not
	// watched).
	certManagerUnavailableRequeue = time.Minute
	// certificatePendingRequeue is how often a Cluster waiting for
	// cert-manager to issue its certificate is retried, in addition to the
	// Secret and Certificate watches.
	certificatePendingRequeue = 15 * time.Second
)

// certificateName is the cert-manager Certificate created for spec.tls.issuerRef.
func certificateName(cluster *postgresv1alpha1.Cluster) string {
	return cluster.Name + "-server"
}

// certManagerSecretName is the Secret cert-manager writes the certificate to.
func certManagerSecretName(cluster *postgresv1alpha1.Cluster) string {
	return cluster.Name + "-server-tls"
}

func newCertificateObject() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(certificateGVK)
	return u
}

// desiredCertificateSpec is the spec of the Certificate for issuerRef. Values
// use the JSON-decoded types ([]any, map[string]any) so they compare equal
// to what the API server returns.
func desiredCertificateSpec(cluster *postgresv1alpha1.Cluster) map[string]any {
	ref := *cluster.Spec.TLS.IssuerRef
	if ref.Kind == "" {
		ref.Kind = issuerKindIssuer
	}
	if ref.Group == "" {
		ref.Group = certManagerGroup
	}
	// CertManagerIssuerReference has the same JSON shape as cert-manager's
	// ObjectReference (name, kind, group).
	issuerRef, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(&ref)
	names := serverDNSNames(cluster)
	dnsNames := make([]any, 0, len(names))
	for _, n := range names {
		dnsNames = append(dnsNames, n)
	}
	return map[string]any{
		"secretName": certManagerSecretName(cluster),
		"dnsNames":   dnsNames,
		"usages":     []any{"server auth", "digital signature", "key encipherment"},
		"issuerRef":  issuerRef,
		"secretTemplate": map[string]any{
			"labels": map[string]any{
				LabelAppName:      AppNamePostgresql,
				LabelAppInstance:  cluster.Name,
				LabelAppManagedBy: LabelValuePgop,
			},
		},
	}
}

// tlsNotReadyError means the TLS Secret cannot be used (yet). The StatefulSet
// is left unchanged and TLSReady=False is reported with Reason and Message.
type tlsNotReadyError struct {
	Reason  string
	Message string
	// RequeueAfter, when set, retries the Cluster after this delay.
	RequeueAfter time.Duration
}

func (e *tlsNotReadyError) Error() string { return e.Message }

// reconcileCertificate creates or updates the cert-manager Certificate for
// spec.tls.issuerRef. It returns a *tlsNotReadyError while cert-manager is
// not installed or has not written the Secret yet.
func (r *ClusterReconciler) reconcileCertificate(ctx context.Context, cluster *postgresv1alpha1.Cluster) error {
	cert := newCertificateObject()
	err := r.Get(ctx, types.NamespacedName{Name: certificateName(cluster), Namespace: cluster.Namespace}, cert)
	switch {
	case meta.IsNoMatchError(err):
		return &tlsNotReadyError{
			Reason: ReasonCertManagerUnavailable,
			Message: "spec.tls.issuerRef requires cert-manager, but the cert-manager.io/v1 Certificate API is not " +
				"available in this cluster; install cert-manager, or use spec.tls.secretName or the self-managed CA",
			RequeueAfter: certManagerUnavailableRequeue,
		}
	case apierrors.IsNotFound(err):
		cert = newCertificateObject()
		cert.SetName(certificateName(cluster))
		cert.SetNamespace(cluster.Namespace)
		cert.SetLabels(map[string]string{
			LabelAppName:      AppNamePostgresql,
			LabelAppInstance:  cluster.Name,
			LabelAppManagedBy: LabelValuePgop,
		})
		if err := unstructured.SetNestedMap(cert.Object, desiredCertificateSpec(cluster), "spec"); err != nil {
			return err
		}
		if err := controllerutil.SetControllerReference(cluster, cert, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, cert); err != nil {
			return fmt.Errorf("failed to create cert-manager Certificate: %w", err)
		}
	case err != nil:
		return err
	case !metav1.IsControlledBy(cert, cluster):
		return &tlsNotReadyError{
			Reason:  ReasonCertificatePending,
			Message: fmt.Sprintf("cert-manager Certificate %q %s", certificateName(cluster), errTLSSecretConflict),
		}
	default:
		spec, _, _ := unstructured.NestedMap(cert.Object, "spec")
		if spec == nil {
			spec = map[string]any{}
		}
		changed := false
		// Only the fields the operator sets are compared, so defaults added by
		// cert-manager never cause an update loop.
		for k, v := range desiredCertificateSpec(cluster) {
			if !apiequality.Semantic.DeepEqual(spec[k], v) {
				spec[k] = v
				changed = true
			}
		}
		if changed {
			if err := unstructured.SetNestedMap(cert.Object, spec, "spec"); err != nil {
				return err
			}
			if err := r.Update(ctx, cert); err != nil {
				return fmt.Errorf("failed to update cert-manager Certificate: %w", err)
			}
		}
	}

	secret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: certManagerSecretName(cluster), Namespace: cluster.Namespace}, secret)
	if apierrors.IsNotFound(err) {
		return &tlsNotReadyError{
			Reason: ReasonCertificatePending,
			Message: fmt.Sprintf("Waiting for cert-manager to issue Certificate %q: %s",
				certificateName(cluster), certificateReadyMessage(cert)),
			RequeueAfter: certificatePendingRequeue,
		}
	}
	if err != nil {
		return err
	}
	return r.adoptCertManagerSecret(ctx, cluster, secret)
}

// certificateReadyMessage summarises a Certificate's Ready condition.
func certificateReadyMessage(cert *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" {
			continue
		}
		return fmt.Sprintf("Ready=%v (%v: %v)", m["status"], m["reason"], m["message"])
	}
	return "not issued yet"
}

// adoptCertManagerSecret adds a (non-controller) owner reference from the
// Secret cert-manager wrote to the Cluster, so it is garbage collected with
// the Cluster. cert-manager does not delete the Secrets of deleted
// Certificates by default.
func (r *ClusterReconciler) adoptCertManagerSecret(ctx context.Context, cluster *postgresv1alpha1.Cluster, secret *corev1.Secret) error {
	if secret.Annotations[certManagerCertificateNameAnnotation] != certificateName(cluster) || isOwnedBy(secret, cluster) {
		return nil
	}
	base := secret.DeepCopy()
	if err := controllerutil.SetOwnerReference(cluster, secret, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, secret, client.MergeFrom(base))
}

// isOwnedBy reports whether obj has an owner reference (controller or not) to owner.
func isOwnedBy(obj, owner metav1.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == owner.GetUID() {
			return true
		}
	}
	return false
}

// cleanupTLSResources deletes TLS resources the operator created for a TLS
// mode the Cluster no longer uses (the cert-manager Certificate and its
// Secret, the self-managed CA and server Secrets). It runs after the
// StatefulSet stopped referencing them. Only objects owned by the Cluster are
// deleted.
func (r *ClusterReconciler) cleanupTLSResources(ctx context.Context, cluster *postgresv1alpha1.Cluster) error {
	t := cluster.Spec.TLS
	inUse := ""
	if t != nil {
		inUse = tlsSecretName(cluster)
	}

	if (t == nil || t.IssuerRef == nil) && r.certManagerSeen.Load() {
		cert := newCertificateObject()
		err := r.Get(ctx, types.NamespacedName{Name: certificateName(cluster), Namespace: cluster.Namespace}, cert)
		switch {
		case err == nil:
			if metav1.IsControlledBy(cert, cluster) {
				if err := r.Delete(ctx, cert); client.IgnoreNotFound(err) != nil {
					return err
				}
			}
		case meta.IsNoMatchError(err), apierrors.IsNotFound(err):
		default:
			return err
		}
	}

	var stale []string
	if t == nil || t.IssuerRef == nil {
		stale = append(stale, certManagerSecretName(cluster))
	}
	if t == nil || !t.IsSelfManaged() {
		stale = append(stale, selfManagedCASecretName(cluster), selfManagedServerSecretName(cluster))
	}
	for _, name := range stale {
		if name == inUse {
			continue
		}
		s := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, s)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !isOwnedBy(s, cluster) {
			continue
		}
		if err := r.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}
