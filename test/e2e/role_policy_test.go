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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
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
			for _, res := range []string{dbRes + "pol-db", dbRes + "pol-db-dup", dbRes + "pol-dbaowned", dbRes + "pol-squat",
				roleRes + "pol-svc", roleRes + "pol-uid", dbRes + "pol-uid-db", roleRes + "pol-steal",
				roleRes + "pol-mem2", roleRes + "pol-old", roleRes + "pol-priv", roleRes + "pol-mem", roleRes + "pol-adopt",
				roleRes + "pol-adopt-su", roleRes + "pol-dup"} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--ignore-not-found",
					"--wait=true", "--timeout=2m", res))
			}
			_, _ = psql(`DROP DATABASE IF EXISTS pol_dbaowned`)
			_, _ = psql(`DROP DATABASE IF EXISTS pol_squat`)
			_, _ = psql(`DROP DATABASE IF EXISTS pol_uid_db`)
			for _, role := range []string{"pol_dba", "pol_adopt_su", "pol_dba_app", "pol_analytics", "pol_svc", "pol_w", "pol_squatter", "pol_uid"} {
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

		// validMarker computes the Cluster's valid ownership marker for
		// kind/name from its key Secret: what someone who copied a marker
		// (or guessed the key) could put in a comment.
		validMarker := func(kind, name string) string {
			out, err := utils.Run(exec.Command("kubectl", "get", "secret", clusterName+"-marker-key", "-n", namespace,
				"-o", "jsonpath={.data.key}"))
			Expect(err).NotTo(HaveOccurred())
			key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
			Expect(err).NotTo(HaveOccurred())
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(kind + "|" + namespace + "|" + clusterName + "|" + name))
			keyID := sha256.Sum256(key)
			return "pgop:v2:" + kind + "/" + name + ":" + base64.RawURLEncoding.EncodeToString(keyID[:6]) + "." +
				base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		}
		nudge := func(res string) {
			_, err := utils.Run(exec.Command("kubectl", "annotate", res, "-n", namespace, "--overwrite",
				"pgop.ruck.io/nudge="+time.Now().Format("150405.000")))
			Expect(err).NotTo(HaveOccurred())
		}
		passwordOf := func(g Gomega, role string) string {
			return query(g, `SELECT coalesce(rolpassword, '') FROM pg_authid WHERE rolname = '`+role+`'`)
		}
		roleComment := func(g Gomega, role string) string {
			return query(g, `SELECT coalesce(shobj_description(oid, 'pg_authid'), '') FROM pg_roles WHERE rolname = '`+role+`'`)
		}
		noSecret := func(name, why string) {
			_, err := utils.Run(exec.Command("kubectl", "get", "secret", name, "-n", namespace))
			Expect(err).To(HaveOccurred(), why)
		}

		It("only takes over existing roles and databases a Cluster editor allowlists", func() {
			By("leaving a DBA's existing role alone, even with a valid marker on it")
			_, err := psql(`CREATE ROLE pol_dba_app LOGIN PASSWORD 'dba-secret'`)
			Expect(err).NotTo(HaveOccurred())
			var dbaPassword string
			Eventually(func(g Gomega) { dbaPassword = passwordOf(g, "pol_dba_app") }).Should(Succeed())
			_, err = psql(`COMMENT ON ROLE pol_dba_app IS '` + validMarker("Role", "pol-adopt") + `'`)
			Expect(err).NotTo(HaveOccurred())
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
			expectReason(roleRes+"pol-adopt", "RoleNotManaged", "spec.rolePolicy.adoptableRoles")
			Consistently(func(g Gomega) { g.Expect(passwordOf(g, "pol_dba_app")).To(Equal(dbaPassword)) }, 20*time.Second, 2*time.Second).
				Should(Succeed())
			noSecret("example-cluster-pol-adopt-credentials", "the DBA's password must not be replaced and handed out")

			By("refusing an allowlisted role that is a member of a superuser")
			_, err = psql(`CREATE ROLE pol_adopt_su NOLOGIN IN ROLE pol_dba`)
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
			expectReason(roleRes+"pol-adopt-su", "RoleNotManaged", "adoptableRoles")
			setPolicy(`{"adoptableRoles":["pol_dba_app","pol_adopt_su"]}`)
			expectReason(roleRes+"pol-adopt-su", "RolePolicyViolation", "does not take over", "pol_dba")

			By("taking over the allowlisted ordinary role")
			waitReady(roleRes + "pol-adopt")
			Eventually(func(g Gomega) {
				g.Expect(passwordOf(g, "pol_dba_app")).NotTo(Equal(dbaPassword))
				g.Expect(jsonpath(g, roleRes+"pol-adopt", "{.status.roleName}")).To(Equal("pol_dba_app"))
			}).Should(Succeed())

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

			By("dropping the adopted role with its Role, but not roles it never owned")
			for _, res := range []string{roleRes + "pol-adopt", roleRes + "pol-adopt-su"} {
				_, err = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m", res))
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(psql(`SELECT count(*) FROM pg_roles WHERE rolname IN ('pol_dba_app', 'pol_adopt_su')`)).To(Equal("1"))
			Expect(psql(`SELECT count(*) FROM pg_roles WHERE rolname = 'pol_adopt_su'`)).To(Equal("1"))

			By("re-marking a recorded role whose marker is missing or unsigned")
			for i, legacy := range []string{`NULL`, `'pgop:v1:Role/pol-old'`} {
				_, err = psql(`COMMENT ON ROLE pol_old IS ` + legacy)
				Expect(err).NotTo(HaveOccurred())
				limit := fmt.Sprintf("%d", 6+i)
				_, err = utils.Run(exec.Command("kubectl", "patch", roleRes+"pol-old", "-n", namespace,
					"--type=merge", "-p", `{"spec":{"connectionLimit":`+limit+`}}`))
				Expect(err).NotTo(HaveOccurred())
				Eventually(func(g Gomega) {
					g.Expect(roleComment(g, "pol_old")).To(Equal(validMarker("Role", "pol-old")))
					g.Expect(query(g, `SELECT rolconnlimit FROM pg_roles WHERE rolname = 'pol_old'`)).To(Equal(limit))
				}).Should(Succeed())
			}

			By("leaving a DBA's existing database alone until it is allowlisted")
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
			expectReason(dbRes+"pol-dbaowned", "DatabaseNotManaged", "spec.rolePolicy.adoptableDatabases")
			Consistently(func(g Gomega) {
				g.Expect(query(g, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'pol_dbaowned'`)).NotTo(Equal("pol_old"))
			}, 15*time.Second, 3*time.Second).Should(Succeed())
			setPolicy(`{"adoptableDatabases":["pol_dbaowned"]}`)
			waitReady(dbRes + "pol-dbaowned")
			Eventually(func(g Gomega) {
				g.Expect(query(g, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'pol_dbaowned'`)).To(Equal("pol_old"))
			}).Should(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m", dbRes+"pol-dbaowned"))
			Expect(err).NotTo(HaveOccurred())
			Expect(psql(`SELECT count(*) FROM pg_database WHERE datname = 'pol_dbaowned'`)).To(Equal("0"), "an adopted database is dropped with its Database")
		})

		It("does not let a tenant hand itself a role or database with a copied marker", func() {
			// psqlAs runs a query as a tenant role over the local socket.
			psqlAs := func(user, query string) (string, error) {
				out, err := utils.Run(exec.Command("kubectl", "exec", "-n", namespace, clusterName+"-0", "-c", "postgresql", "--",
					"psql", "-U", user, "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-tAc", query))
				return strings.TrimSpace(out), err
			}
			_, err := psql(`CREATE ROLE pol_squatter LOGIN CREATEDB CREATEROLE`)
			Expect(err).NotTo(HaveOccurred())

			By("a role built through a throw-away role, marked, and left without members")
			// t creates w WITH CREATEROLE; w creates v (only w has ADMIN on v)
			// and marks it; t drops w: v has no members and a valid marker.
			_, err = psqlAs("pol_squatter", `CREATE ROLE pol_w CREATEROLE LOGIN`)
			Expect(err).NotTo(HaveOccurred())
			_, err = psqlAs("pol_w", `CREATE ROLE pol_svc LOGIN PASSWORD 'squatter-pw'`)
			Expect(err).NotTo(HaveOccurred())
			_, err = psqlAs("pol_w", `COMMENT ON ROLE pol_svc IS '`+validMarker("Role", "pol-svc")+`'`)
			Expect(err).NotTo(HaveOccurred())
			_, err = psqlAs("pol_squatter", `DROP ROLE pol_w`)
			Expect(err).NotTo(HaveOccurred())
			Expect(psql(`SELECT count(*) FROM pg_auth_members WHERE roleid = 'pol_svc'::regrole`)).To(Equal("0"))
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-svc
spec:
  clusterRef:
    name: example-cluster
  roleName: pol_svc
`)).To(Succeed())
			expectReason(roleRes+"pol-svc", "RoleNotManaged", "adoptableRoles")
			noSecret("example-cluster-pol-svc-credentials", "no password may be set or handed out for the squatted role")

			By("a database built by a tenant with CREATEDB and marked by its owner")
			_, err = psqlAs("pol_squatter", `CREATE DATABASE pol_squat`)
			Expect(err).NotTo(HaveOccurred())
			_, err = psqlAs("pol_squatter", `COMMENT ON DATABASE pol_squat IS '`+validMarker("Database", "pol-squat")+`'`)
			Expect(err).NotTo(HaveOccurred())
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: pol-squat
spec:
  clusterRef:
    name: example-cluster
  databaseName: pol_squat
  owner: pol-old
`)).To(Succeed())
			expectReason(dbRes+"pol-squat", "DatabaseNotManaged", "adoptableDatabases")
			Consistently(func(g Gomega) {
				g.Expect(query(g, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'pol_squat'`)).To(Equal("pol_squatter"))
			}, 15*time.Second, 3*time.Second).Should(Succeed())
			_, err = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m", dbRes+"pol-squat"))
			Expect(err).NotTo(HaveOccurred())
			Expect(psql(`SELECT count(*) FROM pg_database WHERE datname = 'pol_squat'`)).To(Equal("1"))
		})

		It("does not let a Role or Database follow its name to another or re-created Cluster", func() {
			By("refusing to retarget clusterRef")
			for _, res := range []string{roleRes + "pol-old"} {
				_, err := utils.Run(exec.Command("kubectl", "patch", res, "-n", namespace, "--type=merge",
					"-p", `{"spec":{"clusterRef":{"name":"cluster-b"}}}`))
				Expect(err).To(HaveOccurred(), res)
				Expect(err.Error()).To(ContainSubstring("clusterRef is immutable"))
			}

			By("treating names recorded on a Cluster with another UID (deleted and re-created) as not this resource's")
			Expect(apply(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: pol-uid
spec:
  clusterRef:
    name: example-cluster
  roleName: pol_uid
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: pol-uid-db
spec:
  clusterRef:
    name: example-cluster
  databaseName: pol_uid_db
  owner: pol-old
`)).To(Succeed())
			waitReady(roleRes + "pol-uid")
			waitReady(dbRes + "pol-uid-db")
			_, err := utils.Run(exec.Command("kubectl", "patch", dbRes+"pol-uid-db", "-n", namespace, "--type=merge",
				"-p", `{"spec":{"clusterRef":{"name":"cluster-b"}}}`))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("clusterRef is immutable"))
			var password string
			Eventually(func(g Gomega) { password = passwordOf(g, "pol_uid") }).Should(Succeed())
			// Simulate a re-created Cluster: the recorded clusterUID no longer
			// matches the Cluster the resources reference.
			for _, res := range []string{roleRes + "pol-uid", dbRes + "pol-uid-db"} {
				Eventually(func(g Gomega) {
					_, err := utils.Run(exec.Command("kubectl", "patch", res, "-n", namespace, "--subresource=status",
						"--type=merge", "-p", `{"status":{"clusterUID":"uid-of-a-deleted-cluster"}}`))
					g.Expect(err).NotTo(HaveOccurred())
					_, err = utils.Run(exec.Command("kubectl", "annotate", res, "-n", namespace, "--overwrite",
						"pgop.ruck.io/nudge="+time.Now().Format("150405.000")))
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(jsonpath(g, res, availableReason)).To(Or(Equal("RoleNotManaged"), Equal("DatabaseNotManaged")))
				}, 2*time.Minute, 5*time.Second).Should(Succeed())
			}
			_, err = utils.Run(exec.Command("kubectl", "annotate", roleRes+"pol-uid", "-n", namespace, "--overwrite",
				"pgop.ruck.io/rotate-password=after-uid-change"))
			Expect(err).NotTo(HaveOccurred())
			Consistently(func(g Gomega) { g.Expect(passwordOf(g, "pol_uid")).To(Equal(password)) }, 20*time.Second, 2*time.Second).
				Should(Succeed())

			By("never dropping them on delete")
			for _, res := range []string{dbRes + "pol-uid-db", roleRes + "pol-uid"} {
				_, err = utils.Run(exec.Command("kubectl", "delete", "-n", namespace, "--wait=true", "--timeout=2m", res))
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(psql(`SELECT count(*) FROM pg_roles WHERE rolname = 'pol_uid'`)).To(Equal("1"))
			Expect(psql(`SELECT count(*) FROM pg_database WHERE datname = 'pol_uid_db'`)).To(Equal("1"))
			_, err = psql(`DROP DATABASE pol_uid_db`)
			Expect(err).NotTo(HaveOccurred())
			_, err = psql(`DROP ROLE pol_uid`)
			Expect(err).NotTo(HaveOccurred())
		})

		It("re-marks recorded objects after the marker key is lost, and still refuses unrecorded ones", func() {
			oldMarker := validMarker("Role", "pol-old")
			_, err := utils.Run(exec.Command("kubectl", "delete", "secret", clusterName+"-marker-key", "-n", namespace))
			Expect(err).NotTo(HaveOccurred())
			var newMarker string
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "secret", clusterName+"-marker-key", "-n", namespace, "-o", "name"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())
				newMarker = validMarker("Role", "pol-old")
				g.Expect(newMarker).NotTo(Equal(oldMarker), "the Cluster generates a new key")
			}).Should(Succeed())
			nudge(roleRes + "pol-old")
			Eventually(func(g Gomega) { g.Expect(roleComment(g, "pol_old")).To(Equal(newMarker)) }).Should(Succeed())
			waitReady(roleRes + "pol-old")

			nudge(roleRes + "pol-svc")
			expectReason(roleRes+"pol-svc", "RoleNotManaged", "adoptableRoles")
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
