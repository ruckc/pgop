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

// RegisterGrantsTests adds specs for Database.spec.grants,
// Database.spec.settings and Role.spec.parameterGrants (issue #24). They use
// example-cluster, created by the Custom Resources specs.
func RegisterGrantsTests() {
	Context("Grants and settings", Ordered, func() {
		SetDefaultEventuallyTimeout(2 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		const clusterName = "example-cluster"

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
				"sh", "-c", `psql -U "$POSTGRES_USER" -d postgres -tAc "$0"`, query))
			return strings.TrimSpace(out), err
		}

		query := func(g Gomega, q string) string {
			out, err := psql(q)
			g.Expect(err).NotTo(HaveOccurred())
			return out
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

		// dbSettings returns the database's ALTER DATABASE ... SET entries.
		const dbSettings = `SELECT coalesce(array_to_string(s.setconfig, '|'), '') FROM pg_database d ` +
			`LEFT JOIN pg_db_role_setting s ON s.setdatabase = d.oid AND s.setrole = 0 WHERE d.datname = 'grant_db'`
		// dbACL returns the database's ACL.
		const dbACL = `SELECT coalesce(datacl::text, '') FROM pg_database WHERE datname = 'grant_db'`

		AfterAll(func() {
			for _, res := range []string{
				"database.pgop.ruck.io/grant-db", "role.pgop.ruck.io/grant-app", "role.pgop.ruck.io/grant-param",
			} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found",
					"--wait=true", "--timeout=2m", res))
			}
		})

		It("applies, narrows and revokes database grants and settings", func() {
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: grant-app
spec:
  clusterRef:
    name: example-cluster
  roleName: grant_app
  login: false
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: grant-db
spec:
  clusterRef:
    name: example-cluster
  databaseName: grant_db
  grants:
    - role: grant_app
      privileges: [CONNECT, CREATE, TEMP]
      withGrantOption: true
  settings:
    work_mem: 64MB
    search_path: '"$user", public, App'
    myapp.note: "it's \\ quoted"
`)
			waitReady("role.pgop.ruck.io/grant-app", "database.pgop.ruck.io/grant-db")

			By("verifying the grants and settings are applied")
			Eventually(func(g Gomega) {
				g.Expect(query(g, `SELECT has_database_privilege('grant_app', 'grant_db', 'CREATE WITH GRANT OPTION')`)).To(Equal("t"))
				g.Expect(query(g, `SELECT has_database_privilege('grant_app', 'grant_db', 'TEMPORARY')`)).To(Equal("t"))
				settings := query(g, dbSettings)
				g.Expect(settings).To(ContainSubstring("work_mem=64MB"))
				g.Expect(settings).To(ContainSubstring(`search_path="$user", public, app`))
				g.Expect(settings).To(ContainSubstring(`myapp.note=it's \ quoted`))
			}).Should(Succeed())

			By("letting the grantee pass its privileges on (dependent privileges)")
			_, err := psql(`CREATE ROLE grant_dep NOLOGIN`)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _, _ = psql(`DROP ROLE IF EXISTS grant_dep`) })
			_, err = psql(`SET ROLE grant_app; GRANT CREATE, CONNECT ON DATABASE grant_db TO grant_dep`)
			Expect(err).NotTo(HaveOccurred())

			By("narrowing the grant, dropping settings and asking for a superuser-only one")
			patch("database.pgop.ruck.io/grant-db",
				`{"spec":{"grants":[{"role":"grant_app","privileges":["CONNECT"]}],`+
					`"settings":{"search_path":null,"myapp.note":null,"work_mem":"32MB","log_statement":"none"}}}`)
			Eventually(func(g Gomega) {
				acl := query(g, dbACL)
				g.Expect(acl).To(ContainSubstring("grant_app=c/"), "CONNECT without grant option expected, got %s", acl)
				g.Expect(acl).NotTo(ContainSubstring("grant_app=c*"))
				g.Expect(query(g, `SELECT has_database_privilege('grant_app', 'grant_db', 'CREATE')`)).To(Equal("f"))
				// The revokes cascaded to what grant_app had passed on.
				g.Expect(acl).NotTo(ContainSubstring("grant_dep="))
				// log_statement is refused; the other settings are applied.
				g.Expect(query(g, dbSettings)).To(Equal("work_mem=32MB"))
				reason, err := utils.Run(exec.Command("kubectl", "get", "database.pgop.ruck.io/grant-db", "-n", namespace,
					"-o", `jsonpath={.status.conditions[?(@.type=="Available")].reason}`))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(reason)).To(Equal("SettingNotAllowed"))
			}).Should(Succeed())

			By("removing the grants and settings entirely")
			patch("database.pgop.ruck.io/grant-db", `{"spec":{"grants":null,"settings":null}}`)
			Eventually(func(g Gomega) {
				g.Expect(query(g, dbACL)).NotTo(ContainSubstring("grant_app="))
				g.Expect(query(g, dbSettings)).To(BeEmpty())
				out, err := utils.Run(exec.Command("kubectl", "get", "database.pgop.ruck.io/grant-db", "-n", namespace,
					"-o", "jsonpath={.status.managedGrants}{.status.managedSettings}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}).Should(Succeed())
			waitReady("database.pgop.ruck.io/grant-db")
		})

		It("drops a Role that a Database still grants privileges to", func() {
			patch("database.pgop.ruck.io/grant-db", `{"spec":{"grants":[{"role":"grant_app","privileges":["CONNECT","CREATE"]}]}}`)
			Eventually(func(g Gomega) {
				g.Expect(query(g, dbACL)).To(ContainSubstring("grant_app="))
			}).Should(Succeed())

			By("deleting the grantee Role")
			_, err := utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m",
				"role.pgop.ruck.io/grant-app"))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(query(g, `SELECT count(*) FROM pg_roles WHERE rolname = 'grant_app'`)).To(Equal("0"))
			}).Should(Succeed())
		})

		It("grants and revokes SET on parameters", func() {
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: grant-param
spec:
  clusterRef:
    name: example-cluster
  roleName: grant_param
  login: false
  parameterGrants:
    - parameter: log_statement
    - parameter: myapp.tenant_id
      withGrantOption: true
`)
			waitReady("role.pgop.ruck.io/grant-param")

			By("verifying the role may SET the superuser-only parameter")
			Eventually(func(g Gomega) {
				g.Expect(query(g, `SELECT has_parameter_privilege('grant_param', 'log_statement', 'SET')`)).To(Equal("t"))
				g.Expect(query(g, `SELECT has_parameter_privilege('grant_param', 'myapp.tenant_id', 'SET WITH GRANT OPTION')`)).To(Equal("t"))
				// psql prints the SET command tags first; SHOW's value is the last line.
				out := query(g, `SET ROLE grant_param; SET log_statement = 'all'; SHOW log_statement`)
				lines := strings.Split(out, "\n")
				g.Expect(strings.TrimSpace(lines[len(lines)-1])).To(Equal("all"))
			}).Should(Succeed())

			By("removing a parameter grant")
			patch("role.pgop.ruck.io/grant-param", `{"spec":{"parameterGrants":[{"parameter":"myapp.tenant_id"}]}}`)
			Eventually(func(g Gomega) {
				g.Expect(query(g, `SELECT has_parameter_privilege('grant_param', 'log_statement', 'SET')`)).To(Equal("f"))
				g.Expect(query(g, `SELECT has_parameter_privilege('grant_param', 'myapp.tenant_id', 'SET WITH GRANT OPTION')`)).To(Equal("f"))
				g.Expect(query(g, `SELECT has_parameter_privilege('grant_param', 'myapp.tenant_id', 'SET')`)).To(Equal("t"))
			}).Should(Succeed())
			_, err := psql(`SET ROLE grant_param; SET log_statement = 'all'`)
			Expect(err).To(HaveOccurred(), "SET log_statement must fail once the grant is revoked")

			By("deleting the role, which revokes its remaining parameter grants first")
			_, err = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m",
				"role.pgop.ruck.io/grant-param"))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(query(g, `SELECT count(*) FROM pg_roles WHERE rolname = 'grant_param'`)).To(Equal("0"))
			}).Should(Succeed())
		})
	})
}
