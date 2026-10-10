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

// RegisterGrantTrackingTests adds specs for the shared grant-tracking engine
// (issue #24, PR 2): tracked and revoked schema grants, PUBLIC as a grantee,
// spec.publicPrivileges, the grantee policy and how two Databases granting
// the same privilege interact. They use example-cluster and reset its
// rolePolicy at the end.
func RegisterGrantTrackingTests() {
	Context("Grant tracking", Ordered, func() {
		SetDefaultEventuallyTimeout(2 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		const (
			clusterName = "example-cluster"
			clusterRes  = "cluster.pgop.ruck.io/" + clusterName
			dbRes       = "database.pgop.ruck.io/track-db"
			db2Res      = "database.pgop.ruck.io/track-db2"
			dupRes      = "database.pgop.ruck.io/track-dup"
			readerRes   = "role.pgop.ruck.io/track-reader"
		)

		apply := func(manifest string) {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		}
		patchErr := func(resource, body string) error {
			_, err := utils.Run(exec.Command("kubectl", "patch", resource, "-n", namespace, "--type=merge", "-p", body))
			return err
		}
		patch := func(resource, body string) { Expect(patchErr(resource, body)).To(Succeed()) }

		// psqlDB runs a query as the superuser over the local socket.
		psqlDB := func(db, query string) (string, error) {
			out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
				"sh", "-c", `psql -U "$POSTGRES_USER" -d "$1" -v ON_ERROR_STOP=1 -tAc "$0"`, query, db))
			return strings.TrimSpace(out), err
		}
		psql := func(query string) (string, error) { return psqlDB("postgres", query) }
		queryDB := func(g Gomega, db, q string) string {
			out, err := psqlDB(db, q)
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
		// publicPrivs lists PUBLIC's privileges on the database or on a schema
		// of track_db (the default ACL when the ACL is NULL).
		const publicOnDB = `SELECT coalesce(string_agg(a.privilege_type, ',' ORDER BY a.privilege_type), '') FROM pg_database d, ` +
			`aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) a WHERE d.datname = 'track_db' AND a.grantee = 0`
		publicOnSchema := func(schema string) string {
			return `SELECT coalesce(string_agg(a.privilege_type, ',' ORDER BY a.privilege_type), '') FROM pg_namespace n, ` +
				`aclexplode(coalesce(n.nspacl, acldefault('n', n.nspowner))) a WHERE n.nspname = '` + schema + `' AND a.grantee = 0`
		}
		dbACLHas := func(g Gomega, db, role string) bool {
			return queryDB(g, "postgres", `SELECT count(*) FROM pg_database d, aclexplode(d.datacl) a WHERE d.datname = '`+db+
				`' AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = '`+role+`')`) != "0"
		}

		AfterAll(func() {
			for _, res := range []string{dupRes, db2Res, dbRes, readerRes} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found",
					"--wait=true", "--timeout=2m", res))
			}
			for _, role := range []string{"track_dep", "track_manual", "track_outsider", "track_su"} {
				_, _ = psql(`DROP ROLE IF EXISTS ` + role)
			}
			_, _ = utils.Run(exec.Command("kubectl", "patch", clusterRes, "-n", namespace,
				"--type=json", "-p", `[{"op":"remove","path":"/spec/rolePolicy"}]`))
		})

		It("tracks schema grants and PUBLIC privileges, and revokes what is removed", func() {
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: track-reader
spec:
  clusterRef:
    name: example-cluster
  roleName: track_reader
  login: false
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: track-db
spec:
  clusterRef:
    name: example-cluster
  databaseName: track_db
  grants:
    - role: track_reader
      privileges: [CONNECT]
  publicPrivileges:
    connect: false
    temporary: false
    publicSchemaUsage: false
  schemas:
    - name: app
      grants:
        - role: track_reader
          privileges: [USAGE, CREATE]
          withGrantOption: true
        - role: PUBLIC
          privileges: [USAGE]
`)
			waitReady(readerRes)
			waitReady(dbRes)

			By("verifying the schema grants and the revoked PUBLIC defaults")
			Eventually(func(g Gomega) {
				g.Expect(queryDB(g, "track_db", `SELECT has_schema_privilege('track_reader', 'app', 'CREATE WITH GRANT OPTION')`)).To(Equal("t"))
				g.Expect(queryDB(g, "track_db", publicOnSchema("app"))).To(Equal("USAGE"))
				g.Expect(queryDB(g, "track_db", publicOnSchema("public"))).To(BeEmpty())
				g.Expect(queryDB(g, "postgres", publicOnDB)).To(BeEmpty())
				g.Expect(dbACLHas(g, "track_db", "track_reader")).To(BeTrue())
				g.Expect(jsonpath(g, dbRes, "{.status.revokedPublicPrivileges}")).To(
					SatisfyAll(ContainSubstring("connect"), ContainSubstring("temporary"), ContainSubstring("publicSchemaUsage")))
			}).Should(Succeed())

			By("letting the grantee pass CREATE on, and granting USAGE outside pgop")
			for _, q := range []string{`CREATE ROLE track_dep NOLOGIN`, `CREATE ROLE track_manual NOLOGIN`} {
				_, err := psql(q)
				Expect(err).NotTo(HaveOccurred())
			}
			_, err := psqlDB("track_db", `SET ROLE track_reader; GRANT CREATE ON SCHEMA app TO track_dep`)
			Expect(err).NotTo(HaveOccurred())
			_, err = psqlDB("track_db", `GRANT USAGE ON SCHEMA app TO track_manual`)
			Expect(err).NotTo(HaveOccurred())

			By("narrowing the schema grant, dropping PUBLIC's and restoring the PUBLIC defaults")
			patch(dbRes, `{"spec":{"publicPrivileges":null,"schemas":[{"name":"app","grants":[{"role":"track_reader","privileges":["USAGE"]}]}]}}`)
			Eventually(func(g Gomega) {
				g.Expect(queryDB(g, "track_db", `SELECT has_schema_privilege('track_reader', 'app', 'CREATE')`)).To(Equal("f"))
				g.Expect(queryDB(g, "track_db", `SELECT has_schema_privilege('track_reader', 'app', 'USAGE WITH GRANT OPTION')`)).To(Equal("f"))
				g.Expect(queryDB(g, "track_db", `SELECT has_schema_privilege('track_reader', 'app', 'USAGE')`)).To(Equal("t"))
				// The revoke cascaded to what track_reader had passed on.
				g.Expect(queryDB(g, "track_db", `SELECT has_schema_privilege('track_dep', 'app', 'CREATE')`)).To(Equal("f"))
				g.Expect(queryDB(g, "track_db", publicOnSchema("app"))).To(BeEmpty())
				// Exactly what pgop revoked is granted back to PUBLIC.
				g.Expect(queryDB(g, "track_db", publicOnSchema("public"))).To(Equal("USAGE"))
				g.Expect(queryDB(g, "postgres", publicOnDB)).To(Equal("CONNECT,TEMPORARY"))
				g.Expect(jsonpath(g, dbRes, "{.status.revokedPublicPrivileges}")).To(BeEmpty())
				// A grant made outside pgop is never revoked.
				g.Expect(queryDB(g, "track_db", `SELECT has_schema_privilege('track_manual', 'app', 'USAGE')`)).To(Equal("t"))
			}).Should(Succeed())

			By("removing the whole schema entry, which revokes its grants but keeps the schema")
			patch(dbRes, `{"spec":{"schemas":null}}`)
			Eventually(func(g Gomega) {
				g.Expect(queryDB(g, "track_db", `SELECT has_schema_privilege('track_reader', 'app', 'USAGE')`)).To(Equal("f"))
				g.Expect(queryDB(g, "track_db", `SELECT count(*) FROM pg_namespace WHERE nspname = 'app'`)).To(Equal("1"))
				g.Expect(queryDB(g, "track_db", `SELECT has_schema_privilege('track_manual', 'app', 'USAGE')`)).To(Equal("t"))
				g.Expect(jsonpath(g, dbRes, "{.status.managedSchemaGrants}")).To(BeEmpty())
			}).Should(Succeed())
			waitReady(dbRes)
		})

		It("never revokes privileges the grantee already held, and leaves unmanaged schemas alone", func() {
			By("declaring grants PostgreSQL's defaults and the schema owner already give")
			patch(dbRes, `{"spec":{"grants":[{"role":"track_reader","privileges":["CONNECT"]},{"role":"PUBLIC","privileges":["CONNECT"]}],`+
				`"schemas":[{"name":"public","grants":[{"role":"PUBLIC","privileges":["USAGE"]}]},`+
				`{"name":"owned","owner":"track_reader","grants":[{"role":"track_reader","privileges":["ALL"],"withGrantOption":true}]}]}}`)
			Eventually(func(g Gomega) {
				g.Expect(queryDB(g, "track_db", `SELECT count(*) FROM pg_namespace WHERE nspname = 'owned'`)).To(Equal("1"))
				g.Expect(jsonpath(g, dbRes, "{.status.ready}")).To(Equal("true"))
				// Nothing was added, so nothing is recorded.
				g.Expect(jsonpath(g, dbRes, "{.status.managedSchemaGrants}")).To(BeEmpty())
				g.Expect(jsonpath(g, dbRes, "{.status.managedGrants}")).NotTo(ContainSubstring("PUBLIC"))
			}).Should(Succeed())

			By("removing them keeps PUBLIC's defaults and the owner's privileges")
			patch(dbRes, `{"spec":{"grants":[{"role":"track_reader","privileges":["CONNECT"]}],"schemas":null}}`)
			Consistently(func(g Gomega) {
				g.Expect(queryDB(g, "postgres", publicOnDB)).To(Equal("CONNECT,TEMPORARY"))
				g.Expect(queryDB(g, "track_db", publicOnSchema("public"))).To(Equal("USAGE"))
				g.Expect(queryDB(g, "track_db", `SELECT has_schema_privilege('track_reader', 'owned', 'CREATE')`)).To(Equal("t"))
				g.Expect(queryDB(g, "track_db", `SELECT has_schema_privilege('track_reader', 'owned', 'USAGE')`)).To(Equal("t"))
			}, 20*time.Second, 2*time.Second).Should(Succeed())
			waitReady(dbRes)

			By("refusing to take over or grant on a schema the Database did not create and does not own")
			_, err := psqlDB("track_db", `CREATE SCHEMA foreign_s`)
			Expect(err).NotTo(HaveOccurred())
			patch(dbRes, `{"spec":{"schemas":[{"name":"foreign_s","owner":"track_reader","grants":[{"role":"track_reader","privileges":["USAGE"]}]}]}}`)
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, dbRes, availableReason)).To(Equal("SchemaNotManaged"))
			}).Should(Succeed())
			Consistently(func(g Gomega) {
				g.Expect(queryDB(g, "track_db", `SELECT pg_get_userbyid(nspowner) <> 'track_reader' FROM pg_namespace WHERE nspname = 'foreign_s'`)).To(Equal("t"))
				g.Expect(queryDB(g, "track_db", `SELECT has_schema_privilege('track_reader', 'foreign_s', 'USAGE')`)).To(Equal("f"))
			}, 10*time.Second, 2*time.Second).Should(Succeed())
			patch(dbRes, `{"spec":{"schemas":null}}`)
			waitReady(dbRes)
		})

		It("refuses grantees outside the policy and revokes them once they are no longer allowed", func() {
			for _, q := range []string{`CREATE ROLE track_outsider NOLOGIN`, `CREATE ROLE track_su NOLOGIN SUPERUSER`} {
				_, err := psql(q)
				Expect(err).NotTo(HaveOccurred())
			}

			By("rejecting reserved and mis-spelled grantees in the API server")
			for _, role := range []string{"postgres", "public", "pgop_operator", "pg_read_server_files"} {
				err := patchErr(dbRes, `{"spec":{"grants":[{"role":"`+role+`","privileges":["CONNECT"]}]}}`)
				Expect(err).To(HaveOccurred(), "grantee %s must be rejected", role)
			}
			Expect(patchErr(dbRes, `{"spec":{"grants":[{"role":"PUBLIC","privileges":["CONNECT"],"withGrantOption":true}]}}`)).
				NotTo(Succeed(), "the grant option must not be granted to PUBLIC")

			By("refusing a role no Role manages and a superuser")
			patch(dbRes, `{"spec":{"grants":[{"role":"track_reader","privileges":["CONNECT"]},`+
				`{"role":"track_outsider","privileges":["CONNECT"]},{"role":"track_su","privileges":["TEMP"]}]}}`)
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, dbRes, availableReason)).To(Equal("GranteeNotAllowed"))
				g.Expect(jsonpath(g, dbRes, availableMessage)).To(SatisfyAll(
					ContainSubstring("track_outsider is not managed by a Role of this Cluster"),
					ContainSubstring("track_su")))
				g.Expect(dbACLHas(g, "track_db", "track_reader")).To(BeTrue())
				g.Expect(dbACLHas(g, "track_db", "track_outsider")).To(BeFalse())
				g.Expect(dbACLHas(g, "track_db", "track_su")).To(BeFalse())
			}).Should(Succeed())

			By("granting to the outsider once the Cluster allowlists it (a superuser stays refused)")
			patch(clusterRes, `{"spec":{"rolePolicy":{"allowedExistingRoles":["track_outsider","track_su"]}}}`)
			Eventually(func(g Gomega) {
				g.Expect(dbACLHas(g, "track_db", "track_outsider")).To(BeTrue())
				g.Expect(dbACLHas(g, "track_db", "track_su")).To(BeFalse())
				g.Expect(jsonpath(g, dbRes, availableMessage)).To(SatisfyAll(
					ContainSubstring("track_su is a superuser"), Not(ContainSubstring("track_outsider"))))
			}).Should(Succeed())

			By("revoking the managed grant once the Cluster stops allowing the outsider")
			_, err := utils.Run(exec.Command("kubectl", "patch", clusterRes, "-n", namespace,
				"--type=json", "-p", `[{"op":"remove","path":"/spec/rolePolicy"}]`))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(dbACLHas(g, "track_db", "track_outsider")).To(BeFalse())
				g.Expect(dbACLHas(g, "track_db", "track_reader")).To(BeTrue())
			}).Should(Succeed())

			patch(dbRes, `{"spec":{"grants":[{"role":"track_reader","privileges":["CONNECT"]}]}}`)
			waitReady(dbRes)
		})

		It("keeps two Databases granting the same privilege independent", func() {
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: track-db2
spec:
  clusterRef:
    name: example-cluster
  databaseName: track_db2
  grants:
    - role: track_reader
      privileges: [CONNECT]
`)
			waitReady(db2Res)

			By("refusing a second Database for the same PostgreSQL database")
			apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: track-dup
spec:
  clusterRef:
    name: example-cluster
  databaseName: track_db
  grants:
    - role: track_reader
      privileges: [CREATE]
`)
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, dupRes, availableReason)).To(Equal("DuplicateDatabaseName"))
			}).Should(Succeed())
			Consistently(func(g Gomega) {
				g.Expect(queryDB(g, "postgres", `SELECT has_database_privilege('track_reader', 'track_db', 'CREATE')`)).To(Equal("f"))
			}, 10*time.Second, 2*time.Second).Should(Succeed())

			By("removing the grant from one Database only")
			patch(db2Res, `{"spec":{"grants":null}}`)
			Eventually(func(g Gomega) {
				g.Expect(dbACLHas(g, "track_db2", "track_reader")).To(BeFalse())
				g.Expect(dbACLHas(g, "track_db", "track_reader")).To(BeTrue())
			}).Should(Succeed())

			By("deleting the duplicate, which neither drops nor revokes anything")
			_, err := utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m", dupRes))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(queryDB(g, "postgres", `SELECT count(*) FROM pg_database WHERE datname = 'track_db'`)).To(Equal("1"))
				g.Expect(dbACLHas(g, "track_db", "track_reader")).To(BeTrue())
			}).Should(Succeed())
		})
	})
}
