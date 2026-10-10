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

// RegisterExtensionTests adds specs for Database.spec.extensions (issue
// #24, PR 4): the trusted/allowlist policy, cascade, version updates, grants
// on extension objects, the writable-schema check and removal. They use
// example-cluster and reset its rolePolicy at the end.
func RegisterExtensionTests() {
	Context("Extensions", Ordered, func() {
		SetDefaultEventuallyTimeout(2 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		const (
			clusterName = "example-cluster"
			clusterRes  = "cluster.pgop.ruck.io/" + clusterName
			dbRes       = "database.pgop.ruck.io/ext-db"
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
		setExtensions := func(extensions string) { patch(dbRes, `{"spec":{"extensions":`+extensions+`}}`) }
		setAllowed := func(allowed string) {
			patch(clusterRes, `{"spec":{"rolePolicy":{"allowedExtensions":`+allowed+`}}}`)
		}
		// psql runs a query as the superuser in ext_db over the local socket.
		psql := func(query string) (string, error) {
			out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
				"sh", "-c", `psql -U "$POSTGRES_USER" -d ext_db -v ON_ERROR_STOP=1 -tAc "$0"`, query))
			return strings.TrimSpace(out), err
		}
		query := func(g Gomega, q string) string {
			out, err := psql(q)
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}
		jsonpath := func(g Gomega, path string) string {
			out, err := utils.Run(exec.Command("kubectl", "get", dbRes, "-n", namespace, "-o", "jsonpath="+path))
			g.Expect(err).NotTo(HaveOccurred())
			return strings.TrimSpace(out)
		}
		expectReason := func(reason string, messageParts ...string) {
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, `{.status.conditions[?(@.type=="Available")].reason}`)).To(Equal(reason))
				msg := jsonpath(g, `{.status.conditions[?(@.type=="Available")].message}`)
				for _, part := range messageParts {
					g.Expect(msg).To(ContainSubstring(part))
				}
			}).Should(Succeed())
		}
		waitReady := func() {
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, "{.status.ready}")).To(Equal("true"))
			}).Should(Succeed())
		}
		// installed returns "version@schema" of an extension, "" when it is
		// not installed.
		installed := func(g Gomega, name string) string {
			return query(g, `SELECT coalesce(max(extversion || '@' || extnamespace::regnamespace::text), '') `+
				`FROM pg_extension WHERE extname = '`+name+`'`)
		}

		AfterAll(func() {
			for _, res := range []string{dbRes, "role.pgop.ruck.io/ext-owner", "role.pgop.ruck.io/ext-app"} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found",
					"--wait=true", "--timeout=2m", res))
			}
			_, _ = utils.Run(exec.Command("kubectl", "patch", clusterRes, "-n", namespace,
				"--type=json", "-p", `[{"op":"remove","path":"/spec/rolePolicy"}]`))
		})

		It("installs trusted extensions and refuses untrusted ones until the Cluster lists them", func() {
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: ext-owner
spec:
  clusterRef:
    name: example-cluster
  roleName: ext_owner
  login: false
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: ext-app
spec:
  clusterRef:
    name: example-cluster
  roleName: ext_app
  login: false
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: ext-db
spec:
  clusterRef:
    name: example-cluster
  databaseName: ext_db
  owner: ext-owner
  extensions:
    - name: pg_trgm
    - name: dblink
`)
			expectReason("ExtensionNotAllowed", "dblink")
			Eventually(func(g Gomega) {
				g.Expect(installed(g, "pg_trgm")).To(HaveSuffix("@public"))
				g.Expect(installed(g, "dblink")).To(BeEmpty())
				g.Expect(jsonpath(g, `{.status.extensions[?(@.name=="pg_trgm")].created}`)).To(Equal("true"))
			}).Should(Succeed())

			By("listing dblink: it still may not run in public, which the database owner can write to")
			setAllowed(`["dblink","earthdistance"]`)
			expectReason("ExtensionSchemaNotAllowed", "ext_owner")
			Eventually(func(g Gomega) { g.Expect(installed(g, "dblink")).To(BeEmpty()) }).Should(Succeed())

			By("installing dblink into a schema of its own, which pgop creates for the operator")
			setExtensions(`[{"name":"pg_trgm"},{"name":"dblink","schema":"dblink_s"}]`)
			waitReady()
			Eventually(func(g Gomega) {
				g.Expect(installed(g, "dblink")).To(HaveSuffix("@dblink_s"))
				g.Expect(query(g, `SELECT r.rolsuper FROM pg_namespace n JOIN pg_roles r ON r.oid = n.nspowner `+
					`WHERE n.nspname = 'dblink_s'`)).To(Equal("t"))
			}).Should(Succeed())
		})

		It("checks the dependencies of cascade before installing anything", func() {
			setExtensions(`[{"name":"pg_trgm"},{"name":"dblink","schema":"dblink_s"},{"name":"earthdistance","schema":"geo"}]`)
			expectReason("ExtensionDependencyMissing", "cube")
			setAllowed(`["dblink"]`)
			setExtensions(`[{"name":"pg_trgm"},{"name":"dblink","schema":"dblink_s"},` +
				`{"name":"earthdistance","schema":"geo","cascade":true}]`)
			expectReason("ExtensionNotAllowed", "earthdistance")
			Eventually(func(g Gomega) { g.Expect(installed(g, "cube")).To(BeEmpty()) }).Should(Succeed())

			setAllowed(`["dblink","earthdistance"]`)
			waitReady()
			Eventually(func(g Gomega) {
				g.Expect(installed(g, "cube")).To(HaveSuffix("@geo"))
				g.Expect(installed(g, "earthdistance")).To(HaveSuffix("@geo"))
			}).Should(Succeed())
		})

		It("updates an extension and refuses a downgrade", func() {
			setExtensions(`[{"name":"pg_trgm"},{"name":"hstore","version":"1.4","dropOnRemoval":true}]`)
			waitReady()
			Eventually(func(g Gomega) { g.Expect(installed(g, "hstore")).To(Equal("1.4@public")) }).Should(Succeed())
			setExtensions(`[{"name":"pg_trgm"},{"name":"hstore","version":"1.8","dropOnRemoval":true}]`)
			Eventually(func(g Gomega) {
				g.Expect(installed(g, "hstore")).To(Equal("1.8@public"))
				g.Expect(jsonpath(g, `{.status.extensions[?(@.name=="hstore")].version}`)).To(Equal("1.8"))
			}).Should(Succeed())
			waitReady()
			setExtensions(`[{"name":"pg_trgm"},{"name":"hstore","version":"1.5","dropOnRemoval":true}]`)
			expectReason("ExtensionDowngradeNotAllowed", "1.8")
			Expect(psql(`SELECT extversion FROM pg_extension WHERE extname = 'hstore'`)).To(Equal("1.8"))
		})

		It("grants EXECUTE on an extension's SQL functions only, and revokes it", func() {
			setExtensions(`[{"name":"pg_trgm"},{"name":"hstore","version":"1.8","dropOnRemoval":true},` +
				`{"name":"citext","grants":[{"role":"ext_app","functions":["EXECUTE"]}]}]`)
			waitReady()
			granted := func(g Gomega, lang string) string {
				return query(g, `SELECT count(*) FROM pg_proc p JOIN pg_depend d ON d.objid = p.oid `+
					`AND d.classid = 'pg_proc'::regclass `+
					`AND d.deptype = 'e' AND d.refobjid = (SELECT oid FROM pg_extension WHERE extname = 'citext') `+
					`JOIN pg_language l ON l.oid = p.prolang, aclexplode(p.proacl) a `+
					`WHERE l.lanname = '`+lang+`' AND a.grantee = 'ext_app'::regrole AND a.privilege_type = 'EXECUTE'`)
			}
			Eventually(func(g Gomega) {
				g.Expect(granted(g, "sql")).NotTo(Equal("0"))
				g.Expect(granted(g, "c")).To(Equal("0"))
				g.Expect(jsonpath(g, `{.status.extensions[?(@.name=="citext")].skippedObjects}`)).NotTo(BeEmpty())
				g.Expect(jsonpath(g, `{.status.managedExtensionGrants[0].kind}`)).To(Equal("functions"))
			}).Should(Succeed())

			By("removing the grant")
			setExtensions(`[{"name":"pg_trgm"},{"name":"hstore","version":"1.8","dropOnRemoval":true},{"name":"citext"}]`)
			Eventually(func(g Gomega) {
				g.Expect(granted(g, "sql")).To(Equal("0"))
				g.Expect(jsonpath(g, `{.status.managedExtensionGrants}`)).To(BeEmpty())
			}).Should(Succeed())
		})

		It("refuses a target schema other roles can write to", func() {
			_, err := psql(`CREATE SCHEMA shared AUTHORIZATION ext_owner; GRANT CREATE ON SCHEMA shared TO PUBLIC`)
			Expect(err).NotTo(HaveOccurred())
			setExtensions(`[{"name":"pg_trgm"},{"name":"hstore","version":"1.8","dropOnRemoval":true},{"name":"citext"},` +
				`{"name":"ltree","schema":"shared"}]`)
			expectReason("ExtensionSchemaNotAllowed", "PUBLIC can create")
			Consistently(func(g Gomega) { g.Expect(installed(g, "ltree")).To(BeEmpty()) }, 10*time.Second).Should(Succeed())

			_, err = psql(`REVOKE CREATE ON SCHEMA shared FROM PUBLIC`)
			Expect(err).NotTo(HaveOccurred())
			patch(dbRes, `{"metadata":{"annotations":{"pgop.ruck.io/nudge":"`+time.Now().Format(time.RFC3339Nano)+`"}}}`)
			waitReady()
			Eventually(func(g Gomega) { g.Expect(installed(g, "ltree")).To(HaveSuffix("@shared")) }).Should(Succeed())
		})

		It("does not drop removed extensions unless dropOnRemoval is set", func() {
			setExtensions(`[{"name":"citext"},{"name":"ltree","schema":"shared"}]`)
			waitReady()
			Eventually(func(g Gomega) {
				g.Expect(installed(g, "hstore")).To(BeEmpty(), "hstore had dropOnRemoval")
				g.Expect(installed(g, "pg_trgm")).NotTo(BeEmpty(), "pg_trgm must stay installed")
				g.Expect(installed(g, "dblink")).NotTo(BeEmpty(), "dblink must stay installed")
			}).Should(Succeed())
		})
	})
}
