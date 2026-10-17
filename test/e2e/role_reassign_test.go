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
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ruckc/pgop/test/utils"
)

// RegisterRoleReassignTests adds specs for Role spec.deletion.reassignOwnedTo
// (issue #24). They use example-cluster.
func RegisterRoleReassignTests() {
	Context("Role deletion with reassignOwnedTo", Ordered, func() {
		SetDefaultEventuallyTimeout(2 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		const clusterName = "example-cluster"

		kubectlApply := func(manifest string) {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		}
		psql := func(query string) (string, error) {
			out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
				"sh", "-c", `psql -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 -tAc "$0"`, query))
			return strings.TrimSpace(out), err
		}
		jsonpath := func(g Gomega, res, path string) string {
			out, err := utils.Run(exec.Command("kubectl", "get", res, "-n", namespace, "-o", "jsonpath="+path))
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}
		roleManifest := func(name, extra string) string {
			return `
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: ` + name + `
spec:
  clusterRef:
    name: example-cluster
` + extra
		}
		waitReady := func(res string) {
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, res, "{.status.ready}")).To(Equal("true"))
			}).Should(Succeed())
		}

		AfterAll(func() {
			for _, r := range []string{"rsg-old", "rsg-nomember", "rsg-target"} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found",
					"--wait=true", "--timeout=2m", "role.pgop.ruck.io/"+r))
			}
			_, _ = psql(`DROP TABLE IF EXISTS rsg_t`)
			_, _ = psql(`DROP TABLE IF EXISTS rsg_n`)
			_, _ = psql(`DROP ROLE IF EXISTS rsg_old`)
			_, _ = psql(`DROP ROLE IF EXISTS rsg_nomember`)
			_, _ = psql(`DROP ROLE IF EXISTS rsg_target`)
		})

		It("reassigns owned objects to a role the deleted role is a member of", func() {
			kubectlApply(roleManifest("rsg-target", "  login: false\n"))
			waitReady("role.pgop.ruck.io/rsg-target")
			kubectlApply(roleManifest("rsg-old", "  memberships:\n    - role: rsg-target\n  deletion:\n    reassignOwnedTo: rsg-target\n"))
			waitReady("role.pgop.ruck.io/rsg-old")

			_, err := psql(`CREATE TABLE rsg_t (id int); ALTER TABLE rsg_t OWNER TO rsg_old`)
			Expect(err).NotTo(HaveOccurred())

			_, err = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m", "role.pgop.ruck.io/rsg-old"))
			Expect(err).NotTo(HaveOccurred())

			out, err := psql(`SELECT tableowner FROM pg_tables WHERE tablename = 'rsg_t'`)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("rsg_target"))
		})

		It("blocks the deletion when the role is not a member of the target", func() {
			kubectlApply(roleManifest("rsg-nomember", "  deletion:\n    reassignOwnedTo: rsg-target\n"))
			waitReady("role.pgop.ruck.io/rsg-nomember")
			_, err := psql(`CREATE TABLE rsg_n (id int); ALTER TABLE rsg_n OWNER TO rsg_nomember`)
			Expect(err).NotTo(HaveOccurred())

			_, err = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=false", "role.pgop.ruck.io/rsg-nomember"))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, "role.pgop.ruck.io/rsg-nomember",
					`{.status.conditions[?(@.type=="Available")].reason}`)).To(Equal("RoleDropBlocked"))
				g.Expect(jsonpath(g, "role.pgop.ruck.io/rsg-nomember",
					`{.status.conditions[?(@.type=="Available")].message}`)).To(ContainSubstring("not a member"))
			}).Should(Succeed())
			out, err := psql(`SELECT tableowner FROM pg_tables WHERE tablename = 'rsg_n'`)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("rsg_nomember"))
		})
	})
}
