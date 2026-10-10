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
				"sh", "-c", `psql -U "$POSTGRES_USER" -d "$1" -v ON_ERROR_STOP=1 -tAc "$0"`, query, db))
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
			for _, res := range []string{dbRes + "pol-db", roleRes + "pol-steal", roleRes + "pol-old", roleRes + "pol-priv",
				roleRes + "pol-mem", roleRes + "pol-adopt"} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found",
					"--wait=true", "--timeout=2m", res))
			}
			_, _ = psql(`DROP ROLE IF EXISTS pol_dba`)
			_, _ = psql(`DROP ROLE IF EXISTS pol_adopt`)
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

		It("does not take over an existing role that is a member of a superuser", func() {
			_, err := psql(`CREATE ROLE pol_adopt NOLOGIN IN ROLE pol_dba`)
			Expect(err).NotTo(HaveOccurred())
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-adopt
spec:
  clusterRef:
    name: example-cluster
  roleName: pol_adopt
`)).To(Succeed())
			expectReason(roleRes+"pol-adopt", "RolePolicyViolation", "does not take over", "pol_dba")
			Eventually(func(g Gomega) {
				// Not altered: still NOLOGIN, no password.
				g.Expect(query(g, `SELECT rolcanlogin::text || ',' || (rolpassword IS NULL) FROM pg_authid WHERE rolname = 'pol_adopt'`)).
					To(Equal("false,true"))
			}).Should(Succeed())

			By("deleting the Role leaves the role it did not take over in place")
			_, err = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m", roleRes+"pol-adopt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(psql(`SELECT count(*) FROM pg_roles WHERE rolname = 'pol_adopt'`)).To(Equal("1"))
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

			By("allowing file_fdw on the Cluster")
			setPolicy(`{"allowedExtensions":["file_fdw"]}`)
			waitReady(dbRes + "pol-db")
			Eventually(func(g Gomega) {
				g.Expect(extensions(g)).To(Equal("file_fdw@public,pg_trgm@public"))
			}).Should(Succeed())
		})
	})
}
