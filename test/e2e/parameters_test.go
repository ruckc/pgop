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
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ruckc/pgop/test/utils"
)

// RegisterParametersTests adds spec.parameters (issue #23) specs into the
// caller's Describe/Context block, which must have deployed the controller
// into `namespace`.
func RegisterParametersTests() {
	Context("Parameters", Ordered, func() {
		// The kubelet syncs mounted ConfigMaps with a delay of up to about a
		// minute, so allow for that on top of pod restarts.
		SetDefaultEventuallyTimeout(5 * time.Minute)
		SetDefaultEventuallyPollingInterval(2 * time.Second)

		const (
			pg18Cluster = "params"
			pg16Cluster = "params-pg16"
		)

		kubectl := func(args ...string) (string, error) {
			out, err := utils.Run(exec.Command("kubectl", append([]string{"-n", namespace}, args...)...))
			return strings.TrimSpace(out), err
		}
		get := func(g Gomega, args ...string) string {
			out, err := kubectl(append([]string{"get"}, args...)...)
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}
		apply := func(manifest string) error {
			cmd := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			return err
		}
		patchParams := func(name, params string) {
			_, err := kubectl("patch", "cluster", name, "--type=merge",
				"-p", fmt.Sprintf(`{"spec":{"parameters":%s}}`, params))
			Expect(err).NotTo(HaveOccurred())
		}
		// psql runs a query over the pod's local socket.
		psql := func(g Gomega, name, query string) string {
			out, err := kubectl("exec", name+"-0", "-c", "postgresql", "--",
				"psql", "-U", "pgop_operator", "-d", "postgres", "-tAc", query)
			g.Expect(err).NotTo(HaveOccurred())
			return strings.TrimSpace(out)
		}
		podUID := func(g Gomega, name string) string {
			return get(g, "pod", name+"-0", "-o", "jsonpath={.metadata.uid}")
		}
		// state is ready/ParametersApplied status/reason.
		state := func(g Gomega, name string) string {
			return get(g, "cluster", name, "-o",
				`jsonpath={.status.ready}/{.status.conditions[?(@.type=="ParametersApplied")].status}/`+
					`{.status.conditions[?(@.type=="ParametersApplied")].reason}`)
		}
		waitApplied := func(name string, settings map[string]string) {
			Eventually(func(g Gomega) {
				g.Expect(state(g, name)).To(Equal("true/True/Applied"))
				g.Expect(get(g, "cluster", name, "-o", "jsonpath={.status.pendingRestart}")).To(BeEmpty())
				for k, v := range settings {
					g.Expect(psql(g, name, "SHOW "+k)).To(Equal(v))
				}
			}).Should(Succeed())
		}

		AfterAll(func() {
			for _, c := range []string{pg18Cluster, pg16Cluster} {
				_, _ = kubectl("delete", "cluster", c, "--ignore-not-found", "--wait=false")
			}
		})

		It("starts a TLS Cluster with parameters from the generated config file", func() {
			Expect(apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: %s
spec:
  storage:
    retainPolicy: Delete
  tls: {}
  parameters:
    work_mem: 8MB
`, pg18Cluster))).To(Succeed())
			waitApplied(pg18Cluster, map[string]string{"work_mem": "8MB"})

			Eventually(func(g Gomega) {
				g.Expect(psql(g, pg18Cluster, "SHOW config_file")).To(Equal("/etc/pgop/config/postgresql.conf"))
				// spec.tls keeps working next to the generated file.
				g.Expect(psql(g, pg18Cluster, "SHOW ssl")).To(Equal("on"))
				g.Expect(psql(g, pg18Cluster, "SHOW hba_file")).To(Equal("/etc/pgop/hba/pg_hba.conf"))
				// Defaults from the data directory's postgresql.conf still apply.
				g.Expect(psql(g, pg18Cluster, "SHOW listen_addresses")).To(Equal("*"))
				g.Expect(get(g, "cluster", pg18Cluster, "-o",
					`jsonpath={.status.conditions[?(@.type=="TLSReady")].status}`)).To(Equal("True"))
			}).Should(Succeed())
		})

		It("reloads a reload-only parameter without restarting the pod", func() {
			uid := podUID(Default, pg18Cluster)
			patchParams(pg18Cluster, `{"work_mem":"16MB"}`)
			waitApplied(pg18Cluster, map[string]string{"work_mem": "16MB"})
			Expect(podUID(Default, pg18Cluster)).To(Equal(uid))
		})

		It("restarts the pod once for shared_preload_libraries", func() {
			uid := podUID(Default, pg18Cluster)
			patchParams(pg18Cluster, `{"work_mem":"16MB","shared_preload_libraries":"pg_stat_statements"}`)
			waitApplied(pg18Cluster, map[string]string{
				"work_mem":                 "16MB",
				"shared_preload_libraries": "pg_stat_statements",
			})
			Expect(podUID(Default, pg18Cluster)).NotTo(Equal(uid))
			Expect(get(Default, "statefulset", pg18Cluster, "-o",
				`jsonpath={.spec.template.metadata.annotations.pgop\.ruck\.io/parameters-restart}`)).NotTo(BeEmpty())
			Expect(psql(Default, pg18Cluster,
				"CREATE EXTENSION IF NOT EXISTS pg_stat_statements; SELECT count(*) > 0 FROM pg_stat_statements")).
				To(HaveSuffix("t"))
		})

		It("reports an invalid value without reloading or restarting", func() {
			uid := podUID(Default, pg18Cluster)
			patchParams(pg18Cluster, `{"work_mem":"lots","shared_preload_libraries":"pg_stat_statements"}`)
			Eventually(func(g Gomega) {
				g.Expect(state(g, pg18Cluster)).To(Equal("true/False/InvalidParameter"))
			}).Should(Succeed())
			Expect(psql(Default, pg18Cluster, "SHOW work_mem")).To(Equal("16MB"))
			Expect(podUID(Default, pg18Cluster)).To(Equal(uid))

			By("recovering once the value is fixed")
			patchParams(pg18Cluster, `{"work_mem":"32MB","shared_preload_libraries":"pg_stat_statements"}`)
			waitApplied(pg18Cluster, map[string]string{"work_mem": "32MB"})
			Expect(podUID(Default, pg18Cluster)).To(Equal(uid))
		})

		It("reports a parameter overridden by ALTER SYSTEM", func() {
			// The operator re-checks every few minutes; touching the Cluster
			// triggers a reconcile right away.
			touch := func(n int) {
				_, err := kubectl("annotate", "--overwrite", "cluster", pg18Cluster, fmt.Sprintf("e2e/touch=%d", n))
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(psql(Default, pg18Cluster, "ALTER SYSTEM SET work_mem = '1MB'")).To(Equal("ALTER SYSTEM"))
			Expect(psql(Default, pg18Cluster, "SELECT pg_reload_conf()")).To(Equal("t"))
			n := 0
			Eventually(func(g Gomega) {
				n++
				touch(n)
				g.Expect(state(g, pg18Cluster)).To(Equal("true/False/OverriddenByAlterSystem"))
			}).WithPolling(5 * time.Second).Should(Succeed())

			Expect(psql(Default, pg18Cluster, "ALTER SYSTEM RESET work_mem")).To(Equal("ALTER SYSTEM"))
			Expect(psql(Default, pg18Cluster, "SELECT pg_reload_conf()")).To(Equal("t"))
			Eventually(func(g Gomega) {
				n++
				touch(n)
				g.Expect(state(g, pg18Cluster)).To(Equal("true/True/Applied"))
			}).WithPolling(5 * time.Second).Should(Succeed())
			waitApplied(pg18Cluster, map[string]string{"work_mem": "32MB"})
		})

		It("rejects operator-managed parameters", func() {
			err := apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: %s
spec:
  parameters:
    port: "5433"
`, pg18Cluster))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("operator-managed"))
		})

		It("removes the generated config file when the last parameter is removed", func() {
			_, err := kubectl("patch", "cluster", pg18Cluster, "--type=json",
				"-p", `[{"op":"remove","path":"/spec/parameters"}]`)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(get(g, "configmap", "--ignore-not-found", pg18Cluster+"-config", "-o", "name")).To(BeEmpty())
				g.Expect(state(g, pg18Cluster)).To(Equal("true//"))
				g.Expect(psql(g, pg18Cluster, "SHOW config_file")).To(HaveSuffix("/docker/postgresql.conf"))
				g.Expect(psql(g, pg18Cluster, "SHOW ssl")).To(Equal("on"))
			}).Should(Succeed())
		})

		It("initializes a PostgreSQL 16 Cluster with restart-only parameters", func() {
			Expect(apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: %s
spec:
  image: postgres:16
  storage:
    retainPolicy: Delete
  parameters:
    shared_preload_libraries: pg_stat_statements
    max_connections: "150"
`, pg16Cluster))).To(Succeed())
			waitApplied(pg16Cluster, map[string]string{
				"shared_preload_libraries": "pg_stat_statements",
				"max_connections":          "150",
			})
			Expect(psql(Default, pg16Cluster, "SHOW config_file")).To(Equal("/etc/pgop/config/postgresql.conf"))
			Expect(psql(Default, pg16Cluster, "SHOW data_directory")).To(Equal("/var/lib/postgresql/data"))
		})
	})
}
