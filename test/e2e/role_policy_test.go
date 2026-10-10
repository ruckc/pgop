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

// RegisterRolePolicyTests adds specs for the role privilege policy (issue
// #24, PR 1): no superuser Roles, privileged attributes, predefined-role
// memberships and untrusted extensions only through the Cluster's
// spec.rolePolicy. They use example-cluster and reset its rolePolicy at the
// end.
func RegisterRolePolicyTests() {
	Context("Role privilege policy", Ordered, func() {
		SetDefaultEventuallyTimeout(2 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		const (
			clusterName = "example-cluster"
			roleRes     = "role.pgop.ruck.io/"
			dbRes       = "database.pgop.ruck.io/"
			clusterRes  = "cluster.pgop.ruck.io/" + clusterName
		)

		apply := func(manifest string) error {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			return err
		}

		setPolicy := func(policy string) {
			_, err := utils.Run(exec.Command("kubectl", "patch", clusterRes, "-n", namespace,
				"--type=merge", "-p", `{"spec":{"rolePolicy":`+policy+`}}`))
			Expect(err).NotTo(HaveOccurred())
		}

		// psqlDB runs a query as the superuser over the local socket.
		psqlDB := func(db, query string) (string, error) {
			out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
				"sh", "-c", `PGOPTIONS='-c statement_timeout=0 -c lock_timeout=0 -c default_transaction_read_only=off -c search_path=public' `+
					`psql -U "$POSTGRES_USER" -d "$1" -v ON_ERROR_STOP=1 -tAc "$0"`, query, db))
			return strings.TrimSpace(out), err
		}
		psql := func(query string) (string, error) { return psqlDB("postgres", query) }

		query := func(g Gomega, q string) string {
			out, err := psql(q)
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}

		jsonpath := func(g Gomega, res, path string) string {
			out, err := utils.Run(exec.Command("kubectl", "get", res, "-n", namespace, "-o", "jsonpath="+path))
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}
		const availableReason = `{.status.conditions[?(@.type=="Available")].reason}`
		const availableMessage = `{.status.conditions[?(@.type=="Available")].message}`

		expectReason := func(res, reason string, messageParts ...string) {
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, res, availableReason)).To(Equal(reason))
				msg := jsonpath(g, res, availableMessage)
				for _, part := range messageParts {
					g.Expect(msg).To(ContainSubstring(part))
				}
			}).Should(Succeed())
		}

		waitReady := func(res string) {
			Eventually(func(g Gomega) {
				g.Expect(jsonpath(g, res, "{.status.ready}")).To(Equal("true"), res+" not ready")
			}).Should(Succeed())
		}

		AfterAll(func() {
			for _, res := range []string{dbRes + "pol-db", dbRes + "pol-db-dup", dbRes + "pol-dbaowned", roleRes + "pol-steal",
				roleRes + "pol-mem2", roleRes + "pol-old", roleRes + "pol-priv", roleRes + "pol-mem", roleRes + "pol-adopt",
				roleRes + "pol-adopt-su", roleRes + "pol-dup"} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found",
					"--wait=true", "--timeout=2m", res))
			}
			_, _ = psql(`DROP DATABASE IF EXISTS pol_dbaowned`)
			for _, role := range []string{"pol_dba", "pol_adopt_su", "pol_dba_app", "pol_analytics"} {
				_, _ = psql(`DROP ROLE IF EXISTS ` + role)
			}
			_, _ = utils.Run(exec.Command("kubectl", "patch", clusterRes, "-n", namespace,
				"--type=json", "-p", `[{"op":"remove","path":"/spec/rolePolicy"}]`))
		})

		It("has no superuser field and demotes existing superuser roles", func() {
			By("rejecting spec.superuser")
			err := apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-su
spec:
  clusterRef:
    name: example-cluster
  superuser: true
`)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("superuser"))

			By("refusing the Cluster's superuser credentials as passwordSecretRef")
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-steal
spec:
  clusterRef:
    name: example-cluster
  roleName: pol_steal
  passwordSecretRef:
    name: example-cluster-credentials
    key: password
`)).To(Succeed())
			expectReason(roleRes+"pol-steal", "RolePolicyViolation", "managed by pgop")
			_, err = utils.Run(exec.Command("kubectl", "get", "secret", "example-cluster-pol-steal-credentials", "-n", namespace))
			Expect(err).To(HaveOccurred(), "no credentials Secret may be created")

			By("demoting a role made superuser by an older pgop")
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-old
spec:
  clusterRef:
    name: example-cluster
  roleName: pol_old
  login: false
