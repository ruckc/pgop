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
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ruckc/pgop/test/utils"
)

// RegisterReplicationTests adds streaming replication (issue #26 milestone 1)
// specs into the caller's Describe/Context block, which must have deployed
// the controller into `namespace`.
func RegisterReplicationTests() {
	Context("Replication", Ordered, func() {
		SetDefaultEventuallyTimeout(6 * time.Minute)
		SetDefaultEventuallyPollingInterval(3 * time.Second)

		const (
			name     = "repl"
			roleName = "repl-app"
			dbName   = "repl-db"
		)
		rwHost := fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace)
		roHost := fmt.Sprintf("%s-ro.%s.svc.cluster.local", name, namespace)

		kubectl := func(args ...string) (string, error) {
			out, err := utils.Run(exec.Command("kubectl", append([]string{"-n", namespace}, args...)...))
			return strings.TrimSpace(out), err
		}
		get := func(g Gomega, args ...string) string {
			out, err := kubectl(append([]string{"get"}, args...)...)
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}
		apply := func(manifest string) {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		}
		scale := func(n int) {
			_, err := kubectl("patch", "cluster", name, "--type=merge", "-p", fmt.Sprintf(`{"spec":{"replicas":%d}}`, n))
			Expect(err).NotTo(HaveOccurred())
		}
		// local runs a query over the pod's Unix socket.
		local := func(pod, query string) (string, error) {
			return kubectl("exec", pod, "-c", "postgresql", "--",
				"psql", "-U", "pgop_operator", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-tAc", query)
		}
		password := func(g Gomega) string {
			b64 := get(g, "secret", name+"-credentials", "-o", "jsonpath={.data.password}")
			pw, err := base64.StdEncoding.DecodeString(b64)
			g.Expect(err).NotTo(HaveOccurred())
			return string(pw)
		}
		// remote runs a query from inside pod over TCP to host, verifying the
		// server certificate (verify-full) like a client would.
		remote := func(g Gomega, pod, host, query string) (string, error) {
			return kubectl("exec", pod, "-c", "postgresql", "--",
				"env", "PGPASSWORD="+password(g), "PGSSLMODE=verify-full", "PGSSLROOTCERT=/etc/pgop/tls/ca.crt",
				"psql", "-h", host, "-U", "pgop_operator", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-tAc", query)
		}
		podUID := func(g Gomega, pod string) string {
			return get(g, "pod", pod, "-o", "jsonpath={.metadata.uid}")
		}
		podIP := func(g Gomega, pod string) string {
			return get(g, "pod", pod, "-o", "jsonpath={.status.podIP}")
		}
		endpoints := func(g Gomega, svc string) string {
			return get(g, "endpointslices", "-l", "kubernetes.io/service-name="+svc,
				"-o", "jsonpath={.items[*].endpoints[?(@.conditions.ready==true)].addresses[*]}")
		}
		// waitHealthy waits until the Cluster is ready with n instances and,
		// with standbys, every standby streams.
		waitHealthy := func(n int) {
			Eventually(func(g Gomega) {
				g.Expect(get(g, "cluster", name, "-o", "jsonpath={.status.ready}/{.status.readyInstances}")).
					To(Equal(fmt.Sprintf("true/%d", n)))
				if n > 1 {
					g.Expect(get(g, "cluster", name, "-o",
						`jsonpath={.status.conditions[?(@.type=="ReplicationHealthy")].status}`)).To(Equal("True"))
				}
				g.Expect(get(g, "cluster", name, "-o",
					`jsonpath={.status.conditions[?(@.type=="TLSReady")].status}`)).To(Equal("True"))
			}).Should(Succeed())
		}
		slots := func(g Gomega) string {
			out, err := local(name+"-0", "SELECT string_agg(slot_name, ',' ORDER BY slot_name) FROM pg_replication_slots")
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}
		exists := func(kind, n string) bool {
			out, err := kubectl("get", kind, n, "--ignore-not-found", "-o", "name")
			return err == nil && out != ""
		}

		AfterAll(func() {
			_, _ = kubectl("delete", "databases.pgop.ruck.io", dbName, "--ignore-not-found", "--wait=false")
			_, _ = kubectl("delete", "roles.pgop.ruck.io", roleName, "--ignore-not-found", "--wait=false")
			_, _ = kubectl("delete", "cluster", name, "--ignore-not-found", "--wait=false")
		})

		It("starts a primary and a streaming standby", func() {
			apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: %s
spec:
  replicas: 2
  storage:
    retainPolicy: Delete
  tls: {}
  parameters:
    work_mem: 8MB
`, name))
			waitHealthy(2)

			Expect(get(Default, "pod", name+"-0", "-o", `jsonpath={.metadata.labels.pgop\.ruck\.io/role}`)).To(Equal("primary"))
			Expect(get(Default, "pod", name+"-1", "-o", `jsonpath={.metadata.labels.pgop\.ruck\.io/role}`)).To(Equal("replica"))
			Expect(get(Default, "cluster", name, "-o", "jsonpath={.status.currentPrimary}")).To(Equal(name + "-0"))
			Expect(get(Default, "cluster", name, "-o", "jsonpath={.status.readOnlyEndpoint}")).To(Equal(roHost + ":5432"))

			Eventually(func(g Gomega) {
				// One standby streams over TLS, as pgop_replicator.
				out, err := local(name+"-0", `SELECT r.application_name || '/' || r.usename || '/' || s.ssl
FROM pg_stat_replication r JOIN pg_stat_ssl s USING (pid) WHERE r.state = 'streaming'`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal(name + "-1/pgop_replicator/true"))
			}).Should(Succeed())
			Expect(slots(Default)).To(Equal("pgop_replica_1"))

			By("the standby is in recovery and uses the same parameters and TLS settings")
			Expect(local(name+"-1", "SELECT pg_is_in_recovery()")).To(Equal("t"))
			Expect(local(name+"-1", "SHOW work_mem")).To(Equal("8MB"))
			Expect(local(name+"-1", "SHOW ssl")).To(Equal("on"))
			Expect(local(name+"-1", "SHOW primary_slot_name")).To(Equal("pgop_replica_1"))
			Expect(local(name+"-0", "SELECT pg_is_in_recovery()")).To(Equal("f"))
		})

		It("routes the read-write Service only to the primary and the -ro Service to the standby", func() {
			ip0, ip1 := podIP(Default, name+"-0"), podIP(Default, name+"-1")
			Eventually(func(g Gomega) {
				g.Expect(endpoints(g, name)).To(Equal(ip0))
				g.Expect(endpoints(g, name+"-ro")).To(Equal(ip1))
			}).Should(Succeed())

			for range 10 {
				Expect(remote(Default, name+"-1", rwHost, "SELECT pg_is_in_recovery()")).To(Equal("f"))
			}
			Expect(remote(Default, name+"-0", roHost, "SELECT pg_is_in_recovery()")).To(Equal("t"))
		})

		It("replicates writes on the primary to the standby, which rejects writes", func() {
			_, err := remote(Default, name+"-1", rwHost,
				"CREATE TABLE repl_e2e (id int PRIMARY KEY, v text); INSERT INTO repl_e2e VALUES (1, 'from-primary')")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				out, err := remote(g, name+"-0", roHost, "SELECT v FROM repl_e2e WHERE id = 1")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("from-primary"))
			}).Should(Succeed())

			out, err := remote(Default, name+"-0", roHost, "INSERT INTO repl_e2e VALUES (2, 'via-ro')")
			Expect(err).To(HaveOccurred())
			Expect(out).To(ContainSubstring("read-only transaction"))
		})

		It("manages Roles and Databases on the primary", func() {
			apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: %[1]s
spec:
  clusterRef:
    name: %[3]s
  roleName: repl_app
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: %[2]s
spec:
  clusterRef:
    name: %[3]s
  databaseName: repl_db
  owner: %[1]s
`, roleName, dbName, name))
			Eventually(func(g Gomega) {
				g.Expect(get(g, "roles.pgop.ruck.io", roleName, "-o", "jsonpath={.status.ready}")).To(Equal("true"))
				g.Expect(get(g, "databases.pgop.ruck.io", dbName, "-o", "jsonpath={.status.ready}")).To(Equal("true"))
				out, err := local(name+"-1", "SELECT count(*) FROM pg_database WHERE datname = 'repl_db'")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("1"))
			}).Should(Succeed())
		})

		It("scales up without restarting the primary", func() {
			uid0 := podUID(Default, name+"-0")
			scale(3)
			waitHealthy(3)
			Expect(podUID(Default, name+"-0")).To(Equal(uid0))
			Expect(slots(Default)).To(Equal("pgop_replica_1,pgop_replica_2"))
			Eventually(func(g Gomega) {
				out, err := local(name+"-2", "SELECT v FROM repl_e2e WHERE id = 1")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("from-primary"))
			}).Should(Succeed())
		})

		It("scales down to one instance, removing standby volumes and slots", func() {
			uid0 := podUID(Default, name+"-0")
			scale(1)
			Eventually(func(g Gomega) {
				g.Expect(get(g, "cluster", name, "-o", "jsonpath={.status.ready}/{.status.readyInstances}")).To(Equal("true/1"))
				g.Expect(exists("pod", name+"-1")).To(BeFalse())
				g.Expect(exists("pod", name+"-2")).To(BeFalse())
				g.Expect(exists("pvc", "data-"+name+"-1")).To(BeFalse())
				g.Expect(exists("pvc", "data-"+name+"-2")).To(BeFalse())
				g.Expect(exists("service", name+"-ro")).To(BeFalse())
				g.Expect(slots(g)).To(BeEmpty())
				g.Expect(get(g, "cluster", name, "-o",
					`jsonpath={.status.conditions[?(@.type=="ReplicationHealthy")].status}`)).To(BeEmpty())
			}).Should(Succeed())
			Expect(exists("pvc", "data-"+name+"-0")).To(BeTrue())
			Expect(podUID(Default, name+"-0")).To(Equal(uid0), "scaling down must not restart the primary")
			Expect(remote(Default, name+"-0", rwHost, "SELECT v FROM repl_e2e WHERE id = 1")).To(Equal("from-primary"))
		})

		It("re-clones a standby from scratch when scaled up again", func() {
			uid0 := podUID(Default, name+"-0")
			_, err := remote(Default, name+"-0", rwHost, "INSERT INTO repl_e2e VALUES (3, 'after-scale-down')")
			Expect(err).NotTo(HaveOccurred())
			scale(2)
			waitHealthy(2)
			Expect(podUID(Default, name+"-0")).To(Equal(uid0))
			Eventually(func(g Gomega) {
				out, err := local(name+"-1", "SELECT string_agg(v, ',' ORDER BY id) FROM repl_e2e")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("from-primary,after-scale-down"))
			}).Should(Succeed())
		})

		It("rejoins a standby whose pod was deleted", func() {
			uid1 := podUID(Default, name+"-1")
			_, err := kubectl("delete", "pod", name+"-1", "--wait=true")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(podUID(g, name+"-1")).NotTo(Equal(uid1))
			}).Should(Succeed())
			waitHealthy(2)
			_, err = remote(Default, name+"-1", rwHost, "INSERT INTO repl_e2e VALUES (4, 'after-rejoin')")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				out, err := local(name+"-1", "SELECT v FROM repl_e2e WHERE id = 4")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("after-rejoin"))
			}).Should(Succeed())
		})
	})
}
