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

// RegisterRoleSettingsTests adds specs for Role.spec.settings and
// Role.spec.databaseSettings (issue #24, PR 3). They use example-cluster.
func RegisterRoleSettingsTests() {
	Context("Role settings", Ordered, func() {
		SetDefaultEventuallyTimeout(2 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		const (
			clusterName = "example-cluster"
			roleRes     = "role.pgop.ruck.io/"
			dbRes       = "database.pgop.ruck.io/"
		)

		apply := func(manifest string) {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		}

		patch := func(resource, body string) {
			_, err := utils.Run(exec.Command("kubectl", "patch", resource, "-n", namespace, "--type=merge", "-p", body))
			Expect(err).NotTo(HaveOccurred())
		}

		// psql runs a query as the superuser over the local socket.
		psql := func(query string) (string, error) {
			out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
				"sh", "-c", `psql -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 -tAc "$0"`, query))
			return strings.TrimSpace(out), err
		}
		// psqlAs runs a query as user in database db over the local socket,
		// so the role's own defaults apply.
		psqlAs := func(user, db, query string) (string, error) {
			out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
				"psql", "-U", user, "-d", db, "-v", "ON_ERROR_STOP=1", "-tAc", query))
			return strings.TrimSpace(out), err
		}
		query := func(g Gomega, q string) string {
			out, err := psql(q)
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}
		jsonpath := func(g Gomega, res, path string) string {
			out, err := utils.Run(exec.Command("kubectl", "get", res, "-n", namespace, "-o", "jsonpath="+path))
			g.Expect(err).NotTo(HaveOccurred())
			return strings.TrimSpace(out)
		}
		const availableReason = `{.status.conditions[?(@.type=="Available")].reason}`
		const availableMessage = `{.status.conditions[?(@.type=="Available")].message}`

		waitReady := func(res string) {
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, res, "{.status.ready}")).To(Equal("true"), res+" not ready")
			}).Should(Succeed())
		}

		// roleSettings returns the role's pg_db_role_setting entries, one
		// line per database ("*" for the role-wide entry), sorted.
		roleSettings := func(g Gomega, role string) string {
			return query(g, `SELECT coalesce(d.datname, '*') || ':' || array_to_string(s.setconfig, '|') `+
				`FROM pg_db_role_setting s JOIN pg_roles r ON r.oid = s.setrole `+
				`LEFT JOIN pg_database d ON d.oid = s.setdatabase WHERE r.rolname = '`+role+`' ORDER BY 1`)
		}

		AfterAll(func() {
			for _, res := range []string{dbRes + "rs-db", roleRes + "rs-app", roleRes + "rs-dba"} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found",
					"--wait=true", "--timeout=2m", res))
			}
			_, _ = psql(`DROP ROLE IF EXISTS rs_dba`)
		})

		It("applies, changes and resets role settings", func() {
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: rs-app
spec:
  clusterRef:
    name: example-cluster
  roleName: rs_app
  settings:
    statement_timeout: 30s
    search_path: '"$user", App'
    myapp.note: "it's \\ quoted"
  databaseSettings:
    - database: rs_db
      settings:
        work_mem: 12MB
`)

			By("staying Available while the database of databaseSettings does not exist")
			waitReady(roleRes + "rs-app")
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, roleRes+"rs-app", availableMessage)).
					To(ContainSubstring("pending until these databases exist: rs_db"))
				settings := roleSettings(g, "rs_app")
				g.Expect(settings).To(ContainSubstring("*:"))
				g.Expect(settings).To(ContainSubstring("statement_timeout=30s"))
				g.Expect(settings).To(ContainSubstring(`search_path="$user", app`))
				g.Expect(settings).To(ContainSubstring(`myapp.note=it's \ quoted`))
				g.Expect(settings).NotTo(ContainSubstring("rs_db:"))
			}).Should(Succeed())

			By("applying the databaseSettings once the database is created")
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: rs-db
spec:
  clusterRef:
    name: example-cluster
  databaseName: rs_db
  owner: rs-app