`)).To(Succeed())
			waitReady(roleRes + "pol-old")
			_, err = psql(`ALTER ROLE pol_old SUPERUSER`)
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "patch", roleRes+"pol-old", "-n", namespace,
				"--type=merge", "-p", `{"spec":{"connectionLimit":5}}`))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(query(g, `SELECT rolsuper::text || ',' || rolconnlimit FROM pg_roles WHERE rolname = 'pol_old'`)).
					To(Equal("false,5"))
			}).Should(Succeed())
		})

		It("refuses a privileged attribute until the Cluster allows it", func() {
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-priv
spec:
  clusterRef:
    name: example-cluster
  roleName: pol_priv
  login: false
  createDB: true
  bypassRLS: true
`)).To(Succeed())
			expectReason(roleRes+"pol-priv", "RolePolicyViolation", "bypassRLS")
			Eventually(func(g Gomega) {
				g.Expect(query(g, `SELECT rolbypassrls::text || ',' || rolcreatedb FROM pg_roles WHERE rolname = 'pol_priv'`)).
					To(Equal("false,true"))
			}).Should(Succeed())

			By("allowing bypassRLS on the Cluster")
			setPolicy(`{"allowedAttributes":["bypassRLS"]}`)
			waitReady(roleRes + "pol-priv")
			Eventually(func(g Gomega) {
				g.Expect(query(g, `SELECT rolbypassrls FROM pg_roles WHERE rolname = 'pol_priv'`)).To(Equal("t"))
			}).Should(Succeed())

			By("withdrawing the permission: the role is altered down")
			setPolicy(`{"allowedAttributes":[]}`)
			expectReason(roleRes+"pol-priv", "RolePolicyViolation", "bypassRLS")
			Eventually(func(g Gomega) {
				g.Expect(query(g, `SELECT rolbypassrls FROM pg_roles WHERE rolname = 'pol_priv'`)).To(Equal("f"))
			}).Should(Succeed())
		})

		It("enforces the membership policy", func() {
			membership := func(g Gomega, role string) string {
				return query(g, `SELECT count(*) FROM pg_auth_members WHERE roleid = '`+role+`'::regrole AND member = 'pol_mem'::regrole`)
			}

			By("rejecting server-file roles and reserved roles at admission")
			for _, target := range []string{"pg_execute_server_program", "postgres", "pgop_operator"} {
				err := apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-bad
spec:
  clusterRef:
    name: example-cluster
  memberships:
    - role: ` + target + `
`)
				Expect(err).To(HaveOccurred(), target)
				Expect(err.Error()).To(ContainSubstring("is not allowed"))
			}

			By("refusing a superuser role and a predefined role the Cluster does not allow")
			_, err := psql(`CREATE ROLE pol_dba SUPERUSER NOLOGIN`)
			Expect(err).NotTo(HaveOccurred())
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-mem
spec:
  clusterRef:
    name: example-cluster
  roleName: pol_mem
  login: false
  memberships:
    - role: pg_monitor
    - role: pol_dba
`)).To(Succeed())
			expectReason(roleRes+"pol-mem", "MembershipNotAllowed", "pol_dba is a superuser", "pg_monitor is a predefined role")
			Eventually(func(g Gomega) {
				g.Expect(membership(g, "pol_dba")).To(Equal("0"))
				g.Expect(membership(g, "pg_monitor")).To(Equal("0"))
			}).Should(Succeed())

			By("allowing pg_monitor on the Cluster and dropping the superuser membership")
			setPolicy(`{"allowedPredefinedRoles":["pg_monitor"]}`)
			_, err = utils.Run(exec.Command("kubectl", "patch", roleRes+"pol-mem", "-n", namespace,
				"--type=merge", "-p", `{"spec":{"memberships":[{"role":"pg_monitor"}]}}`))
			Expect(err).NotTo(HaveOccurred())
			waitReady(roleRes + "pol-mem")
			Eventually(func(g Gomega) {
				g.Expect(membership(g, "pg_monitor")).To(Equal("1"))
				g.Expect(jsonpath(g, roleRes+"pol-mem", "{.status.managedMemberships}")).To(Equal(`["pg_monitor"]`))
			}).Should(Succeed())

			By("withdrawing pg_monitor: the tracked membership is revoked")
			setPolicy(`{"allowedPredefinedRoles":[]}`)
			// The message names the revoked membership only on the reconcile that
			// revokes it; afterwards it still reports the refused membership.
			expectReason(roleRes+"pol-mem", "MembershipNotAllowed", "pg_monitor is a predefined role")
			Eventually(func(g Gomega) {
				g.Expect(membership(g, "pg_monitor")).To(Equal("0"))
				g.Expect(jsonpath(g, roleRes+"pol-mem", "{.status.managedMemberships}")).To(BeEmpty())
			}).Should(Succeed())
		})

		It("refuses memberships in roles no Role of the Cluster manages unless the Cluster lists them", func() {
			_, err := psql(`CREATE ROLE pol_analytics NOLOGIN`)
			Expect(err).NotTo(HaveOccurred())
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-mem2
spec:
  clusterRef:
    name: example-cluster
  roleName: pol_mem2
  login: false
  memberships:
    - role: pol_analytics
    - role: pol_old
`)).To(Succeed())
			expectReason(roleRes+"pol-mem2", "MembershipNotAllowed", "pol_analytics is not managed by a Role of this Cluster")
			member := func(g Gomega, role string) string {
				return query(g, `SELECT count(*) FROM pg_auth_members WHERE roleid = '`+role+`'::regrole AND member = 'pol_mem2'::regrole`)
			}
			Eventually(func(g Gomega) {
				g.Expect(member(g, "pol_old")).To(Equal("1"), "a role managed by a Role of the Cluster is allowed")
				g.Expect(member(g, "pol_analytics")).To(Equal("0"))
			}).Should(Succeed())

			By("allowing the DBA's role on the Cluster")
			setPolicy(`{"allowedExistingRoles":["pol_analytics"]}`)
			waitReady(roleRes + "pol-mem2")
			Eventually(func(g Gomega) { g.Expect(member(g, "pol_analytics")).To(Equal("1")) }).Should(Succeed())
		})

		It("only manages PostgreSQL roles and databases it created", func() {
			passwordOf := func(g Gomega, role string) string {
				return query(g, `SELECT coalesce(rolpassword, '') FROM pg_authid WHERE rolname = '`+role+`'`)
			}
			roleComment := func(g Gomega, role string) string {
				return query(g, `SELECT coalesce(shobj_description(oid, 'pg_authid'), '') FROM pg_roles WHERE rolname = '`+role+`'`)
			}

			By("leaving a DBA's existing role alone")
			_, err := psql(`CREATE ROLE pol_dba_app LOGIN PASSWORD 'dba-secret'`)
			Expect(err).NotTo(HaveOccurred())
			var dbaPassword string
			Eventually(func(g Gomega) { dbaPassword = passwordOf(g, "pol_dba_app") }).Should(Succeed())
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-adopt
spec:
  clusterRef:
    name: example-cluster
  roleName: pol_dba_app
`)).To(Succeed())
			expectReason(roleRes+"pol-adopt", "RoleNotManaged", "COMMENT ON ROLE pol_dba_app IS 'pgop:v1:Role/pol-adopt'")
			Consistently(func(g Gomega) { g.Expect(passwordOf(g, "pol_dba_app")).To(Equal(dbaPassword)) }, 20*time.Second, 2*time.Second).
				Should(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "get", "secret", "example-cluster-pol-adopt-credentials", "-n", namespace))
			Expect(err).To(HaveOccurred(), "the DBA's password must not be replaced and handed out")

			By("refusing a superuser-member role even when it is handed over")
			_, err = psql(`CREATE ROLE pol_adopt_su NOLOGIN IN ROLE pol_dba`)
			Expect(err).NotTo(HaveOccurred())
			_, err = psql(`COMMENT ON ROLE pol_adopt_su IS 'pgop:v1:Role/pol-adopt-su'`)
			Expect(err).NotTo(HaveOccurred())
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-adopt-su
spec:
  clusterRef:
    name: example-cluster
  roleName: pol_adopt_su
  login: false
