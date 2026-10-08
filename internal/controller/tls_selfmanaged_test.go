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
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// shortPolicy keeps the same proportions as the default policy at a scale
// where tests can step through a full CA lifetime.
var shortPolicy = selfManagedCertPolicy{
	CAValidity:        100 * time.Hour,
	CAPrepareBefore:   30 * time.Hour,
	CASwitchBefore:    10 * time.Hour,
	ServerValidity:    9 * time.Hour,
	ServerRenewBefore: 3 * time.Hour,
}

// testDNSNames are the server certificate SANs of Cluster "db" in namespace "ns".
var testDNSNames = []string{"db.ns.svc.cluster.local", "db.ns.svc", "db.ns", "db"}

func parsePEMCert(p []byte) *x509.Certificate {
	b, _ := pem.Decode(p)
	Expect(b).NotTo(BeNil())
	c, err := x509.ParseCertificate(b.Bytes)
	Expect(err).NotTo(HaveOccurred())
	return c
}

var _ = Describe("Self-managed TLS", func() {
	const host = "db.ns.svc.cluster.local"
	names := testDNSNames

	Context("planSelfManagedTLS", func() {
		t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

		plan := func(ca, server map[string][]byte, now time.Time) *selfManagedTLSPlan {
			p, err := planSelfManagedTLS(ca, server, names, now, shortPolicy)
			Expect(err).NotTo(HaveOccurred())
			return p
		}
		bundleLen := func(p *selfManagedTLSPlan) int { return len(parseCertBundle(p.CA[TLSSecretKeyCA])) }
		leaf := func(p *selfManagedTLSPlan) *x509.Certificate { return parsePEMCert(p.Server[TLSSecretKeyCert]) }

		It("creates a CA and a server certificate usable with verify-full", func() {
			p := plan(nil, nil, t0)
			Expect(p.CA).To(HaveKey(TLSSecretKeyKey))
			Expect(p.CA).NotTo(HaveKey(caSecretKeyNextCert))
			Expect(bundleLen(p)).To(Equal(1))
			Expect(p.Server[TLSSecretKeyCA]).To(Equal(p.CA[TLSSecretKeyCA]))
			Expect(p.Server).NotTo(HaveKey(caSecretKeyNextKey))

			m, err := parseTLSMaterial(p.Server, host, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(m.Leaf.DNSNames).To(Equal(names))
			Expect(m.Leaf.NotAfter).To(Equal(t0.Add(shortPolicy.ServerValidity)))
			Expect(p.NextCheck).To(Equal(t0.Add(shortPolicy.ServerValidity - shortPolicy.ServerRenewBefore)))
			Expect(parsePEMCert(p.CA[TLSSecretKeyCert]).IsCA).To(BeTrue())
		})

		It("keeps valid certificates unchanged", func() {
			p := plan(nil, nil, t0)
			again := plan(p.CA, p.Server, t0.Add(time.Hour))
			Expect(again.CA).To(Equal(p.CA))
			Expect(again.Server).To(Equal(p.Server))
		})

		It("renews the server certificate before it expires, keeping the CA", func() {
			p := plan(nil, nil, t0)
			renewed := plan(p.CA, p.Server, p.NextCheck)
			Expect(renewed.CA).To(Equal(p.CA))
			Expect(renewed.Server[TLSSecretKeyCert]).NotTo(Equal(p.Server[TLSSecretKeyCert]))
			Expect(leaf(renewed).NotAfter).To(Equal(p.NextCheck.Add(shortPolicy.ServerValidity)))
		})

		It("reissues the server certificate when the DNS names change", func() {
			p := plan(nil, nil, t0)
			q, err := planSelfManagedTLS(p.CA, p.Server, []string{"other.ns.svc.cluster.local"}, t0, shortPolicy)
			Expect(err).NotTo(HaveOccurred())
			Expect(parsePEMCert(q.Server[TLSSecretKeyCert]).DNSNames).To(Equal([]string{"other.ns.svc.cluster.local"}))
		})

		It("regenerates an invalid CA and the server certificate it issued", func() {
			p := plan(nil, nil, t0)
			broken := map[string][]byte{TLSSecretKeyCert: []byte("junk"), TLSSecretKeyKey: p.CA[TLSSecretKeyKey]}
			q := plan(broken, p.Server, t0)
			Expect(q.CA[TLSSecretKeyCert]).NotTo(Equal(p.CA[TLSSecretKeyCert]))
			_, err := parseTLSMaterial(q.Server, host, t0)
			Expect(err).NotTo(HaveOccurred())
			Expect(q.Server[TLSSecretKeyCert]).NotTo(Equal(p.Server[TLSSecretKeyCert]))
		})

		It("rotates the CA in two steps without breaking verify-full", func() {
			p := plan(nil, nil, t0)
			oldCA := parsePEMCert(p.CA[TLSSecretKeyCert])
			prepareAt := oldCA.NotAfter.Add(-shortPolicy.CAPrepareBefore)
			switchAt := oldCA.NotAfter.Add(-shortPolicy.CASwitchBefore)

			By("renewing server certificates from the old CA until the next CA is prepared")
			for now := t0; now.Before(prepareAt); now = p.NextCheck {
				p = plan(p.CA, p.Server, now)
			}
			Expect(p.CA).NotTo(HaveKey(caSecretKeyNextCert))
			Expect(p.NextCheck).To(Equal(prepareAt))

			By("preparing the next CA: trusted by clients, not signing yet")
			before := p
			p = plan(p.CA, p.Server, prepareAt)
			Expect(p.CA).To(HaveKey(caSecretKeyNextCert))
			Expect(bundleLen(p)).To(Equal(2))
			Expect(p.CA[TLSSecretKeyCert]).To(Equal(before.CA[TLSSecretKeyCert]))
			Expect(p.Server[TLSSecretKeyCert]).To(Equal(before.Server[TLSSecretKeyCert]))
			Expect(p.Server[TLSSecretKeyCA]).To(Equal(p.CA[TLSSecretKeyCA]))
			nextCA := parsePEMCert(p.CA[caSecretKeyNextCert])

			By("switching to the next CA")
			for now := prepareAt; now.Before(switchAt); now = p.NextCheck {
				p = plan(p.CA, p.Server, now)
			}
			oldLeaf := p.Server[TLSSecretKeyCert]
			p = plan(p.CA, p.Server, switchAt)
			Expect(parsePEMCert(p.CA[TLSSecretKeyCert]).Equal(nextCA)).To(BeTrue())
			Expect(p.CA).NotTo(HaveKey(caSecretKeyNextCert))
			Expect(bundleLen(p)).To(Equal(2), "the previous CA stays trusted until it expires")
			Expect(leaf(p).CheckSignatureFrom(nextCA)).To(Succeed())

			By("still verifying the previous server certificate, so the server can be reloaded")
			b, _ := pem.Decode(oldLeaf)
			Expect(certVerifies(b.Bytes, p.Server[TLSSecretKeyCA], host, switchAt)).To(BeTrue())

			By("dropping the previous CA from the bundle once it expired")
			Expect(p.NextCheck).NotTo(BeTemporally(">", oldCA.NotAfter))
			p = plan(p.CA, p.Server, oldCA.NotAfter)
			Expect(bundleLen(p)).To(Equal(1))
		})

		It("generates a new CA when the switch is overdue and no next CA was prepared", func() {
			p := plan(nil, nil, t0)
			oldCA := parsePEMCert(p.CA[TLSSecretKeyCert])
			q := plan(p.CA, p.Server, oldCA.NotAfter.Add(-time.Hour))
			Expect(parsePEMCert(q.CA[TLSSecretKeyCert]).Equal(oldCA)).To(BeFalse())
			_, err := parseTLSMaterial(q.Server, host, oldCA.NotAfter.Add(-time.Hour))
			Expect(err).NotTo(HaveOccurred())
		})

		It("never issues a server certificate that outlives its CA", func() {
			ca, err := generateCA("ca", t0, time.Hour)
			Expect(err).NotTo(HaveOccurred())
			leaf, err := issueServerCert(ca, names, t0, 24*time.Hour)
			Expect(err).NotTo(HaveOccurred())
			Expect(leaf.Cert.NotAfter).To(Equal(ca.Cert.NotAfter))
		})
	})

	It("truncates long CommonNames", func() {
		Expect(commonName("short")).To(Equal("short"))
		long := fmt.Sprintf("%070d", 0)
		Expect(commonName(long)).To(HaveLen(64))
	})

	It("selects the TLS Secret per mode", func() {
		c := &postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
		c.Spec.TLS = &postgresv1alpha1.ClusterTLSSpec{SecretName: "mine"}
		Expect(tlsSecretName(c)).To(Equal("mine"))
		c.Spec.TLS = &postgresv1alpha1.ClusterTLSSpec{IssuerRef: &postgresv1alpha1.CertManagerIssuerReference{Name: "ca"}}
		Expect(tlsSecretName(c)).To(Equal("db-server-tls"))
		c.Spec.TLS = &postgresv1alpha1.ClusterTLSSpec{}
		Expect(c.Spec.TLS.IsSelfManaged()).To(BeTrue())
		Expect(tlsSecretName(c)).To(Equal("db-server-cert"))
		Expect(serverDNSNames(c)).To(Equal(testDNSNames))
	})

	It("verifies presented certificates against a CA bundle", func() {
		ca := newTestCA()
		der := leafDER(ca.issueFor(host))
		Expect(certVerifies(der, ca.pem, host, time.Now())).To(BeTrue())
		Expect(certVerifies(der, newTestCA().pem, host, time.Now())).To(BeFalse())
		Expect(certVerifies(der, ca.pem, "other", time.Now())).To(BeFalse())
		Expect(certVerifies([]byte("junk"), ca.pem, host, time.Now())).To(BeFalse())
	})

	Context("on a Cluster", func() {
		var (
			ctx  context.Context
			name string
		)
		BeforeEach(func() {
			ctx = context.Background()
			name = fmt.Sprintf("tlssm-%d", time.Now().UnixNano())
		})
		key := func(n string) types.NamespacedName { return types.NamespacedName{Name: n, Namespace: testTLSNamespace} }
		getCluster := func() *postgresv1alpha1.Cluster {
			c := &postgresv1alpha1.Cluster{}
			Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
			return c
		}
		getSecret := func(n string) *corev1.Secret {
			s := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, key(n), s)).To(Succeed())
			return s
		}
		secretGone := func(n string) bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key(n), &corev1.Secret{}))
		}
		getSTS := func() *appsv1.StatefulSet {
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
			return sts
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
		reconcile := func(r *ClusterReconciler) time.Duration {
			res, err := r.Reconcile(ctx, reconcileRequest(getCluster()))
			Expect(err).NotTo(HaveOccurred())
			return res.RequeueAfter
		}
		markSTSReady := func() {
			sts := getSTS()
			sts.Status.Replicas = 1
			sts.Status.ReadyReplicas = 1
			Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
		}

		It("accepts spec.tls without secretName or issuerRef", func() {
			createCluster(&postgresv1alpha1.ClusterTLSSpec{})
			t := getCluster().Spec.TLS
			Expect(t).NotTo(BeNil())
			Expect(t.IsSelfManaged()).To(BeTrue())
			Expect(t.IsRequireTLS()).To(BeTrue())
		})

		It("rejects secretName together with issuerRef", func() {
			c := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testTLSNamespace},
				Spec: postgresv1alpha1.ClusterSpec{TLS: &postgresv1alpha1.ClusterTLSSpec{
					SecretName: "x",
					IssuerRef:  &postgresv1alpha1.CertManagerIssuerReference{Name: "ca"},
				}},
			}
			err := k8sClient.Create(ctx, c)
			Expect(apierrors.IsInvalid(err)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
		})

		It("generates owned CA and server Secrets, mounts them and keeps them stable", func() {
			createCluster(&postgresv1alpha1.ClusterTLSSpec{})
			r := &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			requeue := reconcile(r)
			Expect(requeue).To(BeNumerically(">", 0))

			cluster := getCluster()
			caSecret := getSecret(name + "-ca")
			server := getSecret(name + "-server-cert")
			for _, s := range []*corev1.Secret{caSecret, server} {
				Expect(metav1.IsControlledBy(s, cluster)).To(BeTrue())
				Expect(s.Type).To(Equal(corev1.SecretTypeTLS))
			}
			Expect(server.Data).NotTo(HaveKey(caSecretKeyNextKey))
			Expect(server.Data[TLSSecretKeyCA]).To(Equal(caSecret.Data[TLSSecretKeyCA]))
			_, err := parseTLSMaterial(server.Data, clusterHost(cluster), time.Now())
			Expect(err).NotTo(HaveOccurred())

			sts := getSTS()
			Expect(sts.Spec.Template.Spec.Volumes[0].Secret.SecretName).To(Equal(name + "-server-cert"))
			Expect(tlsCondition().Reason).To(Equal(ReasonWaitingForServer))

			By("not rewriting anything on the next reconcile")
			reconcile(r)
			Expect(getSecret(name + "-ca").ResourceVersion).To(Equal(caSecret.ResourceVersion))
			Expect(getSecret(name + "-server-cert").ResourceVersion).To(Equal(server.ResourceVersion))
			Expect(getSTS().ResourceVersion).To(Equal(sts.ResourceVersion))

			By("mapping the server Secret to the Cluster")
			Expect(r.clustersForTLSSecret(ctx, server)).To(ContainElement(reconcileRequest(cluster)))
		})

		It("does not take over a Secret it does not own", func() {
			s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-ca", Namespace: testTLSNamespace},
				StringData: map[string]string{"foo": "bar"}}
			Expect(k8sClient.Create(ctx, s)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, s) })
			createCluster(&postgresv1alpha1.ClusterTLSSpec{})
			reconcile(&ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()})

			cond := tlsCondition()
			Expect(cond.Reason).To(Equal(ReasonInvalidTLSSecret))
			Expect(cond.Message).To(ContainSubstring("not owned by the Cluster"))
			Expect(getSecret(name + "-ca").Data).To(HaveKey("foo"))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{}))).To(BeTrue())
		})

		It("renews the server certificate and reloads the server without a restart", func() {
			now := time.Now()
			createCluster(&postgresv1alpha1.ClusterTLSSpec{})
			var reloads []postgres.ConnectionConfig
			var presented []byte
			r := &ClusterReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Now:                   func() time.Time { return now },
				SelfManagedCertPolicy: &shortPolicy,
				ProbeServerCertificate: func(context.Context, string, string) ([]byte, error) {
					return presented, nil
				},
				ReloadServerConfig: func(_ context.Context, cfg postgres.ConnectionConfig) error {
					reloads = append(reloads, cfg)
					return nil
				},
			}
			requeue := reconcile(r)
			Expect(requeue).To(BeNumerically("<=", shortPolicy.ServerValidity-shortPolicy.ServerRenewBefore))
			markSTSReady()
			presented = leafDER(getSecret(name + "-server-cert").Data)
			reconcile(r)
			Expect(tlsCondition().Status).To(Equal(metav1.ConditionTrue))
			Expect(tlsCondition().Message).To(ContainSubstring(name + "-server-cert"))
			creds := getSecret(name + "-credentials")
			Expect(creds.Data[SecretKeyCACert]).To(Equal(getSecret(name + "-ca").Data[TLSSecretKeyCA]))

			By("advancing past the renewal time")
			oldLeaf := getSecret(name + "-server-cert").Data[TLSSecretKeyCert]
			now = now.Add(shortPolicy.ServerValidity - shortPolicy.ServerRenewBefore + time.Minute)
			reconcile(r)
			Expect(getSecret(name + "-server-cert").Data[TLSSecretKeyCert]).NotTo(Equal(oldLeaf))
			Expect(tlsCondition().Reason).To(Equal(ReasonCertificateReloading))
			Expect(reloads).To(HaveLen(1))
			Expect(reloads[0].SSLMode).To(Equal(postgres.SSLModeVerifyFull))
			Expect(getSTS().Spec.Template.Annotations).NotTo(HaveKey(AnnotationTLSRestart))

			By("confirming the renewed certificate")
			presented = leafDER(getSecret(name + "-server-cert").Data)
			reconcile(r)
			Expect(tlsCondition().Status).To(Equal(metav1.ConditionTrue))
		})

		It("removes the self-managed Secrets when TLS is turned off", func() {
			createCluster(&postgresv1alpha1.ClusterTLSSpec{})
			r := &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			reconcile(r)
			Expect(secretGone(name + "-ca")).To(BeFalse())

			c := getCluster()
			c.Spec.TLS = nil
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			reconcile(r)
			Expect(getSTS().Spec.Template.Spec.Volumes).To(BeEmpty())
			Expect(secretGone(name + "-ca")).To(BeTrue())
			Expect(secretGone(name + "-server-cert")).To(BeTrue())
		})
	})
})