`)
			waitReady(dbRes + "rs-db")
			Eventually(func(g Gomega) {
				g.Expect(roleSettings(g, "rs_app")).To(ContainSubstring("rs_db:work_mem=12MB"))
				g.Expect(jsonpath(g, roleRes+"rs-app", availableMessage)).NotTo(ContainSubstring("pending"))
				g.Expect(jsonpath(g, roleRes+"rs-app", "{.status.managedDatabaseSettings[0].database}")).To(Equal("rs_db"))
			}).Should(Succeed())

			By("checking the settings apply to the role's own sessions")
			Eventually(func(g Gomega) {
				out, err := psqlAs("rs_app", "rs_db",
					`SELECT current_setting('statement_timeout') || '|' || current_setting('work_mem')`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("30s|12MB"))
				out, err = psqlAs("rs_app", "postgres", `SHOW work_mem`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(Equal("12MB"))
			}).Should(Succeed())

			By("changing a value, dropping one and asking for refused ones")
			patch(roleRes+"rs-app", `{"spec":{"settings":{"statement_timeout":"45s","search_path":null,`+
				`"log_statement":"none","myapp.note":"x"},`+
				`"databaseSettings":[{"database":"rs_db","settings":{"work_mem":"16MB","log_min_duration_statement":"0"}}]}}`)
			Eventually(func(g Gomega) {
				settings := roleSettings(g, "rs_app")
				g.Expect(settings).To(ContainSubstring("statement_timeout=45s"))
				g.Expect(settings).NotTo(ContainSubstring("search_path"))
				g.Expect(settings).NotTo(ContainSubstring("log_statement"))
				g.Expect(settings).To(ContainSubstring("rs_db:work_mem=16MB"))
				g.Expect(settings).NotTo(ContainSubstring("log_min_duration_statement"))
				g.Expect(jsonpath(g, roleRes+"rs-app", availableReason)).To(Equal("SettingNotAllowed"))
				msg := jsonpath(g, roleRes+"rs-app", availableMessage)
				g.Expect(msg).To(ContainSubstring(`log_statement has context "superuser"`))
				g.Expect(msg).To(ContainSubstring(`databaseSettings[rs_db]: log_min_duration_statement has context "superuser"`))
			}).Should(Succeed())

			By("rejecting denylisted parameters at the API server")
			_, err := utils.Run(exec.Command("kubectl", "patch", roleRes+"rs-app", "-n", namespace, "--type=merge",
				"-p", `{"spec":{"settings":{"session_authorization":"postgres"}}}`))
			Expect(err).To(HaveOccurred())

			By("resetting everything pgop set when the settings are removed")
			_, err = psql(`ALTER ROLE rs_app SET lock_timeout = '5s'`)
			Expect(err).NotTo(HaveOccurred())
			patch(roleRes+"rs-app", `{"spec":{"settings":null,"databaseSettings":null}}`)
			Eventually(func(g Gomega) {
				// A setting made outside pgop is never reset.
				g.Expect(roleSettings(g, "rs_app")).To(Equal("*:lock_timeout=5s"))
				g.Expect(jsonpath(g, roleRes+"rs-app", "{.status.managedSettings}{.status.managedDatabaseSettings}")).To(BeEmpty())
			}).Should(Succeed())
			waitReady(roleRes + "rs-app")
		})

		It("applies no settings to a role it does not manage", func() {
			_, err := psql(`CREATE ROLE rs_dba LOGIN`)
			Expect(err).NotTo(HaveOccurred())
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: rs-dba
spec:
  clusterRef:
    name: example-cluster
  roleName: rs_dba
  settings:
    statement_timeout: 1ms
  databaseSettings:
    - database: postgres
      settings:
        work_mem: 64kB
`)
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, roleRes+"rs-dba", availableReason)).To(Equal("RoleNotManaged"))
			}).Should(Succeed())
			Consistently(func(g Gomega) {
				g.Expect(roleSettings(g, "rs_dba")).To(BeEmpty())
				g.Expect(jsonpath(g, roleRes+"rs-dba", "{.status.managedSettings}{.status.managedDatabaseSettings}")).To(BeEmpty())
			}, 20*time.Second, 2*time.Second).Should(Succeed())
		})
	})
}