`)).To(Succeed())
			expectReason(roleRes+"pol-adopt-su", "RolePolicyViolation", "does not take over", "pol_dba")

			By("taking a role over once a superuser hands it over")
			_, err = psql(`COMMENT ON ROLE pol_dba_app IS 'pgop:v1:Role/pol-adopt'`)
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "annotate", roleRes+"pol-adopt", "-n", namespace, "--overwrite", "pgop.ruck.io/nudge=1"))
			Expect(err).NotTo(HaveOccurred())
			waitReady(roleRes + "pol-adopt")
			Eventually(func(g Gomega) { g.Expect(passwordOf(g, "pol_dba_app")).NotTo(Equal(dbaPassword)) }).Should(Succeed())

			By("refusing a second Role with the same PostgreSQL name")
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-dup
spec:
  clusterRef:
    name: example-cluster
  roleName: pol_dba_app
`)).To(Succeed())
			expectReason(roleRes+"pol-dup", "DuplicateRoleName", "pol-adopt")
			_, err = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m", roleRes+"pol-dup"))
			Expect(err).NotTo(HaveOccurred())
			Expect(psql(`SELECT count(*) FROM pg_roles WHERE rolname = 'pol_dba_app'`)).To(Equal("1"), "deleting the duplicate must not drop the role")

			By("dropping the handed-over role with its Role, but not roles it never owned")
			for _, res := range []string{roleRes + "pol-adopt", roleRes + "pol-adopt-su"} {
				_, err = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m", res))
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(psql(`SELECT count(*) FROM pg_roles WHERE rolname IN ('pol_dba_app', 'pol_adopt_su')`)).To(Equal("1"))
			Expect(psql(`SELECT count(*) FROM pg_roles WHERE rolname = 'pol_adopt_su'`)).To(Equal("1"))

			By("marking a role an earlier pgop created without a marker (legacy)")
			_, err = psql(`COMMENT ON ROLE pol_old IS NULL`)
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "patch", roleRes+"pol-old", "-n", namespace,
				"--type=merge", "-p", `{"spec":{"connectionLimit":6}}`))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(roleComment(g, "pol_old")).To(Equal("pgop:v1:Role/pol-old"))
				g.Expect(query(g, `SELECT rolconnlimit FROM pg_roles WHERE rolname = 'pol_old'`)).To(Equal("6"))
			}).Should(Succeed())

			By("leaving a DBA's existing database alone")
			_, err = psql(`CREATE DATABASE pol_dbaowned`)
			Expect(err).NotTo(HaveOccurred())
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: pol-dbaowned
spec:
  clusterRef:
    name: example-cluster
  databaseName: pol_dbaowned
  owner: pol-old
