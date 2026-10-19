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
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ruckc/pgop/test/utils"
)

// RegisterVolumePermissionsTests adds the PG<=17 volume ownership specs
// (issue #65) into the caller's Describe/Context block, which must have
// deployed the controller into `namespace`. Kind's default local-path
// provisioner ignores fsGroup and hands out a root-owned directory.
func RegisterVolumePermissionsTests() {
	Context("Volume permissions", Ordered, func() {
		SetDefaultEventuallyTimeout(5 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		kubectl := func(args ...string) (string, error) {
			out, err := utils.Run(exec.Command("kubectl", append([]string{"-n", namespace}, args...)...))
			return strings.TrimSpace(out), err
		}
		apply := func(manifest string) error {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			return err
		}

		versions := map[string]string{"volperm-16": "postgres:16", "volperm-17": "postgres:17"}

		// The PG<=17 root init container adds CHOWN/FOWNER/DAC_OVERRIDE, which
		// the namespace's "restricted" Pod Security level rejects. Relax to
		// "baseline" (which permits those capabilities) for these specs only.
		BeforeAll(func() {
			_, err := kubectl("label", "ns", namespace, "--overwrite",
				"pod-security.kubernetes.io/enforce=baseline")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			_, _ = kubectl("label", "ns", namespace, "--overwrite",
				"pod-security.kubernetes.io/enforce=restricted")
			for name := range versions {
				_, _ = kubectl("delete", "cluster", name, "--ignore-not-found", "--wait=false")
			}
		})

		for name, image := range versions {
			It("starts a "+image+" Cluster on a volume not owned by uid 999", func() {
				Expect(apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: %s
spec:
  image: %s
  storage:
    retainPolicy: Delete
`, name, image))).To(Succeed())

				Eventually(func(g Gomega) {
					out, err := kubectl("get", "cluster", name, "-o", "jsonpath={.status.ready}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(Equal("true"))
				}).Should(Succeed())

				out, err := kubectl("exec", name+"-0", "-c", "postgresql", "--",
					"psql", "-U", "pgop_operator", "-d", "postgres", "-tAc", "SELECT 1")
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(Equal("1"))

				out, err = kubectl("exec", name+"-0", "-c", "postgresql", "--",
					"stat", "-c", "%u", "/var/lib/postgresql/data")
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(Equal("999"))

				out, err = kubectl("get", "statefulset", name, "-o",
					"jsonpath={.spec.template.spec.containers[0].securityContext.capabilities.add}")
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(BeEmpty())
			})
		}
	})
}
