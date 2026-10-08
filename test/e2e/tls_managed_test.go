//go:build e2e

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

package e2e

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ruckc/pgop/test/utils"
)

// RegisterManagedTLSTests adds specs for operator-provisioned server
// certificates (issue #22 phase 2): spec.tls.issuerRef (cert-manager) and the
// self-managed CA. Must be called inside a Describe/Context that has already
// deployed the controller into `namespace`; cert-manager is installed by the
// suite.
func RegisterManagedTLSTests() {
	Context("Managed TLS", Ordered, func() {
		SetDefaultEventuallyTimeout(5 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		const (
			issuerCluster = "tls-issuer"
			selfCluster   = "tls-self"
		)

		kubectl := func(args ...string) (string, error) {
			out, err := utils.Run(exec.Command("kubectl", append([]string{"-n", namespace}, args...)...))
			return strings.TrimSpace(out), err
		}
		get := func(args ...string) string {
			out, err := kubectl(append([]string{"get"}, args...)...)
			Expect(err).NotTo(HaveOccurred())
			return out
		}
		apply := func(manifest string) {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		}
		secretValue := func(g Gomega, secret, key string) string {
			escaped := strings.ReplaceAll(key, ".", `\.`)
			out, err := kubectl("get", "secret", secret, "-o", "jsonpath={.data."+escaped+"}")
			g.Expect(err).NotTo(HaveOccurred())
			v, err := base64.StdEncoding.DecodeString(out)
			g.Expect(err).NotTo(HaveOccurred())
			return string(v)
		}
		clusterState := func(name string) string {
			out, _ := kubectl("get", "cluster", name,
				"-o", `jsonpath={.status.ready}/{.status.conditions[?(@.type=="TLSReady")].status}`)
			return out
		}
		waitTLSReady := func(name string) {
			Eventually(func() string { return clusterState(name) }).Should(Equal("true/True"))
		}
		// verifyFull connects inside the pod over TCP with sslmode=verify-full
		// against the mounted CA bundle and returns pg_stat_ssl.ssl.
		verifyFull := func(g Gomega, name string) {
			fqdn := fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace)
			password := secretValue(g, name+"-credentials", "password")
			conn := fmt.Sprintf("host=%s hostaddr=127.0.0.1 user=pgop_operator dbname=postgres "+
				"sslmode=verify-full sslrootcert=/etc/pgop/tls/ca.crt", fqdn)
			out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, name+"-0", "-c", "postgresql", "--",
				"env", "PGPASSWORD="+password, "psql", conn, "-tAc", "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("t"))
		}
		ownerKinds := func(kind, name string) string {
			return get(kind, name, "-o", "jsonpath={.metadata.ownerReferences[*].kind}")
		}
		gone := func(kind, name string) func() string {
			return func() string {
				out, _ := kubectl("get", kind, name, "--ignore-not-found", "-o", "name")
				return out
			}
		}

		AfterAll(func() {
			for _, res := range []string{
				"cluster/" + issuerCluster, "cluster/" + selfCluster,
				"issuer/tlsm-ca-issuer", "certificate/tlsm-ca", "issuer/tlsm-selfsigned", "secret/tlsm-ca",
			} {
				_, _ = kubectl("delete", "--ignore-not-found", "--wait=false", res)
			}
			for _, c := range []string{issuerCluster, selfCluster} {
				_, _ = kubectl("delete", "pvc", "--ignore-not-found", "data-"+c+"-0")
			}
		})

		It("issues the server certificate through spec.tls.issuerRef", func() {
			apply(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: tlsm-selfsigned
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: tlsm-ca
spec:
  isCA: true
  commonName: pgop-e2e-managed-ca
  secretName: tlsm-ca
  privateKey:
    algorithm: ECDSA
    size: 256
    rotationPolicy: Always
  issuerRef:
    name: tlsm-selfsigned
    kind: Issuer
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: tlsm-ca-issuer
spec:
  ca:
    secretName: tlsm-ca
`)
			_, err := kubectl("wait", "--for=condition=Ready", "certificate/tlsm-ca", "--timeout=3m")
			Expect(err).NotTo(HaveOccurred())

			apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: %s
spec:
  storage:
    retainPolicy: Delete
  tls:
    issuerRef:
      name: tlsm-ca-issuer
`, issuerCluster))
			waitTLSReady(issuerCluster)

			By("owning the Certificate and the Secret cert-manager wrote")
			Expect(ownerKinds("certificate", issuerCluster+"-server")).To(Equal("Cluster"))
			Expect(ownerKinds("secret", issuerCluster+"-server-tls")).To(ContainSubstring("Cluster"))
			Expect(get("certificate", issuerCluster+"-server", "-o", "jsonpath={.spec.dnsNames[0]}")).
				To(Equal(fmt.Sprintf("%s.%s.svc.cluster.local", issuerCluster, namespace)))

			Eventually(func(g Gomega) {
				g.Expect(secretValue(g, issuerCluster+"-credentials", "sslmode")).To(Equal("verify-full"))
				verifyFull(g, issuerCluster)
			}).Should(Succeed())
		})

		It("restarts the pod when the issuing CA is replaced", func() {
			oldCA := secretValue(Default, issuerCluster+"-server-tls", "ca.crt")
			oldHash := get("cluster", issuerCluster, "-o", "jsonpath={.status.tlsSecretHash}")

			By("replacing the CA and re-issuing the server certificate from it")
			_, err := kubectl("delete", "secret", "tlsm-ca")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(secretValue(g, "tlsm-ca", "ca.crt")).NotTo(BeEmpty())
				g.Expect(secretValue(g, "tlsm-ca", "ca.crt")).NotTo(Equal(oldCA))
			}).Should(Succeed())
			_, err = kubectl("delete", "secret", issuerCluster+"-server-tls")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(secretValue(g, issuerCluster+"-server-tls", "ca.crt")).NotTo(Equal(oldCA))
			}).Should(Succeed())

			By("restarting the pod, since the old certificate cannot be verified with the new CA")
			Eventually(func() string {
				return get("statefulset", issuerCluster, "-o", `jsonpath={.spec.template.metadata.annotations.pgop\.ruck\.io/tls-restart}`)
			}).ShouldNot(BeEmpty())
			Eventually(func(g Gomega) {
				g.Expect(clusterState(issuerCluster)).To(Equal("true/True"))
				hash, err := kubectl("get", "cluster", issuerCluster, "-o", "jsonpath={.status.tlsSecretHash}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(hash).NotTo(Equal(oldHash))
				verifyFull(g, issuerCluster)
			}).Should(Succeed())
		})

		It("removes the Certificate and its Secret with the Cluster", func() {
			_, err := kubectl("delete", "cluster", issuerCluster, "--timeout=2m")
			Expect(err).NotTo(HaveOccurred())
			Eventually(gone("certificate", issuerCluster+"-server")).Should(BeEmpty())
			Eventually(gone("secret", issuerCluster+"-server-tls")).Should(BeEmpty())
		})

		It("generates a self-managed CA when neither secretName nor issuerRef is set", func() {
			apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: %s
spec:
  storage:
    retainPolicy: Delete
  tls: {}
`, selfCluster))
			waitTLSReady(selfCluster)

			Expect(ownerKinds("secret", selfCluster+"-ca")).To(Equal("Cluster"))
			Expect(ownerKinds("secret", selfCluster+"-server-cert")).To(Equal("Cluster"))
			Eventually(func(g Gomega) {
				g.Expect(secretValue(g, selfCluster+"-credentials", "sslmode")).To(Equal("verify-full"))
				g.Expect(secretValue(g, selfCluster+"-credentials", "ca.crt")).
					To(Equal(secretValue(g, selfCluster+"-ca", "ca.crt")))
				verifyFull(g, selfCluster)
			}).Should(Succeed())

			By("never mounting the CA private key into the pod")
			out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, selfCluster+"-0", "-c", "postgresql", "--",
				"ls", "/etc/pgop/tls"))
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Fields(out)).To(ConsistOf("ca.crt", "tls.crt", "tls.key"))
		})

		It("recovers when the self-managed CA is lost", func() {
			oldCA := secretValue(Default, selfCluster+"-ca", "ca.crt")
			_, err := kubectl("delete", "secret", selfCluster+"-ca")
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				g.Expect(secretValue(g, selfCluster+"-ca", "ca.crt")).NotTo(Equal(oldCA))
				g.Expect(clusterState(selfCluster)).To(Equal("true/True"))
				g.Expect(secretValue(g, selfCluster+"-credentials", "ca.crt")).
					To(Equal(secretValue(g, selfCluster+"-ca", "ca.crt")))
				verifyFull(g, selfCluster)
			}).Should(Succeed())
		})

		It("removes the self-managed Secrets with the Cluster", func() {
			_, err := kubectl("delete", "cluster", selfCluster, "--timeout=2m")
			Expect(err).NotTo(HaveOccurred())
			Eventually(gone("secret", selfCluster+"-ca")).Should(BeEmpty())
			Eventually(gone("secret", selfCluster+"-server-cert")).Should(BeEmpty())
		})
	})
}
