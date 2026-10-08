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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

const (
	testTLSNamespace = "default"
	testHBAArg       = "hba_file=" + hbaMountPath + "/" + hbaFileName
)

// testCA is a throwaway CA for issuing test server certificates.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA() *testCA {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pgop test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	Expect(err).NotTo(HaveOccurred())
	cert, err := x509.ParseCertificate(der)
	Expect(err).NotTo(HaveOccurred())
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der})}
}

// issue returns kubernetes.io/tls Secret data for a server certificate with
// the given DNS SANs, valid from notBefore to notAfter.
func (ca *testCA) issue(dnsNames []string, notBefore, notAfter time.Time) map[string][]byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "server"},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	Expect(err).NotTo(HaveOccurred())
	keyDER, err := x509.MarshalECPrivateKey(key)
	Expect(err).NotTo(HaveOccurred())
	return map[string][]byte{
		TLSSecretKeyCert: pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der}),
		TLSSecretKeyKey:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		TLSSecretKeyCA:   ca.pem,
	}
}

func (ca *testCA) issueFor(host string) map[string][]byte {
	return ca.issue([]string{host}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
}

// leafDER returns the DER of the first certificate in tls.crt.
func leafDER(data map[string][]byte) []byte {
	block, _ := pem.Decode(data[TLSSecretKeyCert])
	Expect(block).NotTo(BeNil())
	return block.Bytes
}

var _ = Describe("TLS helpers", func() {
	const host = "db.ns.svc.cluster.local"

	Context("parseTLSMaterial", func() {
		var ca *testCA
		BeforeEach(func() { ca = newTestCA() })

		It("accepts a valid certificate for the Service host", func() {
			data := ca.issueFor(host)
			m, err := parseTLSMaterial(data, host, time.Now())
			Expect(err).NotTo(HaveOccurred())
			Expect(m.Leaf.Raw).To(Equal(leafDER(data)))
			Expect(m.CAPEM).To(Equal(ca.pem))
			Expect(m.Hash).To(HaveLen(64))
		})

		for _, key := range []string{TLSSecretKeyCert, TLSSecretKeyKey, TLSSecretKeyCA} {
			It("rejects a Secret without "+key, func() {
				data := ca.issueFor(host)
				delete(data, key)
				_, err := parseTLSMaterial(data, host, time.Now())
				Expect(err).To(MatchError(ContainSubstring(key)))
			})
		}

		It("rejects a key that does not match the certificate", func() {
			data := ca.issueFor(host)
			data[TLSSecretKeyKey] = ca.issueFor(host)[TLSSecretKeyKey]
			_, err := parseTLSMaterial(data, host, time.Now())
			Expect(err).To(MatchError(ContainSubstring("not a valid key pair")))
		})

		It("rejects a certificate without the Service host as SAN", func() {
			data := ca.issueFor("other.ns.svc.cluster.local")
			_, err := parseTLSMaterial(data, host, time.Now())
			Expect(err).To(MatchError(ContainSubstring("verify-full")))
		})

		It("rejects a certificate not issued by ca.crt", func() {
			data := ca.issueFor(host)
			data[TLSSecretKeyCA] = newTestCA().pem
			_, err := parseTLSMaterial(data, host, time.Now())
			Expect(err).To(MatchError(ContainSubstring("verify-full")))
		})

		It("rejects an expired certificate", func() {
			data := ca.issue([]string{host}, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
			_, err := parseTLSMaterial(data, host, time.Now())
			Expect(err).To(HaveOccurred())
		})
	})

	Context("pod spec", func() {
		cluster := func(t *postgresv1alpha1.ClusterTLSSpec) *postgresv1alpha1.Cluster {
			return &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
				Spec:       postgresv1alpha1.ClusterSpec{TLS: t},
			}
		}

		It("adds nothing when TLS is disabled", func() {
			vols, mounts := postgresTLSVolumes(cluster(nil))
			Expect(vols).To(BeNil())
			Expect(mounts).To(BeNil())
			Expect(postgresTLSArgs(nil)).To(BeNil())
		})

		It("mounts the certificate and the managed pg_hba by default", func() {
			c := cluster(&postgresv1alpha1.ClusterTLSSpec{SecretName: "db-tls"})
			vols, mounts := postgresTLSVolumes(c)
			Expect(vols).To(HaveLen(2))
			Expect(vols[0].Secret.SecretName).To(Equal("db-tls"))
			Expect(*vols[0].Secret.DefaultMode).To(Equal(int32(0o640)))
			Expect(vols[1].ConfigMap.Name).To(Equal("db-hba"))
			Expect(mounts).To(HaveLen(2))
			Expect(mounts[0].ReadOnly).To(BeTrue())

			args := postgresTLSArgs(c.Spec.TLS)
			Expect(args[0]).To(Equal("postgres"))
			Expect(args).To(ContainElements("ssl=on", "ssl_min_protocol_version=TLSv1.2", testHBAArg))
		})

		It("does not override pg_hba when requireTLS is false", func() {
			c := cluster(&postgresv1alpha1.ClusterTLSSpec{SecretName: "db-tls", RequireTLS: new(false),
				MinProtocolVersion: postgresv1alpha1.TLSProtocolVersion13})
			vols, mounts := postgresTLSVolumes(c)
			Expect(vols).To(HaveLen(1))
			Expect(mounts).To(HaveLen(1))
			args := postgresTLSArgs(c.Spec.TLS)
			Expect(args).To(ContainElement("ssl_min_protocol_version=TLSv1.3"))
			Expect(args).NotTo(ContainElement(testHBAArg))
		})

		It("rejects non-TLS TCP connections in the managed pg_hba", func() {
			Expect(managedPgHBA).To(MatchRegexp(`(?m)^hostnossl\s+all\s+all\s+all\s+reject$`))
			Expect(managedPgHBA).To(MatchRegexp(`(?m)^local\s+all\s+all\s+trust$`))
		})
	})

	Context("connection info", func() {
		It("percent-encodes credentials in the URI", func() {
			uri := connectionURI("app user", "p@ss:w/rd?#'", host, 5432, "my db", "verify-full")
			u, err := url.Parse(uri)
			Expect(err).NotTo(HaveOccurred())
			Expect(u.Scheme).To(Equal("postgresql"))
			Expect(u.User.Username()).To(Equal("app user"))
			pw, _ := u.User.Password()
			Expect(pw).To(Equal("p@ss:w/rd?#'"))
			Expect(u.Host).To(Equal(host + ":5432"))
			Expect(u.Path).To(Equal("/my db"))
			Expect(u.Query().Get("sslmode")).To(Equal("verify-full"))
		})

		It("adds ca.crt with verify-full and removes it again", func() {
			data := map[string][]byte{SecretKeyUsername: []byte("u"), SecretKeyPassword: []byte("p")}
			Expect(applyConnectionInfo(data, host, 5432, "postgres",
				clientTLS{SSLMode: postgres.SSLModeVerifyFull, CACert: []byte("CA")})).To(BeTrue())
			Expect(data).To(HaveKeyWithValue(SecretKeyCACert, []byte("CA")))
			Expect(string(data[SecretKeySSLMode])).To(Equal("verify-full"))

			Expect(applyConnectionInfo(data, host, 5432, "postgres",
				clientTLS{SSLMode: postgres.SSLModeVerifyFull, CACert: []byte("CA")})).To(BeFalse())

			Expect(applyConnectionInfo(data, host, 5432, "postgres", clientTLS{SSLMode: postgres.SSLModeDisable})).To(BeTrue())
			Expect(data).NotTo(HaveKey(SecretKeyCACert))
			Expect(string(data[SecretKeyURI])).To(HaveSuffix("?sslmode=disable"))
			Expect(string(data[SecretKeyPassword])).To(Equal("p"))
		})

		It("picks disable, prefer or verify-full from the Cluster TLS state", func() {
			c := &postgresv1alpha1.Cluster{}
			Expect(clientTLSFor(c, []byte("CA"))).To(Equal(clientTLS{SSLMode: "disable"}))
			c.Spec.TLS = &postgresv1alpha1.ClusterTLSSpec{SecretName: "x"}
			Expect(clientTLSFor(c, []byte("CA"))).To(Equal(clientTLS{SSLMode: "prefer"}))
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type: ConditionTypeTLSReady, Status: metav1.ConditionTrue, Reason: ReasonTLSActive})
			Expect(clientTLSFor(c, []byte("CA"))).To(Equal(clientTLS{SSLMode: "verify-full", CACert: []byte("CA")}))
		})
	})

	Context("probeServerCertificate", func() {
		// fakeServer accepts one connection, reads the SSLRequest, answers
		// with reply and, for 'S', completes a TLS handshake with cert.
		fakeServer := func(reply byte, cert tls.Certificate) string {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = ln.Close() })
			go func() {
				defer GinkgoRecover()
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				req := make([]byte, 8)
				_, _ = io.ReadFull(conn, req)
				_, _ = conn.Write([]byte{reply})
				if reply == 'S' {
					srv := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
					_ = srv.Handshake()
				}
			}()
			return ln.Addr().String()
		}

		It("returns the certificate the server presents", func() {
			data := newTestCA().issueFor(host)
			cert, err := tls.X509KeyPair(data[TLSSecretKeyCert], data[TLSSecretKeyKey])
			Expect(err).NotTo(HaveOccurred())
			got, err := probeServerCertificate(context.Background(), fakeServer('S', cert), host)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(leafDER(data)))
		})

		It("reports a server without TLS", func() {
			_, err := probeServerCertificate(context.Background(), fakeServer('N', tls.Certificate{}), host)
			Expect(err).To(MatchError(errServerTLSUnavailable))
		})
	})
})

