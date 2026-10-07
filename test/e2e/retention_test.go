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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ruckc/pgop/test/utils"
)

// RegisterRetentionTests adds PVC retention (storage.retainPolicy) specs into
// the caller's Describe/Context block. Must be called inside a Describe/Context
// that has already deployed the controller into `namespace`.
func RegisterRetentionTests() {
	Context("PVC retention", Ordered, func() {
		SetDefaultEventuallyTimeout(5 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		applyCluster := func(name, retainPolicy string) {
			storage := ""
			if retainPolicy != "" {
				storage = fmt.Sprintf("\n  storage:\n    retainPolicy: %s", retainPolicy)
			}
			clusterYAML := fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 1%s
`, name, namespace, storage)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = newStringReader(clusterYAML)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Cluster %s", name)
		}

		waitReady := func(name string) {
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "cluster", name, "-n", namespace,
					"-o", "jsonpath={.status.ready}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("true"))
			}).Should(Succeed())
		}

		existingVolumeStatus := func(name string) (string, error) {
			return utils.Run(exec.Command("kubectl", "get", "cluster", name, "-n", namespace,
				"-o", `jsonpath={.status.conditions[?(@.type=="ExistingVolume")].status}`))
		}

		deleteClusterAndWait := func(name string) {
			_, err := utils.Run(exec.Command("kubectl", "delete", "cluster", name, "-n", namespace, "--wait=true"))
			Expect(err).NotTo(HaveOccurred())
			By("waiting for the owned StatefulSet, Secret and Pod to be garbage collected")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "statefulset,secret,pod", "-n", namespace,
					"-l", "app.kubernetes.io/instance="+name, "-o", "name"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(BeEmpty())
			}).Should(Succeed())
		}

		pvcExists := func(name string) bool {
			_, err := utils.Run(exec.Command("kubectl", "get", "pvc", "data-"+name+"-0", "-n", namespace))
			return err == nil
		}

		AfterAll(func() {
			for _, name := range []string{"retain-delete", "retain-keep"} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "cluster", name, "-n", namespace, "--ignore-not-found"))
				_, _ = utils.Run(exec.Command("kubectl", "delete", "pvc", "data-"+name+"-0", "-n", namespace,
					"--ignore-not-found"))
			}
		})

		It("should delete the PVC with the Cluster when retainPolicy is Delete", func() {
			const name = "retain-delete"
			applyCluster(name, "Delete")
			waitReady(name)
			Expect(pvcExists(name)).To(BeTrue())

			deleteClusterAndWait(name)

			By("waiting for the PVC to be deleted")
			Eventually(func() bool { return pvcExists(name) }).Should(BeFalse())
		})

		It("should keep the PVC by default and report ExistingVolume when the Cluster is recreated", func() {
			const name = "retain-keep"
			applyCluster(name, "")
			waitReady(name)

			out, err := existingVolumeStatus(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("False"))

			deleteClusterAndWait(name)

			By("verifying the PVC is retained")
			Consistently(func() bool { return pvcExists(name) }, 10*time.Second, time.Second).Should(BeTrue())

			By("recreating the Cluster with the same name")
			applyCluster(name, "")

			Eventually(func(g Gomega) {
				out, err := existingVolumeStatus(name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("True"))
			}).Should(Succeed())

			By("verifying a PreExistingPVC Warning event was recorded")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "events", "-n", namespace,
					"--field-selector", "reason=PreExistingPVC,involvedObject.name="+name, "-o", "name"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())
			}).Should(Succeed())

			waitReady(name)

			By("verifying the regenerated credentials authenticate against the old data directory")
			Eventually(func(g Gomega) {
				pwB64, err := utils.Run(exec.Command("kubectl", "get", "secret", name+"-credentials", "-n", namespace,
					"-o", "jsonpath={.data.password}"))
				g.Expect(err).NotTo(HaveOccurred())
				pw, err := base64.StdEncoding.DecodeString(pwB64)
				g.Expect(err).NotTo(HaveOccurred())
				out, err := utils.Run(exec.Command("kubectl", "exec", name+"-0", "-n", namespace, "--",
					"env", "PGPASSWORD="+string(pw), "psql", "-h", "127.0.0.1", "-U", "pgop_operator",
					"-d", "postgres", "-tAc", "SELECT 1"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("1"))
			}).Should(Succeed())
		})
	})
}