`)).To(Succeed())
			expectReason(dbRes+"pol-dbaowned", "DatabaseNotManaged", "COMMENT ON DATABASE pol_dbaowned IS 'pgop:v1:Database/pol-dbaowned'")
			Consistently(func(g Gomega) {
				g.Expect(query(g, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'pol_dbaowned'`)).NotTo(Equal("pol_old"))
			}, 15*time.Second, 3*time.Second).Should(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m", dbRes+"pol-dbaowned"))
			Expect(err).NotTo(HaveOccurred())
			Expect(psql(`SELECT count(*) FROM pg_database WHERE datname = 'pol_dbaowned'`)).To(Equal("1"), "a database pgop does not own is never dropped")
		})

		It("installs trusted extensions and untrusted ones only when the Cluster allows them", func() {
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: pol-db
spec:
  clusterRef:
    name: example-cluster
  databaseName: pol_db
  extensions:
    - name: pg_trgm
    - name: file_fdw
`)).To(Succeed())
			expectReason(dbRes+"pol-db", "ExtensionNotAllowed", "file_fdw")
			extensions := func(g Gomega) string {
				out, err := psqlDB("pol_db", `SELECT string_agg(e.extname || '@' || n.nspname, ',' ORDER BY e.extname) FROM pg_extension e `+
					`JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname IN ('pg_trgm', 'file_fdw')`)
				g.Expect(err).NotTo(HaveOccurred())
				return out
			}
			Eventually(func(g Gomega) {
				// The trusted extension is installed into public, not pg_catalog,
				// although operator sessions pin search_path to pg_catalog.
				g.Expect(extensions(g)).To(Equal("pg_trgm@public"))
			}).Should(Succeed())

			By("refusing a second Database with the same PostgreSQL name")
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: pol-db-dup
spec:
  clusterRef:
    name: example-cluster
  databaseName: pol_db
`)).To(Succeed())
			expectReason(dbRes+"pol-db-dup", "DuplicateDatabaseName", "pol-db")
			_, err := utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m", dbRes+"pol-db-dup"))
			Expect(err).NotTo(HaveOccurred())
			Expect(psql(`SELECT count(*) FROM pg_database WHERE datname = 'pol_db'`)).To(Equal("1"), "deleting the duplicate must not drop the database")

			By("hostile database defaults do not affect the operator's sessions")
			for _, set := range []string{"default_transaction_read_only = on", "statement_timeout = 1",
				"lock_timeout = 1", "idle_in_transaction_session_timeout = 1", "search_path = evil, public", "check_function_bodies = off"} {
				_, err = psql(`ALTER DATABASE pol_db SET ` + set)
				Expect(err).NotTo(HaveOccurred())
			}

			By("allowing file_fdw on the Cluster")
			setPolicy(`{"allowedExtensions":["file_fdw"]}`)
			waitReady(dbRes + "pol-db")
			Eventually(func(g Gomega) {
				g.Expect(extensions(g)).To(Equal("file_fdw@public,pg_trgm@public"))
			}).Should(Succeed())
		})
	})
}
