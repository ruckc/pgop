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

// RegisterPasswordTests adds specs for Role.passwordSecretRef and password
// rotation (issue #25). They use example-cluster, created by the Custom
// Resources specs.
func RegisterPasswordTests() {
	Context("Role passwords", Ordered, func() {
		SetDefaultEventuallyTimeout(2 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		const clusterName = "example-cluster"

		apply := func(manifest string) {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		}

		secretValue := func(g Gomega, secret, key string) string {
			out, err := utils.Run(exec.Command("kubectl", "get", "secret", secret, "-n", namespace,
				"-o", "jsonpath={.data."+key+"}"))
			g.Expect(err).NotTo(HaveOccurred())
			v, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
			g.Expect(err).NotTo(HaveOccurred())
			return string(v)
		}

		waitReady := func(resources ...string) {
			for _, res := range resources {
				Eventually(func(g Gomega) {
					out, err := utils.Run(exec.Command("kubectl", "get", res, "-n", namespace,
						"-o", "jsonpath={.status.ready}"))
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("true"), res+" not ready")
				}).Should(Succeed())
			}
		}

		// login connects as user with password over TCP to the pod IP, so the
		// connection matches the scram-sha-256 pg_hba rule rather than the
		// trust rules for localhost.
		var podIP string
		login := func(user, password string) error {
			conn := fmt.Sprintf("host=%s user=%s dbname=postgres sslmode=disable connect_timeout=5", podIP, user)
			_, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
				"env", "PGPASSWORD="+password, "psql", "-w", conn, "-tAc", "SELECT 1"))
			return err
		}

		BeforeAll(func() {
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", clusterName+"-0", "-n", namespace,
				"-o", "jsonpath={.status.podIP}"))
			Expect(err).NotTo(HaveOccurred())
			podIP = strings.TrimSpace(out)
			Expect(podIP).NotTo(BeEmpty())
		})

		AfterAll(func() {
			// Databases first: a role cannot be dropped while it owns a database.
			for _, res := range []string{
				"database.pgop.ruck.io/pw-ref-db", "database.pgop.ruck.io/pw-rot-db",
				"role.pgop.ruck.io/pw-ref", "role.pgop.ruck.io/pw-rot", "secret/pw-ref-source",
			} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found",
					"--wait=true", "--timeout=2m", res))
			}
		})

		It("uses the password from passwordSecretRef and follows changes to it", func() {
			apply(`
apiVersion: v1
kind: Secret
metadata:
  name: pw-ref-source
stringData:
  pass: first-password-1
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pw-ref
spec:
  clusterRef:
    name: example-cluster
  roleName: pw_ref
  passwordSecretRef:
    name: pw-ref-source
    key: pass
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: pw-ref-db
spec:
  clusterRef:
    name: example-cluster
  databaseName: pw_ref_db
  owner: pw-ref
`)
			waitReady("role.pgop.ruck.io/pw-ref", "database.pgop.ruck.io/pw-ref-db")

			By("logging in with the referenced password")
			Expect(login("pw_ref", "first-password-1")).To(Succeed())
			Expect(login("pw_ref", "wrong-password")).NotTo(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(secretValue(g, clusterName+"-pw-ref-credentials", "password")).To(Equal("first-password-1"))
				g.Expect(secretValue(g, "pw-ref-db-pw-ref-credentials", "password")).To(Equal("first-password-1"))
			}).Should(Succeed())

			By("changing the referenced Secret")
			_, err := utils.Run(exec.Command("kubectl", "patch", "secret", "pw-ref-source", "-n", namespace,
				"--type=merge", "-p", `{"stringData":{"pass":"second-password-2"}}`))
			Expect(err).NotTo(HaveOccurred())

			By("verifying the new password works, the old one does not, and both Secrets follow")
			Eventually(func(g Gomega) {
				g.Expect(login("pw_ref", "second-password-2")).To(Succeed())
			}).Should(Succeed())
			Expect(login("pw_ref", "first-password-1")).NotTo(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(secretValue(g, clusterName+"-pw-ref-credentials", "password")).To(Equal("second-password-2"))
				g.Expect(secretValue(g, clusterName+"-pw-ref-credentials", "uri")).To(ContainSubstring(":second-password-2@"))
				g.Expect(secretValue(g, "pw-ref-db-pw-ref-credentials", "password")).To(Equal("second-password-2"))
				g.Expect(secretValue(g, "pw-ref-db-pw-ref-credentials", "uri")).To(ContainSubstring(":second-password-2@"))
			}).Should(Succeed())
		})

		It("rotates a generated password on request and the Database Secret follows", func() {
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pw-rot
spec:
  clusterRef:
    name: example-cluster
  roleName: pw_rot
  passwordRotation:
    every: 720h
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: pw-rot-db
spec:
  clusterRef:
    name: example-cluster
  databaseName: pw_rot_db
  owner: pw-rot
`)
			waitReady("role.pgop.ruck.io/pw-rot", "database.pgop.ruck.io/pw-rot-db")

			var oldPassword string
			Eventually(func(g Gomega) {
				oldPassword = secretValue(g, clusterName+"-pw-rot-credentials", "password")
				g.Expect(oldPassword).NotTo(BeEmpty())
				g.Expect(secretValue(g, "pw-rot-db-pw-rot-credentials", "password")).To(Equal(oldPassword))
			}).Should(Succeed())
			Expect(login("pw_rot", oldPassword)).To(Succeed())

			out, err := utils.Run(exec.Command("kubectl", "get", "role.pgop.ruck.io", "pw-rot", "-n", namespace,
				"-o", "jsonpath={.status.passwordRotatedAt}"))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(BeEmpty(), "passwordRotatedAt not recorded")

			By("requesting a rotation with the rotate-password annotation")
			_, err = utils.Run(exec.Command("kubectl", "annotate", "role.pgop.ruck.io", "pw-rot", "-n", namespace,
				"--overwrite", "pgop.ruck.io/rotate-password=e2e-1"))
			Expect(err).NotTo(HaveOccurred())

			var newPassword string
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "role.pgop.ruck.io", "pw-rot", "-n", namespace,
					"-o", "jsonpath={.status.passwordRotationRequest}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("e2e-1"))
				newPassword = secretValue(g, clusterName+"-pw-rot-credentials", "password")
				g.Expect(newPassword).NotTo(Equal(oldPassword))
			}).Should(Succeed())

			By("verifying the new password works and the old one does not")
			Expect(login("pw_rot", newPassword)).To(Succeed())
			Expect(login("pw_rot", oldPassword)).NotTo(Succeed())

			By("verifying the Database Secret follows the rotated password")
			Eventually(func(g Gomega) {
				g.Expect(secretValue(g, "pw-rot-db-pw-rot-credentials", "password")).To(Equal(newPassword))
				g.Expect(secretValue(g, "pw-rot-db-pw-rot-credentials", "uri")).To(ContainSubstring(":" + newPassword + "@"))
			}).Should(Succeed())

			By("verifying a PasswordRotated Event was emitted")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "events", "-n", namespace,
					"--field-selector", "reason=PasswordRotated,involvedObject.name=pw-rot", "-o", "name"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty())
			}).Should(Succeed())
		})

		It("rejects passwordRotation combined with passwordSecretRef", func() {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "--dry-run=server", "-f", "-")
			cmd.Stdin = newStringReader(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pw-invalid
spec:
  clusterRef:
    name: example-cluster
  passwordSecretRef:
    name: pw-ref-source
    key: pass
  passwordRotation:
    every: 24h
`)
			out, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred())
			Expect(err.Error() + out).To(ContainSubstring("cannot be combined with passwordSecretRef"))
		})
	})
}
