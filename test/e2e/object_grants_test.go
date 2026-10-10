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

// RegisterObjectGrantTests adds specs for Database.spec.schemas[].objectGrants
// and defaultPrivileges (issue #24, PR 5): grants on tables, sequences and
// functions, "*" picking up a new table, revoke on removal and when an
// object stops matching, default privileges for a migrator role, objects
// pgop refuses (another superuser's, the operator's SECURITY DEFINER
// function, an extension's), the grantee policy and role deletion. They use
// example-cluster.
func RegisterObjectGrantTests() {
	Context("Object grants", Ordered, func() {
		SetDefaultEventuallyTimeout(2 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		const (
			clusterName = "example-cluster"
			dbRes       = "database.pgop.ruck.io/og-db"
			appRes      = "role.pgop.ruck.io/og-app"
		)

		apply := func(manifest string) {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		}
		patch := func(body string) {
			_, err := utils.Run(exec.Command("kubectl", "patch", dbRes, "-n", namespace, "--type=merge", "-p", body))
			Expect(err).NotTo(HaveOccurred())
		}
		nudge := func() {
			patch(`{"metadata":{"annotations":{"pgop.ruck.io/nudge":"` + time.Now().Format(time.RFC3339Nano) + `"}}}`)
		}
		// setSchema replaces spec.schemas with the schema app and the given
		// objectGrants and defaultPrivileges (JSON).
		setSchema := func(objectGrants, defaultPrivileges string) {
			patch(`{"spec":{"schemas":[{"name":"app","owner":"og_owner",` +
				`"grants":[{"role":"og_migrator","privileges":["USAGE","CREATE"]},{"role":"og_app","privileges":["USAGE"]}],` +
				`"objectGrants":` + objectGrants + `,"defaultPrivileges":` + defaultPrivileges + `}]}}`)
		}
		psqlDB := func(db, query string) (string, error) {
			out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
				"sh", "-c", `psql -U "$POSTGRES_USER" -d "$1" -v ON_ERROR_STOP=1 -tAc "$0"`, query, db))
			return strings.TrimSpace(out), err
		}
		mustSQL := func(query string) {
			_, err := psqlDB("og_db", query)
			Expect(err).NotTo(HaveOccurred())
		}
		query := func(g Gomega, q string) string {
			out, err := psqlDB("og_db", q)
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}
		has := func(g Gomega, fn, object, privilege string) bool {
			return query(g, `SELECT `+fn+`('og_app', '`+object+`', '`+privilege+`')`) == "t"
		}
		jsonpath := func(g Gomega, res, path string) string {
			out, err := utils.Run(exec.Command("kubectl", "get", res, "-n", namespace, "-o", "jsonpath="+path))
			g.Expect(err).NotTo(HaveOccurred())
			return strings.TrimSpace(out)
		}
		const reason = `{.status.conditions[?(@.type=="Available")].reason}`
		waitReady := func() {
			Eventually(func(g Gomega) { g.Expect(jsonpath(g, dbRes, "{.status.ready}")).To(Equal("true")) }).Should(Succeed())
		}
		// explicitGrants counts og_app's ACL entries on the routines of app.
		explicitGrants := func(g Gomega) string {
			return query(g, `SELECT count(*) FROM pg_proc p, aclexplode(p.proacl) a `+
				`WHERE p.pronamespace = 'app'::regnamespace AND a.grantee = 'og_app'::regrole`)
		}

		AfterAll(func() {
			for _, res := range []string{dbRes, appRes, "role.pgop.ruck.io/og-migrator", "role.pgop.ruck.io/og-owner"} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found",
					"--wait=true", "--timeout=2m", res))
			}
			for _, role := range []string{"og_outsider", "og_dba"} {
				_, _ = psqlDB("postgres", `DROP ROLE IF EXISTS `+role)
			}
		})

		It("grants on existing objects of trusted owners and skips the others", func() {
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: og-owner
spec:
  clusterRef:
    name: example-cluster
  roleName: og_owner
  login: false
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: og-app
spec:
  clusterRef:
    name: example-cluster
  roleName: og_app
  login: false
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: og-migrator
spec:
  clusterRef:
    name: example-cluster
  roleName: og_migrator
  login: false
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: og-db
spec:
  clusterRef:
    name: example-cluster
  databaseName: og_db
  owner: og-owner
  schemas:
    - name: app
      owner: og_owner
      grants:
        - role: og_migrator
          privileges: [USAGE, CREATE]
        - role: og_app
          privileges: [USAGE]
`)
			waitReady()

			By("creating objects: og_owner's, another superuser's, the operator's and an extension's")
			_, _ = psqlDB("postgres", `CREATE ROLE og_dba SUPERUSER NOLOGIN`)
			_, _ = psqlDB("postgres", `CREATE ROLE og_outsider NOLOGIN`)
			mustSQL(`SET ROLE og_owner; CREATE TABLE app.t1 (id int); CREATE SEQUENCE app.s1; ` +
				`CREATE FUNCTION app.f(integer) RETURNS integer LANGUAGE sql AS 'SELECT 1'; ` +
				`CREATE FUNCTION app.f(text) RETURNS integer LANGUAGE sql AS 'SELECT 2'`)
			mustSQL(`SET ROLE og_dba; CREATE TABLE app.su_t (id int)`)
			mustSQL(`CREATE FUNCTION app.op_sd() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'; ` +
				`CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA app`)

			setSchema(`[{"role":"og_app","kind":"table","objects":["*"],"privileges":["SELECT"]},`+
				`{"role":"og_app","kind":"sequence","objects":["*"],"privileges":["USAGE"]},`+
				`{"role":"og_app","kind":"function","objects":["f(integer)"],"privileges":["EXECUTE"]},`+
				`{"role":"og_app","kind":"function","objects":["*"],"privileges":["EXECUTE"]}]`,
				`[{"forRole":"og_migrator","role":"og_app","kind":"table","privileges":["SELECT"]}]`)
			waitReady()
			Eventually(func(g Gomega) {
				g.Expect(has(g, "has_table_privilege", "app.t1", "SELECT")).To(BeTrue())
				g.Expect(has(g, "has_table_privilege", "app.su_t", "SELECT")).To(BeFalse(), "another superuser's table")
				g.Expect(has(g, "has_sequence_privilege", "app.s1", "USAGE")).To(BeTrue())
				// f(integer) and f(text); not op_sd() nor pg_trgm's functions.
				g.Expect(explicitGrants(g)).To(Equal("2"))
				g.Expect(jsonpath(g, dbRes, `{.status.conditions[?(@.type=="ObjectGrantsComplete")].reason}`)).
					To(Equal("ObjectGrantSkipped"))
				g.Expect(jsonpath(g, dbRes, `{.status.objectGrants[?(@.kind=="function")].skippedExamples}`)).
					To(And(ContainSubstring("SECURITY DEFINER"), ContainSubstring("extension pg_trgm")))
				g.Expect(jsonpath(g, dbRes, `{.status.objectGrants[?(@.kind=="table")].skippedExamples}`)).
					To(ContainSubstring("app.su_t is owned by the superuser og_dba"))
			}).Should(Succeed())
		})

		It(`picks up a new table with "*" and applies default privileges to the migrator's tables`, func() {
			mustSQL(`SET ROLE og_owner; CREATE TABLE app.t2 (id int)`)
			nudge()
			Eventually(func(g Gomega) { g.Expect(has(g, "has_table_privilege", "app.t2", "SELECT")).To(BeTrue()) }).Should(Succeed())

			mustSQL(`SET ROLE og_migrator; CREATE TABLE app.m1 (id int)`)
			Eventually(func(g Gomega) { g.Expect(has(g, "has_table_privilege", "app.m1", "SELECT")).To(BeTrue()) }).Should(Succeed())
		})

		It("revokes what stops matching or is removed", func() {
			setSchema(`[{"role":"og_app","kind":"table","objects":["t1"],"privileges":["SELECT"]}]`, `[]`)
			waitReady()
			Eventually(func(g Gomega) {
				g.Expect(has(g, "has_table_privilege", "app.t1", "SELECT")).To(BeTrue())
				g.Expect(has(g, "has_table_privilege", "app.t2", "SELECT")).To(BeFalse())
				g.Expect(has(g, "has_sequence_privilege", "app.s1", "USAGE")).To(BeFalse())
				g.Expect(explicitGrants(g)).To(Equal("0"))
				g.Expect(query(g, `SELECT count(*) FROM pg_default_acl`)).To(Equal("0"))
			}).Should(Succeed())
			mustSQL(`SET ROLE og_migrator; CREATE TABLE app.m2 (id int)`)
			Consistently(func(g Gomega) { g.Expect(has(g, "has_table_privilege", "app.m2", "SELECT")).To(BeFalse()) },
				5*time.Second).Should(Succeed())
		})

		It("refuses grantees outside the grantee policy and named objects of untrusted owners", func() {
			setSchema(`[{"role":"og_outsider","kind":"table","objects":["t1"],"privileges":["SELECT"]}]`, `[]`)
			Eventually(func(g Gomega) { g.Expect(jsonpath(g, dbRes, reason)).To(Equal("GranteeNotAllowed")) }).Should(Succeed())
			Expect(psqlDB("og_db", `SELECT has_table_privilege('og_outsider', 'app.t1', 'SELECT')`)).To(Equal("f"))

			setSchema(`[{"role":"og_app","kind":"table","objects":["su_t"],"privileges":["SELECT"]}]`, `[]`)
			Eventually(func(g Gomega) { g.Expect(jsonpath(g, dbRes, reason)).To(Equal("ObjectGrantSkipped")) }).Should(Succeed())

			By("rejecting a superuser-only role name and a reserved forRole in the API server")
			_, err := utils.Run(exec.Command("kubectl", "patch", dbRes, "-n", namespace, "--type=merge", "-p",
				`{"spec":{"schemas":[{"name":"app","defaultPrivileges":[{"forRole":"postgres","role":"og_app","kind":"table",`+
					`"privileges":["SELECT"]}]}]}}`))
			Expect(err).To(HaveOccurred())
		})

		It("lets a Role holding object grants and default privileges be deleted", func() {
			setSchema(`[{"role":"og_app","kind":"table","objects":["*"],"privileges":["SELECT"],"withGrantOption":true},`+
				`{"role":"og_app","kind":"function","objects":["f"],"privileges":["EXECUTE"]}]`,
				`[{"forRole":"og_migrator","role":"og_app","kind":"table","privileges":["SELECT"]}]`)
			waitReady()
			mustSQL(`SET ROLE og_migrator; CREATE TABLE app.m3 (id int)`)
			Eventually(func(g Gomega) {
				g.Expect(has(g, "has_table_privilege", "app.m3", "SELECT")).To(BeTrue())
				g.Expect(explicitGrants(g)).To(Equal("2"))
			}).Should(Succeed())

			_, err := utils.Run(exec.Command("kubectl", "delete", appRes, "-n", namespace, "--wait=true", "--timeout=3m"))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(query(g, `SELECT count(*) FROM pg_roles WHERE rolname = 'og_app'`)).To(Equal("0"))
				g.Expect(query(g, `SELECT count(*) FROM pg_default_acl`)).To(Equal("0"))
			}).Should(Succeed())
		})
	})
}
