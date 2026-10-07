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

// RegisterTLSTests adds server TLS (spec.tls, issue #22 phase 1) specs into
// the caller's Describe/Context block. Must be called inside a Describe/Context
// that has already deployed the controller into `namespace`. cert-manager is
// installed by the suite (see e2e_suite_test.go).
func RegisterTLSTests() {
	Context("TLS", Ordered, func() {
		SetDefaultEventuallyTimeout(5 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		const (
			clusterName = "tls-cluster"
			tlsSecret   = "tls-cluster-tls"
			roleName    = "tls-app"
			dbName      = "tls-app-db"
		)
		fqdn := fmt.Sprintf("%s.%s.svc.cluster.local", clusterName, namespace)

		apply := func(manifest string) {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		}

		get := func(args ...string) string {
			out, err := utils.Run(exec.Command("kubectl", append([]string{"get", "-n", namespace}, args...)...))
			Expect(err).NotTo(HaveOccurred())
			return strings.TrimSpace(out)
		}

		secretValue := func(secret, key string) string {
			escaped := strings.ReplaceAll(key, ".", `\.`)
			b64 := get("secret", secret, "-o", "jsonpath={.data."+escaped+"}")
			v, err := base64.StdEncoding.DecodeString(b64)
			Expect(err).NotTo(HaveOccurred())
			return string(v)
		}

		// psqlTCP runs psql inside the PostgreSQL pod over TCP. hostaddr pins
		// the connection to the pod itself while host keeps the Service name,
		// which verify-full checks against the certificate SANs.
		psqlTCP := func(user, password, sslMode, query string) (string, error) {
			conn := fmt.Sprintf("host=%s hostaddr=127.0.0.1 user=%s dbname=postgres sslmode=%s sslrootcert=/etc/pgop/tls/ca.crt",
				fqdn, user, sslMode)
			out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
				"env", "PGPASSWORD="+password, "psql", conn, "-tAc", query))
			return strings.TrimSpace(out), err
		}

		AfterAll(func() {
			for _, res := range []string{
				"database.pgop.ruck.io/" + dbName, "role.pgop.ruck.io/" + roleName, "cluster/" + clusterName,
				"certificate/" + tlsSecret, "issuer/tls-ca-issuer", "certificate/tls-ca", "issuer/tls-selfsigned",
			} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found", "--wait=false", res))
			}
			_, _ = utils.Run(exec.Command("kubectl", "delete", "pvc", "-n", namespace, "--ignore-not-found",
				"data-"+clusterName+"-0"))
		})

		It("issues a server certificate with cert-manager", func() {
			apply(fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: tls-selfsigned
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: tls-ca
spec:
  isCA: true
  commonName: pgop-e2e-ca
  secretName: tls-ca
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: tls-selfsigned
    kind: Issuer
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: tls-ca-issuer
spec:
  ca:
    secretName: tls-ca
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %[1]s
spec:
  secretName: %[1]s
  dnsNames:
    - %[2]s
    - %[3]s.%[4]s.svc
    - %[3]s
  usages: [server auth, digital signature, key encipherment]
  issuerRef:
    name: tls-ca-issuer
    kind: Issuer
`, tlsSecret, fqdn, clusterName, namespace))

			_, err := utils.Run(exec.Command("kubectl", "wait", "-n", namespace, "--for=condition=Ready",
				"certificate/"+tlsSecret, "--timeout=3m"))
			Expect(err).NotTo(HaveOccurred())
		})

		It("serves TLS with the certificate and reports TLSReady", func() {
			apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: %s
spec:
  storage:
    retainPolicy: Delete
  tls:
    secretName: %s
`, clusterName, tlsSecret))

			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "cluster", clusterName, "-n", namespace,
					"-o", `jsonpath={.status.ready}/{.status.conditions[?(@.type=="TLSReady")].status}`))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("true/True"))
			}).Should(Succeed())

			Expect(get("cluster", clusterName, "-o", "jsonpath={.status.tlsSecretHash}")).NotTo(BeEmpty())
			Expect(get("configmap", clusterName+"-hba", "-o", "name")).NotTo(BeEmpty())

			By("advertising verify-full and the CA in the credentials Secret")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "secret", clusterName+"-credentials", "-n", namespace,
					"-o", "jsonpath={.data.sslmode}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal(base64.StdEncoding.EncodeToString([]byte("verify-full"))))
			}).Should(Succeed())
			Expect(secretValue(clusterName+"-credentials", "ca.crt")).To(ContainSubstring("BEGIN CERTIFICATE"))
			Expect(secretValue(clusterName+"-credentials", "uri")).To(HaveSuffix("sslmode=verify-full"))

			password := secretValue(clusterName+"-credentials", "password")

			By("accepting verify-full connections and encrypting them")
			Eventually(func(g Gomega) {
				out, err := psqlTCP("pgop_operator", password, "verify-full",
					"SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("t"))
			}).Should(Succeed())

			By("rejecting non-TLS TCP connections (requireTLS defaults to true)")
			out, err := psqlTCP("pgop_operator", password, "disable", "SELECT 1")
			Expect(err).To(HaveOccurred(), "plaintext connection unexpectedly succeeded: %s", out)
			Expect(err.Error()).To(ContainSubstring("no encryption"))
		})

		It("reconciles a Role and Database over verify-full", func() {
			apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: %[1]s
spec:
  clusterRef:
    name: %[3]s
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: %[2]s
spec:
  clusterRef:
    name: %[3]s
  owner: %[1]s
`, roleName, dbName, clusterName))

			for _, res := range []string{"role.pgop.ruck.io/" + roleName, "database.pgop.ruck.io/" + dbName} {
				Eventually(func(g Gomega) {
					out, err := utils.Run(exec.Command("kubectl", "get", res, "-n", namespace, "-o", "jsonpath={.status.ready}"))
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("true"), res+" not ready")
				}).Should(Succeed())
			}

			roleSecret := clusterName + "-" + roleName + "-credentials"
			Expect(secretValue(roleSecret, "sslmode")).To(Equal("verify-full"))
			Expect(secretValue(roleSecret, "ca.crt")).To(ContainSubstring("BEGIN CERTIFICATE"))
			dbSecret := dbName + "-" + roleName + "-credentials"
			Expect(secretValue(dbSecret, "uri")).To(HaveSuffix("/" + dbName + "?sslmode=verify-full"))

			By("letting the application role connect with verify-full")
			out, err := psqlTCP(secretValue(roleSecret, "username"), secretValue(roleSecret, "password"), "verify-full",
				"SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("t"))
		})

		It("reverts to plaintext when spec.tls is removed", func() {
			_, err := utils.Run(exec.Command("kubectl", "patch", "cluster", clusterName, "-n", namespace,
				"--type=json", "-p", `[{"op":"remove","path":"/spec/tls"}]`))
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "secret", clusterName+"-credentials", "-n", namespace,
					"-o", "jsonpath={.data.sslmode}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal(base64.StdEncoding.EncodeToString([]byte("disable"))))
			}).Should(Succeed())
			Expect(get("secret", clusterName+"-credentials", "-o", `jsonpath={.data.ca\.crt}`)).To(BeEmpty())

			By("waiting for the pod to restart without TLS and accept plaintext connections")
			password := secretValue(clusterName+"-credentials", "password")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
					"env", "PGPASSWORD="+password, "psql", "host=127.0.0.1 user=pgop_operator dbname=postgres sslmode=disable",
					"-tAc", "SHOW ssl"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("off"))
			}).Should(Succeed())

			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "configmap", clusterName+"-hba", "-n", namespace,
					"--ignore-not-found", "-o", "name"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(BeEmpty())
			}).Should(Succeed())
		})
	})
}
