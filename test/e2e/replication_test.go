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
		// endpoints returns the ready addresses behind a Service (a jsonpath
		// filter fails on a slice without endpoints, so filter here).
		endpoints := func(g Gomega, svc string) string {
			out := get(g, "endpointslices", "-l", "kubernetes.io/service-name="+svc,
				"-o", `jsonpath={range .items[*].endpoints[*]}{.conditions.ready}={.addresses[0]}{"\n"}{end}`)
			var ready []string
			for line := range strings.Lines(out) {
				if addr, ok := strings.CutPrefix(strings.TrimSpace(line), "true="); ok {
					ready = append(ready, addr)
				}
			}
			return strings.Join(ready, " ")
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
		streamingLabel := func(g Gomega, pod string) string {
			return get(g, "pod", pod, "-o", `jsonpath={.metadata.labels.pgop\.ruck\.io/streaming}`)
		}
		// primaryUsable checks what clients and Role/Database reconciles
		// depend on: the Cluster is ready, TLS is active and the credentials
		// Secret tells clients to verify the server.
		primaryUsable := func(g Gomega) {
			g.Expect(get(g, "cluster", name, "-o", "jsonpath={.status.ready}")).To(Equal("true"))
			g.Expect(get(g, "cluster", name, "-o",
				`jsonpath={.status.conditions[?(@.type=="TLSReady")].status}`)).To(Equal("True"))
			sslmode, err := base64.StdEncoding.DecodeString(get(g, "secret", name+"-credentials", "-o", "jsonpath={.data.sslmode}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(sslmode)).To(Equal("verify-full"))
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
    max_slot_wal_keep_size: 64MB
`, name))
			waitHealthy(2)

			Expect(get(Default, "pod", name+"-0", "-o", `jsonpath={.metadata.labels.pgop\.ruck\.io/role}`)).To(Equal("primary"))
			Expect(get(Default, "pod", name+"-1", "-o", `jsonpath={.metadata.labels.pgop\.ruck\.io/role}`)).To(Equal("replica"))
			Expect(streamingLabel(Default, name+"-1")).To(Equal("true"))
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

		It("rejoins a standby whose pod was deleted, without affecting the primary's clients", func() {
			uid1 := podUID(Default, name+"-1")
			_, err := kubectl("delete", "pod", name+"-1", "--wait=false")
			Expect(err).NotTo(HaveOccurred())
			// While the standby is down and restarting, the Cluster stays
			// ready and clients keep verify-full.
			Consistently(primaryUsable).WithTimeout(20 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
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

		It("takes a standby with an invalidated slot out of -ro and re-clones it", func() {
			By("cutting the standby off and generating more WAL than max_slot_wal_keep_size")
			_, err := local(name+"-0", "ALTER ROLE pgop_replicator NOLOGIN")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				_, err := local(name+"-0",
					"SELECT pg_terminate_backend(pid) FROM pg_stat_replication WHERE application_name = '"+name+"-1'")
				g.Expect(err).NotTo(HaveOccurred())
				out, err := local(name+"-0", "SELECT count(*) FROM pg_stat_replication WHERE application_name = '"+name+"-1'")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("0"))
			}).Should(Succeed())
			_, err = local(name+"-0", "CREATE TABLE repl_big AS SELECT g, repeat('x', 1000) AS v FROM generate_series(1, 150000) g")
			Expect(err).NotTo(HaveOccurred())
			_, err = local(name+"-0", "SELECT pg_switch_wal(); CHECKPOINT")
			Expect(err).NotTo(HaveOccurred())

			By("the operator re-creates the slot, reports it, and removes the standby from -ro")
			Eventually(func(g Gomega) {
				g.Expect(get(g, "events", "--field-selector", "reason=ReplicationSlotInvalidated,involvedObject.name="+name,
					"-o", "name")).NotTo(BeEmpty())
				g.Expect(streamingLabel(g, name+"-1")).To(Equal("false"))
				g.Expect(endpoints(g, name+"-ro")).To(BeEmpty())
				primaryUsable(g)
			}).Should(Succeed())
			_, err = local(name+"-0", "ALTER ROLE pgop_replicator LOGIN")
			Expect(err).NotTo(HaveOccurred())
			Consistently(func(g Gomega) {
				g.Expect(streamingLabel(g, name+"-1")).To(Equal("false"))
			}).WithTimeout(20 * time.Second).WithPolling(5 * time.Second).Should(Succeed())

			By("re-cloning the standby")
			_, err = kubectl("delete", "pvc", "data-"+name+"-1", "--wait=false")
			Expect(err).NotTo(HaveOccurred())
			_, err = kubectl("delete", "pod", name+"-1")
			Expect(err).NotTo(HaveOccurred())
			waitHealthy(2)
			Eventually(func(g Gomega) {
				g.Expect(streamingLabel(g, name+"-1")).To(Equal("true"))
				out, err := local(name+"-1", "SELECT count(*) FROM repl_big")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("150000"))
				g.Expect(endpoints(g, name+"-ro")).To(Equal(podIP(g, name+"-1")))
			}).Should(Succeed())
		})

		It("restarts only the standbys when the replication password is rotated", func() {
			uid0, uid1 := podUID(Default, name+"-0"), podUID(Default, name+"-1")
			newPassword := base64.StdEncoding.EncodeToString([]byte("rotated-replication-password"))
			_, err := kubectl("patch", "secret", name+"-credentials", "--type=merge",
				"-p", fmt.Sprintf(`{"data":{"replication-password":%q}}`, newPassword))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(podUID(g, name+"-1")).NotTo(Equal(uid1))
			}).Should(Succeed())
			waitHealthy(2)
			Expect(podUID(Default, name+"-0")).To(Equal(uid0))
			Eventually(func(g Gomega) {
				g.Expect(streamingLabel(g, name+"-1")).To(Equal("true"))
			}).Should(Succeed())
		})
	})
}
