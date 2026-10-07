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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

const testClusterIssuer = "ClusterIssuer"

// certificateCRD is a minimal stand-in for cert-manager's Certificate CRD.
func certificateCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "certificates.cert-manager.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: certManagerGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "certificates", Singular: "certificate", Kind: "Certificate", ListKind: "CertificateList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: new(true),
				}},
				Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
			}},
		},
	}
}

var _ = Describe("cert-manager TLS", func() {
	cluster := func() *postgresv1alpha1.Cluster {
		return &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
			Spec: postgresv1alpha1.ClusterSpec{TLS: &postgresv1alpha1.ClusterTLSSpec{
				IssuerRef: &postgresv1alpha1.CertManagerIssuerReference{Name: "my-ca"},
			}},
		}
	}

	It("requests a certificate for the Service names from the issuer", func() {
		spec := desiredCertificateSpec(cluster())
		Expect(spec["secretName"]).To(Equal("db-server-tls"))
		dnsNames := make([]any, 0, len(testDNSNames))
		for _, n := range testDNSNames {
			dnsNames = append(dnsNames, n)
		}
		Expect(spec["dnsNames"]).To(Equal(dnsNames))
		Expect(spec["usages"]).To(ContainElement("server auth"))
		Expect(spec["issuerRef"]).To(Equal(map[string]any{"name": "my-ca", "kind": "Issuer", "group": certManagerGroup}))

		c := cluster()
		c.Spec.TLS.IssuerRef.Kind = testClusterIssuer
		Expect(desiredCertificateSpec(c)["issuerRef"]).To(HaveKeyWithValue("kind", testClusterIssuer))
	})

	It("summarises the Certificate Ready condition", func() {
		cert := newCertificateObject()
		Expect(certificateReadyMessage(cert)).To(Equal("not issued yet"))
		Expect(unstructured.SetNestedSlice(cert.Object, []any{map[string]any{
			"type": "Ready", "status": "False", "reason": "Pending", "message": "issuer not found",
		}}, "status", "conditions")).To(Succeed())
		Expect(certificateReadyMessage(cert)).To(Equal("Ready=False (Pending: issuer not found)"))
	})

	Context("on a Cluster", Ordered, func() {
		var (
			ctx  context.Context
			name string
		)
		BeforeEach(func() {
			ctx = context.Background()
			name = fmt.Sprintf("tlscm-%d", time.Now().UnixNano())
		})
		key := func(n string) types.NamespacedName { return types.NamespacedName{Name: n, Namespace: testTLSNamespace} }
		getCluster := func() *postgresv1alpha1.Cluster {
			c := &postgresv1alpha1.Cluster{}
			Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
			return c
		}
		createCluster := func(t *postgresv1alpha1.ClusterTLSSpec) {
			c := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testTLSNamespace},
				Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage, TLS: t},
			}
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			DeferCleanup(func() {
				c := &postgresv1alpha1.Cluster{}
				if k8sClient.Get(ctx, key(name), c) == nil {
					c.SetFinalizers(nil)
					_ = k8sClient.Update(ctx, c)
					_ = k8sClient.Delete(ctx, c)
				}
			})
		}
		tlsCondition := func() *metav1.Condition {
			return meta.FindStatusCondition(getCluster().Status.Conditions, ConditionTypeTLSReady)
		}
		stsExists := func() bool {
			err := k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{})
			if apierrors.IsNotFound(err) {
				return false
			}
			Expect(err).NotTo(HaveOccurred())
			return true
		}
		issuer := func(n string) *postgresv1alpha1.ClusterTLSSpec {
			return &postgresv1alpha1.ClusterTLSSpec{IssuerRef: &postgresv1alpha1.CertManagerIssuerReference{Name: n}}
		}

		It("defaults the issuer kind and group", func() {
			createCluster(issuer("my-ca"))
			ref := getCluster().Spec.TLS.IssuerRef
			Expect(ref.Kind).To(Equal(issuerKindIssuer))
			Expect(ref.Group).To(Equal(certManagerGroup))
		})

		It("reports CertManagerUnavailable when cert-manager is not installed", func() {
			createCluster(issuer("my-ca"))
			r := &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			res, err := r.Reconcile(ctx, reconcileRequest(getCluster()))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(res.RequeueAfter).To(BeNumerically("<=", certManagerUnavailableRequeue))

			cond := tlsCondition()
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonCertManagerUnavailable))
			Expect(cond.Message).To(ContainSubstring("install cert-manager"))
			Expect(stsExists()).To(BeFalse())
			Expect(r.certManagerSeen.Load()).To(BeFalse())
		})

		Context("with the Certificate CRD installed", Ordered, func() {
			BeforeAll(func() {
				_, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
					CRDs: []*apiextensionsv1.CustomResourceDefinition{certificateCRD()},
				})
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() {
					Expect(envtest.UninstallCRDs(cfg, envtest.CRDInstallOptions{
						CRDs: []*apiextensionsv1.CustomResourceDefinition{certificateCRD()},
					})).To(Succeed())
				})
			})

			getCert := func() *unstructured.Unstructured {
				cert := newCertificateObject()
				Expect(k8sClient.Get(ctx, key(name+"-server"), cert)).To(Succeed())
				return cert
			}

			It("creates an owned Certificate, waits for it, then mounts the Secret cert-manager writes", func() {
				createCluster(issuer("my-ca"))
				r := &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				reconcileOnce := func() {
					_, err := r.Reconcile(ctx, reconcileRequest(getCluster()))
					Expect(err).NotTo(HaveOccurred())
				}
				Eventually(func(g Gomega) {
					_, err := r.Reconcile(ctx, reconcileRequest(getCluster()))
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(tlsCondition().Reason).To(Equal(ReasonCertificatePending))
				}).WithTimeout(30 * time.Second).Should(Succeed())
				Expect(r.certManagerSeen.Load()).To(BeTrue())

				cert := getCert()
				Expect(metav1.IsControlledBy(cert, getCluster())).To(BeTrue())
				spec, _, _ := unstructured.NestedMap(cert.Object, "spec")
				Expect(spec["secretName"]).To(Equal(name + "-server-tls"))
				Expect(spec["dnsNames"]).To(ContainElement(name + "." + testTLSNamespace + ".svc.cluster.local"))
				Expect(tlsCondition().Message).To(ContainSubstring("not issued yet"))
				Expect(stsExists()).To(BeFalse())

				By("following issuerRef changes")
				c := getCluster()
				c.Spec.TLS.IssuerRef.Name = "other-ca"
				c.Spec.TLS.IssuerRef.Kind = testClusterIssuer
				Expect(k8sClient.Update(ctx, c)).To(Succeed())
				reconcileOnce()
				ref, _, _ := unstructured.NestedMap(getCert().Object, "spec", "issuerRef")
				Expect(ref).To(Equal(map[string]any{"name": "other-ca", "kind": testClusterIssuer, "group": certManagerGroup}))
				rv := getCert().GetResourceVersion()
				reconcileOnce()
				Expect(getCert().GetResourceVersion()).To(Equal(rv))

				By("cert-manager issuing the certificate")
				issued := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: name + "-server-tls", Namespace: testTLSNamespace,
						Annotations: map[string]string{certManagerCertificateNameAnnotation: name + "-server"},
					},
					Type: corev1.SecretTypeTLS,
					Data: newTestCA().issueFor(name + "." + testTLSNamespace + ".svc.cluster.local"),
				}
				Expect(k8sClient.Create(ctx, issued)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, issued) })
				Expect(r.clustersForTLSSecret(ctx, issued)).To(ContainElement(reconcileRequest(getCluster())))
				reconcileOnce()

				Expect(tlsCondition().Reason).To(Equal(ReasonWaitingForServer))
				sts := &appsv1.StatefulSet{}
				Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
				Expect(sts.Spec.Template.Spec.Volumes[0].Secret.SecretName).To(Equal(name + "-server-tls"))
				s := &corev1.Secret{}
				Expect(k8sClient.Get(ctx, key(name+"-server-tls"), s)).To(Succeed())
				Expect(isOwnedBy(s, getCluster())).To(BeTrue())
				Expect(metav1.GetControllerOf(s)).To(BeNil(), "cert-manager may own the Secret as controller")

				By("switching to the self-managed CA")
				c = getCluster()
				c.Spec.TLS = &postgresv1alpha1.ClusterTLSSpec{}
				Expect(k8sClient.Update(ctx, c)).To(Succeed())
				reconcileOnce()
				Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
				Expect(sts.Spec.Template.Spec.Volumes[0].Secret.SecretName).To(Equal(name + "-server-cert"))
				Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-server"), newCertificateObject()))).To(BeTrue())
				Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-server-tls"), &corev1.Secret{}))).To(BeTrue())
			})

			It("does not take over a Certificate it does not own", func() {
				cert := newCertificateObject()
				cert.SetName(name + "-server")
				cert.SetNamespace(testTLSNamespace)
				Expect(unstructured.SetNestedField(cert.Object, "someone-else", "spec", "secretName")).To(Succeed())
				Expect(k8sClient.Create(ctx, cert)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, cert) })

				createCluster(issuer("my-ca"))
				r := &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				_, err := r.Reconcile(ctx, reconcileRequest(getCluster()))
				Expect(err).NotTo(HaveOccurred())
				Expect(tlsCondition().Reason).To(Equal(ReasonCertificatePending))
				Expect(tlsCondition().Message).To(ContainSubstring("not owned by the Cluster"))
				secretName, _, _ := unstructured.NestedString(getCert().Object, "spec", "secretName")
				Expect(secretName).To(Equal("someone-else"))
			})
		})
	})
})
