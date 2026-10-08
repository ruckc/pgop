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

// tlsRustfsNamespace holds the RustFS used by the TLS backup specs. It is
// separate from rustfsNamespace, which the backup specs delete when they
// finish.
const tlsRustfsNamespace = "rustfs-tls"

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
				"restore.pgop.ruck.io/tls-restore", "backuprun.pgop.ruck.io/tls-restore-src", "backup.pgop.ruck.io/tls-backup",
				"database.pgop.ruck.io/" + dbName, "role.pgop.ruck.io/" + roleName, "cluster/" + clusterName,
				"certificate/" + tlsSecret, "issuer/tls-ca-issuer", "certificate/tls-ca", "issuer/tls-selfsigned",
			} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found", "--wait=false", res))
			}
			_, _ = utils.Run(exec.Command("kubectl", "delete", "pvc", "-n", namespace, "--ignore-not-found",
				"data-"+clusterName+"-0"))
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", tlsRustfsNamespace, "--ignore-not-found", "--wait=false"))
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

		It("backs up and restores a Database with pg_dump/pg_restore over verify-full", func() {
			setupRustFS(tlsRustfsNamespace)
			applyRustFSCredentials()

			password := secretValue(clusterName+"-credentials", "password")
			psqlDB := func(query string) (string, error) {
				conn := fmt.Sprintf("host=%s hostaddr=127.0.0.1 user=pgop_operator dbname=%s sslmode=verify-full sslrootcert=/etc/pgop/tls/ca.crt",
					fqdn, dbName)
				out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
					"env", "PGPASSWORD="+password, "psql", conn, "-v", "ON_ERROR_STOP=1", "-tAc", query))
				return strings.TrimSpace(out), err
			}
			_, err := psqlDB("CREATE TABLE tls_backup_check (v int); INSERT INTO tls_backup_check VALUES (42)")
			Expect(err).NotTo(HaveOccurred())

			apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Backup
metadata:
  name: tls-backup
spec:
  type: logical
  databaseRef:
    name: %s
  schedule: "0 3 * * *"
  retention:
    disabled: true
  destination:
    type: s3
    s3:
      bucket: pgop-backups
      prefix: tls
      region: us-east-1
      endpoint: http://rustfs.%s.svc.cluster.local:9000
      credentialsSecretRef:
        name: rustfs-credentials
`, dbName, tlsRustfsNamespace))

			By("taking the sslmode and CA for pg_dump from the credentials Secret")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "cronjob", "tls-backup-data", "-n", namespace, "-o",
					`jsonpath={.spec.jobTemplate.spec.template.spec.initContainers[0].env[?(@.name=="PGSSLMODE")].valueFrom.secretKeyRef.key}`))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("sslmode"))
			}).Should(Succeed())

			By("running a data backup Job against the requireTLS server")
			_, err = utils.Run(exec.Command("kubectl", "create", "job", "tls-backup-manual",
				"--from=cronjob/tls-backup-data", "-n", namespace))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "job", "tls-backup-manual", "-n", namespace,
					"-o", "jsonpath={.status.succeeded}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("1"), "TLS backup job did not succeed")
			}).Should(Succeed())

			By("confirming pg_dump connected with sslmode=verify-full")
			out, err := utils.Run(exec.Command("kubectl", "logs", "job/tls-backup-manual", "-n", namespace, "-c", "pg-dump"))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("sslmode=verify-full"))

			out, err = utils.Run(exec.Command("kubectl", "logs", "job/tls-backup-manual", "-n", namespace, "-c", "s3-upload"))
			Expect(err).NotTo(HaveOccurred())
			location := parseUploadedLocation(out)
			Expect(location).To(HavePrefix("s3://pgop-backups/tls/data/"))

			By("restoring the dump with pg_restore over verify-full")
			_, err = psqlDB("DELETE FROM tls_backup_check")
			Expect(err).NotTo(HaveOccurred())
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: BackupRun
metadata:
  name: tls-restore-src
spec:
  backupRef:
    name: tls-backup
  type: data
`)
			_, err = utils.Run(exec.Command("kubectl", "patch", "backuprun.pgop.ruck.io", "tls-restore-src",
				"-n", namespace, "--subresource=status", "--type=merge",
				"-p", fmt.Sprintf(`{"status":{"location":%q}}`, location)))
			Expect(err).NotTo(HaveOccurred())
			apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Restore
metadata:
  name: tls-restore
spec:
  type: logical
  backupRunRef:
    name: tls-restore-src
  clusterRef:
    name: %s
  databaseRef:
    name: %s
`, clusterName, dbName))
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "restore.pgop.ruck.io", "tls-restore",
					"-n", namespace, "-o", "jsonpath={.status.phase}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("Succeeded"), "TLS restore did not succeed")
			}).Should(Succeed())

			out, err = utils.Run(exec.Command("kubectl", "logs", "job/tls-restore-restore", "-n", namespace, "-c", "pg-restore"))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("sslmode=verify-full"))

			Expect(psqlDB("SELECT v FROM tls_backup_check")).To(Equal("42"))
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