var _ = Describe("Cluster TLS", func() {
	var (
		ctx  context.Context
		ca   *testCA
		name string
	)

	BeforeEach(func() {
		ctx = context.Background()
		ca = newTestCA()
		name = fmt.Sprintf("tls-%d", time.Now().UnixNano())
	})

	cleanup := func(obj client.Object) {
		DeferCleanup(func() {
			obj.SetFinalizers(nil)
			_ = k8sClient.Update(ctx, obj)
			_ = k8sClient.Delete(ctx, obj)
		})
	}

	host := func() string { return name + "." + testTLSNamespace + ".svc.cluster.local" }

	createTLSSecret := func(secretName string, data map[string][]byte) *corev1.Secret {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testTLSNamespace},
			Type:       corev1.SecretTypeTLS,
			Data:       data,
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		cleanup(s)
		return s
	}

	createCluster := func(t *postgresv1alpha1.ClusterTLSSpec) *postgresv1alpha1.Cluster {
		c := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testTLSNamespace},
			Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage, TLS: t},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		cleanup(c)
		return c
	}

	getCluster := func() *postgresv1alpha1.Cluster {
		c := &postgresv1alpha1.Cluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testTLSNamespace}, c)).To(Succeed())
		return c
	}

	setTLS := func(t *postgresv1alpha1.ClusterTLSSpec) {
		c := getCluster()
		c.Spec.TLS = t
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
	}

	getSTS := func() *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testTLSNamespace}, sts)).To(Succeed())
		return sts
	}

	getSecret := func(secretName string) *corev1.Secret {
		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: testTLSNamespace}, s)).To(Succeed())
		return s
	}

	hbaExists := func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-hba", Namespace: testTLSNamespace}, &corev1.ConfigMap{})
		if apierrors.IsNotFound(err) {
			return false
		}
		Expect(err).NotTo(HaveOccurred())
		return true
	}

	markSTSReady := func() {
		sts := getSTS()
		sts.Status.Replicas = 1
		sts.Status.ReadyReplicas = 1
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
	}

	tlsCondition := func() *metav1.Condition {
		return meta.FindStatusCondition(getCluster().Status.Conditions, ConditionTypeTLSReady)
	}

	newReconciler := func() *ClusterReconciler {
		return &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	reconcileOnce := func(r *ClusterReconciler) {
		_, err := r.Reconcile(ctx, reconcileRequest(getCluster()))
		Expect(err).NotTo(HaveOccurred())
	}

	It("defaults requireTLS and minProtocolVersion", func() {
		createTLSSecret(name+"-tls", ca.issueFor(host()))
		createCluster(&postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls"})
		t := getCluster().Spec.TLS
		Expect(t.RequireTLS).NotTo(BeNil())
		Expect(*t.RequireTLS).To(BeTrue())
		Expect(t.MinProtocolVersion).To(Equal(postgresv1alpha1.TLSProtocolVersion12))
	})

	It("restarts the pod once when the CA changed, instead of reloading", func() {
		data := ca.issueFor(host())
		tlsSecret := createTLSSecret(name+"-tls", data)
		createCluster(&postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls"})

		presented := leafDER(data)
		reloads := 0
		r := newReconciler()
		r.ProbeServerCertificate = func(context.Context, string, string) ([]byte, error) { return presented, nil }
		r.ReloadServerConfig = func(context.Context, postgres.ConnectionConfig) error {
			reloads++
			return nil
		}
		reconcileOnce(r)
		markSTSReady()
		reconcileOnce(r)
		Expect(tlsCondition().Status).To(Equal(metav1.ConditionTrue))
		Expect(getSTS().Spec.Template.Annotations).NotTo(HaveKey(AnnotationTLSRestart))

		By("replacing the certificate with one from a new CA")
		tlsSecret.Data = newTestCA().issueFor(host())
		Expect(k8sClient.Update(ctx, tlsSecret)).To(Succeed())
		reconcileOnce(r)
		cond := tlsCondition()
		Expect(cond.Reason).To(Equal(ReasonCertificateReloading))
		Expect(cond.Message).To(ContainSubstring("restarting the PostgreSQL pod"))
		Expect(reloads).To(BeZero(), "never reload over a connection the new CA cannot verify")
		sts := getSTS()
		hash := sts.Spec.Template.Annotations[AnnotationTLSRestart]
		Expect(hash).NotTo(BeEmpty())

		By("not restarting again for the same certificate")
		reconcileOnce(r)
		Expect(getSTS().ResourceVersion).To(Equal(sts.ResourceVersion))

		By("becoming ready once the restarted pod presents the new certificate")
		presented = leafDER(tlsSecret.Data)
		reconcileOnce(r)
		Expect(tlsCondition().Status).To(Equal(metav1.ConditionTrue))
		Expect(getSTS().Spec.Template.Annotations[AnnotationTLSRestart]).To(Equal(hash))
	})

	It("leaves a Cluster without spec.tls exactly as before", func() {
		createCluster(nil)
		reconcileOnce(newReconciler())

		sts := getSTS()
		Expect(sts.Spec.Template.Spec.Volumes).To(BeEmpty())
		Expect(sts.Spec.Template.Spec.Containers[0].Args).To(BeEmpty())
		Expect(hbaExists()).To(BeFalse())
		Expect(tlsCondition()).To(BeNil())

		creds := getSecret(name + "-credentials")
		Expect(string(creds.Data[SecretKeySSLMode])).To(Equal("disable"))
		Expect(creds.Data).NotTo(HaveKey(SecretKeyCACert))
		Expect(string(creds.Data[SecretKeyURI])).To(HavePrefix("postgresql://pgop_operator:"))
		Expect(string(creds.Data[SecretKeyURI])).To(HaveSuffix("@" + host() + ":5432/postgres?sslmode=disable"))
	})

	It("mounts the certificate, enables ssl and creates an owned pg_hba ConfigMap", func() {
		createTLSSecret(name+"-tls", ca.issueFor(host()))
		createCluster(&postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls"})
		r := newReconciler()
		reconcileOnce(r)

		sts := getSTS()
		Expect(sts.Spec.Template.Spec.Volumes).To(HaveLen(2))
		c := sts.Spec.Template.Spec.Containers[0]
		Expect(c.Args).To(ContainElements("ssl=on", testHBAArg))
		Expect(c.VolumeMounts).To(ContainElement(corev1.VolumeMount{Name: tlsVolumeName, MountPath: tlsMountPath, ReadOnly: true}))

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-hba", Namespace: testTLSNamespace}, cm)).To(Succeed())
		Expect(cm.Data[hbaFileName]).To(Equal(managedPgHBA))
		Expect(metav1.IsControlledBy(cm, getCluster())).To(BeTrue())

		cond := tlsCondition()
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonWaitingForServer))

		By("advertising sslmode=prefer until the server is confirmed to serve TLS")
		creds := getSecret(name + "-credentials")
		Expect(string(creds.Data[SecretKeySSLMode])).To(Equal("prefer"))
		Expect(creds.Data).NotTo(HaveKey(SecretKeyCACert))

		By("not updating the StatefulSet again on an unchanged reconcile")
		rv := getSTS().ResourceVersion
		reconcileOnce(r)
		Expect(getSTS().ResourceVersion).To(Equal(rv))
	})

	It("does not manage pg_hba when requireTLS is false", func() {
		createTLSSecret(name+"-tls", ca.issueFor(host()))
		createCluster(&postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls", RequireTLS: new(false)})
		reconcileOnce(newReconciler())

		sts := getSTS()
		Expect(sts.Spec.Template.Spec.Volumes).To(HaveLen(1))
		Expect(sts.Spec.Template.Spec.Containers[0].Args).To(ContainElement("ssl=on"))
		Expect(sts.Spec.Template.Spec.Containers[0].Args).NotTo(ContainElement(testHBAArg))
		Expect(hbaExists()).To(BeFalse())
	})

	It("converges an existing StatefulSet when TLS is enabled and disabled", func() {
		createTLSSecret(name+"-tls", ca.issueFor(host()))
		createCluster(nil)
		r := newReconciler()
		reconcileOnce(r)
		Expect(getSTS().Spec.Template.Spec.Volumes).To(BeEmpty())

		By("enabling TLS")
		setTLS(&postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls"})
		reconcileOnce(r)
		sts := getSTS()
		Expect(sts.Spec.Template.Spec.Volumes).To(HaveLen(2))
		Expect(sts.Spec.Template.Spec.Containers[0].Args).To(ContainElement("ssl=on"))
		Expect(hbaExists()).To(BeTrue())

		By("disabling TLS")
		setTLS(nil)
		reconcileOnce(r)
		sts = getSTS()
		Expect(sts.Spec.Template.Spec.Volumes).To(BeEmpty())
		Expect(sts.Spec.Template.Spec.Containers[0].Args).To(BeEmpty())
		Expect(sts.Spec.Template.Spec.Containers[0].VolumeMounts).To(HaveLen(1))
		Expect(hbaExists()).To(BeFalse())
		Expect(tlsCondition()).To(BeNil())
	})

	DescribeTable("reports an unusable TLS Secret without touching the StatefulSet",
		func(mutate func(data map[string][]byte) map[string][]byte, wantMsg string) {
			createCluster(nil)
			r := newReconciler()
			reconcileOnce(r)
			before := getSTS()

			if data := mutate(ca.issueFor(host())); data != nil {
				createTLSSecret(name+"-tls", data)
			}
			setTLS(&postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls"})
			reconcileOnce(r)

			cond := tlsCondition()
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonInvalidTLSSecret))
			Expect(cond.Message).To(ContainSubstring(wantMsg))
			Expect(getSTS().ResourceVersion).To(Equal(before.ResourceVersion))
			Expect(hbaExists()).To(BeFalse())
		},
		Entry("missing Secret", func(map[string][]byte) map[string][]byte { return nil }, "not found"),
		Entry("missing ca.crt", func(d map[string][]byte) map[string][]byte {
			delete(d, TLSSecretKeyCA)
			return d
		}, TLSSecretKeyCA),
		Entry("SAN mismatch", func(map[string][]byte) map[string][]byte {
			return ca.issueFor("wrong.example.com")
		}, "verify-full"),
	)

	It("does not create the StatefulSet while the TLS Secret is missing", func() {
		createCluster(&postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls"})
		reconcileOnce(newReconciler())
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testTLSNamespace}, &appsv1.StatefulSet{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(tlsCondition().Reason).To(Equal(ReasonInvalidTLSSecret))
	})

	It("becomes TLSReady once the server presents the certificate, and reloads a rotated one", func() {
		data := ca.issueFor(host())
		tlsSecret := createTLSSecret(name+"-tls", data)
		createCluster(&postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls"})

		presented := leafDER(data)
		var reloads []postgres.ConnectionConfig
		r := newReconciler()
		r.ProbeServerCertificate = func(_ context.Context, addr, serverName string) ([]byte, error) {
			Expect(addr).To(Equal(host() + ":5432"))
			Expect(serverName).To(Equal(host()))
			return presented, nil
		}
		r.ReloadServerConfig = func(_ context.Context, cfg postgres.ConnectionConfig) error {
			reloads = append(reloads, cfg)
			return nil
		}

		reconcileOnce(r)
		markSTSReady()
		reconcileOnce(r)

		cond := tlsCondition()
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(ReasonTLSActive))
		Expect(getCluster().Status.TLSSecretHash).NotTo(BeEmpty())
		Expect(reloads).To(BeEmpty())

		creds := getSecret(name + "-credentials")
		Expect(string(creds.Data[SecretKeySSLMode])).To(Equal("verify-full"))
		Expect(creds.Data[SecretKeyCACert]).To(Equal(ca.pem))
		Expect(string(creds.Data[SecretKeyURI])).To(HaveSuffix("?sslmode=verify-full"))

		By("connecting the operator with verify-full and the CA")
		cfg, err := operatorConnectionConfig(ctx, k8sClient, getCluster(), "postgres")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.SSLMode).To(Equal(postgres.SSLModeVerifyFull))
		Expect(cfg.RootCertPEM).To(Equal(ca.pem))
		Expect(cfg.Host).To(Equal(host()))

		By("rotating the certificate")
		oldHash := getCluster().Status.TLSSecretHash
		tlsSecret.Data = ca.issueFor(host())
		Expect(k8sClient.Update(ctx, tlsSecret)).To(Succeed())
		reconcileOnce(r)
		Expect(tlsCondition().Reason).To(Equal(ReasonCertificateReloading))
		Expect(reloads).To(HaveLen(1))
		Expect(reloads[0].SSLMode).To(Equal(postgres.SSLModeVerifyFull))
		Expect(reloads[0].RootCertPEM).To(Equal(ca.pem))

		By("confirming the reloaded certificate")
		presented = leafDER(tlsSecret.Data)
		reconcileOnce(r)
		Expect(tlsCondition().Status).To(Equal(metav1.ConditionTrue))
		Expect(getCluster().Status.TLSSecretHash).NotTo(Equal(oldHash))

		By("dropping ca.crt and verify-full when TLS is disabled")
		setTLS(nil)
		reconcileOnce(r)
		creds = getSecret(name + "-credentials")
		Expect(string(creds.Data[SecretKeySSLMode])).To(Equal("disable"))
		Expect(creds.Data).NotTo(HaveKey(SecretKeyCACert))
		Expect(getCluster().Status.TLSSecretHash).To(BeEmpty())
	})

	It("maps a TLS Secret to the Clusters that reference it", func() {
		createCluster(&postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls"})
		reqs := newReconciler().clustersForTLSSecret(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-tls", Namespace: testTLSNamespace}})
		Expect(reqs).To(ContainElement(reconcileRequest(getCluster())))
		reqs = newReconciler().clustersForTLSSecret(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-other", Namespace: testTLSNamespace}})
		Expect(reqs).NotTo(ContainElement(reconcileRequest(getCluster())))
	})

	Context("Role and Database credentials Secrets", func() {
		activateTLS := func() *postgresv1alpha1.Cluster {
			createTLSSecret(name+"-tls", ca.issueFor(host()))
			c := createCluster(&postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls"})
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type: ConditionTypeTLSReady, Status: metav1.ConditionTrue, Reason: ReasonTLSActive, Message: "test"})
			Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
			return c
		}

		It("converges an existing Role Secret onto TLS and keeps its password", func() {
			cluster := createCluster(nil)
			role := &postgresv1alpha1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-app", Namespace: testTLSNamespace},
				Spec:       postgresv1alpha1.RoleSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: name}},
			}
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)

			rr := &RoleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			const pw = "role-pw"
			secretName, err := rr.reconcileCredentialsSecret(ctx, role, cluster, nil, pw)
			Expect(err).NotTo(HaveOccurred())
			s := getSecret(secretName)
			Expect(string(s.Data[SecretKeySSLMode])).To(Equal("disable"))

			By("turning TLS on for the Cluster")
			createTLSSecret(name+"-tls", ca.issueFor(host()))
			cluster = getCluster()
			cluster.Spec.TLS = &postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls"}
			cluster.Spec.Port = 6432
			meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type: ConditionTypeTLSReady, Status: metav1.ConditionTrue, Reason: ReasonTLSActive, Message: "test"})

			_, err = rr.reconcileCredentialsSecret(ctx, role, cluster, getSecret(secretName), pw)
			Expect(err).NotTo(HaveOccurred())
			s = getSecret(secretName)
			Expect(string(s.Data[SecretKeyPassword])).To(Equal(pw))
			Expect(string(s.Data[SecretKeyPort])).To(Equal("6432"))
			Expect(string(s.Data[SecretKeySSLMode])).To(Equal("verify-full"))
			Expect(s.Data[SecretKeyCACert]).To(Equal(ca.pem))
			Expect(string(s.Data[SecretKeyURI])).To(ContainSubstring(":6432/postgres?sslmode=verify-full"))

			By("mapping the Cluster to the Role")
			Expect(rr.rolesForCluster(ctx, cluster)).To(ContainElement(reconcileRequest(role)))
		})

		It("writes TLS settings into the Database Secret and removes ca.crt when TLS goes away", func() {
			cluster := activateTLS()
			owner := &postgresv1alpha1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-owner", Namespace: testTLSNamespace},
				Spec:       postgresv1alpha1.RoleSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: name}},
			}
			Expect(k8sClient.Create(ctx, owner)).To(Succeed())
			cleanup(owner)
			roleSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-" + owner.Name + "-credentials", Namespace: testTLSNamespace},
				Data:       map[string][]byte{SecretKeyUsername: []byte("owner"), SecretKeyPassword: []byte("pw")},
			}
			Expect(k8sClient.Create(ctx, roleSecret)).To(Succeed())
			cleanup(roleSecret)
			db := &postgresv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-db", Namespace: testTLSNamespace},
				Spec: postgresv1alpha1.DatabaseSpec{
					ClusterRef: postgresv1alpha1.ClusterReference{Name: name}, Owner: owner.Name},
			}
			Expect(k8sClient.Create(ctx, db)).To(Succeed())
			cleanup(db)

			dr := &DatabaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(dr.reconcileCredentialsSecret(ctx, db, owner, cluster)).To(Succeed())
			s := getSecret(db.Name + "-" + owner.Name + "-credentials")
			Expect(string(s.Data[SecretKeySSLMode])).To(Equal("verify-full"))
			Expect(s.Data[SecretKeyCACert]).To(Equal(ca.pem))
			Expect(string(s.Data[SecretKeyURI])).To(Equal(
				"postgresql://owner:pw@" + host() + ":5432/" + db.Name + "?sslmode=verify-full"))

			cluster.Spec.TLS = nil
			Expect(dr.reconcileCredentialsSecret(ctx, db, owner, cluster)).To(Succeed())
			s = getSecret(db.Name + "-" + owner.Name + "-credentials")
			Expect(string(s.Data[SecretKeySSLMode])).To(Equal("disable"))
			Expect(s.Data).NotTo(HaveKey(SecretKeyCACert))

			Expect(dr.databasesForCluster(ctx, cluster)).To(ContainElement(reconcileRequest(db)))
		})

		It("only passes Cluster updates that change how clients connect", func() {
			c := &postgresv1alpha1.Cluster{}
			c2 := c.DeepCopy()
			c2.Status.Endpoint = "changed"
			Expect(clusterConnectionFingerprint(c)).To(Equal(clusterConnectionFingerprint(c2)))
			c2.Status.TLSSecretHash = "abc"
			Expect(clusterConnectionFingerprint(c)).NotTo(Equal(clusterConnectionFingerprint(c2)))
		})
	})
})
