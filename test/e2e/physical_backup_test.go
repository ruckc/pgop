//go:build e2e
// +build e2e

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

// physicalS3Namespace runs RustFS behind a TLS proxy: pgBackRest only talks
// HTTPS to S3. It is separate from the logical backup tests' namespace,
// which those delete when they finish.
const physicalS3Namespace = "rustfs-tls"

// RegisterPhysicalBackupTests takes pgBackRest backups of a Cluster to
// RustFS, deletes the data and restores it: to a point in time, and to the
// state of a backup.
func RegisterPhysicalBackupTests() {
	Context("Backup — physical pgBackRest via RustFS", Ordered, func() {
		SetDefaultEventuallyTimeout(10 * time.Minute)
		SetDefaultEventuallyPollingInterval(5 * time.Second)

		const (
			name       = "pbk"
			backupName = "pbk-backup"
			fullJob    = "pbk-full-1"
		)
		s3Host := fmt.Sprintf("rustfs-tls.%s.svc.cluster.local", physicalS3Namespace)

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
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = newStringReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		}
		// sql runs a query on the primary over its Unix socket.
		sql := func(query string) (string, error) {
			return kubectl("exec", name+"-0", "-c", "postgresql", "--",
				"psql", "-U", "pgop_operator", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-tAc", query)
		}
		mustSQL := func(query string) string {
			out, err := sql(query)
			Expect(err).NotTo(HaveOccurred(), query)
			return out
		}
		rows := func() string {
			return mustSQL("SELECT string_agg(v, ',' ORDER BY v) FROM pbk_items")
		}
		waitReady := func() {
			Eventually(func(g Gomega) {
				g.Expect(get(g, "cluster", name, "-o", "jsonpath={.status.ready}")).To(Equal("true"))
				out, err := sql("SELECT pg_is_in_recovery()")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("f"))
			}).Should(Succeed())
		}
		// waitArchived switches to a new WAL segment and waits until the
		// previous one is in the repository.
		waitArchived := func() {
			segment := mustSQL("SELECT pg_walfile_name(pg_switch_wal())")
			Eventually(func(g Gomega) {
				out, err := sql("SELECT coalesce(last_archived_wal, '') FROM pg_stat_archiver")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out >= segment).To(BeTrue(), "last archived %q, waiting for %q", out, segment)
			}).Should(Succeed())
		}
		restore := func(restoreName, extra string) {
			apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Restore
metadata:
  name: %s
  namespace: %s
spec:
  type: physical
  backupRunRef:
    name: %s
  clusterRef:
    name: %s
%s`, restoreName, namespace, fullJob, name, extra))

			By("the Restore waits for the Cluster's confirmation")
			Eventually(func(g Gomega) {
				g.Expect(get(g, "restore.pgop.ruck.io", restoreName, "-o",
					`jsonpath={.status.conditions[?(@.type=="Available")].reason}`)).To(Equal("AwaitingConfirmation"))
			}).Should(Succeed())
			Expect(get(Default, "cluster", name, "-o", "jsonpath={.status.ready}")).To(Equal("true"), "not stopped yet")
			_, err := kubectl("annotate", "cluster", name, "--overwrite", "pgop.ruck.io/allow-restore="+restoreName)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				phase := get(g, "restore.pgop.ruck.io", restoreName, "-o", "jsonpath={.status.phase}")
				if phase == "Failed" {
					status, _ := kubectl("get", "restore.pgop.ruck.io", restoreName, "-o", "jsonpath={.status}")
					logs, _ := kubectl("logs", "job/"+restoreName+"-restore", "--all-containers", "--tail=50")
					StopTrying("restore failed").Wrap(fmt.Errorf("status %s\nJob logs:\n%s", status, logs)).Now()
				}
				g.Expect(phase).To(Equal("Succeeded"))
			}).Should(Succeed())
			Expect(get(Default, "cluster", name, "-o", `jsonpath={.metadata.annotations.pgop\.ruck\.io/restore-in-progress}`)).To(BeEmpty())
			Expect(get(Default, "cluster", name, "-o", `jsonpath={.metadata.annotations.pgop\.ruck\.io/allow-restore}`)).
				To(BeEmpty(), "the confirmation is used up")
			Expect(get(Default, "cluster", name, "-o", "jsonpath={.status.lastRestore.name}/{.status.lastRestore.result}")).
				To(Equal(restoreName + "/Succeeded"))
			waitReady()
		}

		BeforeAll(func() {
			setupRustFS(physicalS3Namespace)
			deployRustFSTLSProxy(physicalS3Namespace)
			applyRustFSCredentials()

			By("copying the RustFS CA into the manager namespace")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "secret", "rustfs-tls-cert", "-n", physicalS3Namespace,
					"-o", "jsonpath={.data.ca\\.crt}"))
				g.Expect(err).NotTo(HaveOccurred())
				ca, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(ca)).To(ContainSubstring("BEGIN CERTIFICATE"))
				secret, err := utils.Run(exec.Command("kubectl", "create", "secret", "generic", "rustfs-ca", "-n", namespace,
					"--from-literal=ca.crt="+string(ca), "--dry-run=client", "-o", "yaml"))
				g.Expect(err).NotTo(HaveOccurred())
				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = newStringReader(secret)
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}, 3*time.Minute).Should(Succeed())
		})

		AfterAll(func() {
			for _, r := range []string{"pbk-restore-pitr", "pbk-restore-set"} {
				_, _ = kubectl("delete", "restore.pgop.ruck.io", r, "--ignore-not-found", "--wait=false")
			}
			_, _ = kubectl("delete", "backup.pgop.ruck.io", backupName, "--ignore-not-found", "--wait=false")
			_, _ = kubectl("delete", "cluster", name, "--ignore-not-found", "--wait=false")
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", physicalS3Namespace, "--ignore-not-found", "--wait=false"))
		})

		It("archives WAL and takes a full backup", func() {
			apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: %s
  namespace: %s
spec:
  storage:
    retainPolicy: Delete
`, name, namespace))
			waitReady()
			By("the Cluster without a physical Backup runs the official image without a sidecar")
			Expect(get(Default, "statefulset", name, "-o", "jsonpath={.spec.template.spec.containers[*].name}")).To(Equal("postgresql"))
			Expect(get(Default, "statefulset", name, "-o", "jsonpath={.spec.template.spec.containers[0].image}")).To(Equal("postgres:18"))

			apply(fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Backup
metadata:
  name: %s
  namespace: %s
spec:
  type: physical
  clusterRef:
    name: %s
  physical:
    fullSchedule: "0 0 1 1 *"
    incrementalSchedule: "0 0 1 1 *"
  backupRunTTL: "2h"
  destination:
    type: s3
    s3:
      bucket: pgop-backups
      prefix: physical/%s
      region: us-east-1
      endpoint: https://%s
      credentialsSecretRef:
        name: rustfs-credentials
      caSecretRef:
        name: rustfs-ca
        key: ca.crt
`, backupName, namespace, name, name, s3Host))

			By("the Cluster runs the Postgres+pgBackRest image with the pgbackrest sidecar")
			Eventually(func(g Gomega) {
				g.Expect(get(g, "statefulset", name, "-o", "jsonpath={.spec.template.spec.containers[*].name}")).
					To(Equal("postgresql pgbackrest"))
				g.Expect(get(g, "pod", name+"-0", "-o", "jsonpath={.spec.containers[0].image}")).To(Equal(postgresPgbackrestImage))
				g.Expect(get(g, "pod", name+"-0", "-o", "jsonpath={.status.containerStatuses[*].ready}")).To(Equal("true true"))
			}).Should(Succeed())
			waitReady()
			Expect(mustSQL("SHOW archive_mode")).To(Equal("on"))
			Expect(mustSQL("SHOW archive_command")).To(ContainSubstring("pgbackrest"))
			Expect(get(Default, "backup.pgop.ruck.io", backupName, "-o",
				`jsonpath={.status.conditions[?(@.type=="Available")].status}`)).To(Equal("True"))

			By("writing data and checking that WAL reaches the repository")
			mustSQL("CREATE TABLE pbk_items (v text)")
			mustSQL("INSERT INTO pbk_items VALUES ('a'), ('b'), ('c')")
			waitArchived()
			Eventually(func(g Gomega) {
				g.Expect(get(g, "cluster", name, "-o",
					`jsonpath={.status.conditions[?(@.type=="WALArchiving")].status}`)).To(Equal("True"))
				g.Expect(get(g, "backup.pgop.ruck.io", backupName, "-o",
					`jsonpath={.status.conditions[?(@.type=="WALArchiving")].status}`)).To(Equal("True"))
			}, 4*time.Minute).Should(Succeed())
			Expect(get(Default, "pod", name+"-0", "-o",
				`jsonpath={.spec.containers[0].env[?(@.name=="PGBACKREST_ARCHIVE_PUSH_QUEUE_MAX")].value}`)).To(Equal("268435456"))

			By("taking a full backup from the CronJob")
			_, err := kubectl("create", "job", fullJob, "--from=cronjob/"+backupName+"-full")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				if get(g, "job", fullJob, "-o", `jsonpath={.status.conditions[?(@.type=="Failed")].status}`) == "True" {
					logs, _ := kubectl("logs", "job/"+fullJob, "--all-containers", "--tail=50")
					StopTrying("backup Job failed").Wrap(fmt.Errorf("Job logs:\n%s", logs)).Now()
				}
				g.Expect(get(g, "job", fullJob, "-o", "jsonpath={.status.succeeded}")).To(Equal("1"))
			}).Should(Succeed())
			logs, _ := kubectl("logs", "job/"+fullJob)
			_, _ = fmt.Fprintf(GinkgoWriter, "backup Job logs:\n%s\n", logs)

			By("recording the backup as a BackupRun and in the Backup status")
			Eventually(func(g Gomega) {
				g.Expect(get(g, "backuprun.pgop.ruck.io", fullJob, "-o", "jsonpath={.status.phase}")).To(Equal("Succeeded"))
				g.Expect(get(g, "backuprun.pgop.ruck.io", fullJob, "-o", "jsonpath={.status.location}")).
					To(MatchRegexp(`^s3://pgop-backups/physical/pbk/backup/main/[0-9]{8}-[0-9]{6}F$`))
				g.Expect(get(g, "backup.pgop.ruck.io", backupName, "-o", "jsonpath={.status.lastFullBackupTime}")).NotTo(BeEmpty())
			}).Should(Succeed())
		})

		It("restores to a point in time", func() {
			mustSQL("INSERT INTO pbk_items VALUES ('d')")
			time.Sleep(2 * time.Second)
			target := mustSQL(`SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`)
			time.Sleep(2 * time.Second)
			mustSQL("DELETE FROM pbk_items")
			Expect(rows()).To(BeEmpty())
			waitArchived()

			restore("pbk-restore-pitr", fmt.Sprintf("  targetTime: %q\n", target))
			Expect(rows()).To(Equal("a,b,c,d"))
		})

		It("restores the state of a backup", func() {
			mustSQL("DELETE FROM pbk_items")
			restore("pbk-restore-set", "")
			Expect(rows()).To(Equal("a,b,c"))

			By("the restored Cluster archives WAL again")
			mustSQL("INSERT INTO pbk_items VALUES ('e')")
			waitArchived()
		})
	})
}

// deployRustFSTLSProxy serves the RustFS in ns over HTTPS (nginx with a
// cert-manager certificate from a self-signed CA) as the Service
// "rustfs-tls" on port 443.
func deployRustFSTLSProxy(ns string) {
	By("deploying a TLS proxy in front of RustFS")
	manifest := fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: selfsigned
  namespace: %[1]s
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: rustfs-ca
  namespace: %[1]s
spec:
  isCA: true
  commonName: rustfs-ca
  secretName: rustfs-ca
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: selfsigned
    kind: Issuer
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: rustfs-ca
  namespace: %[1]s
spec:
  ca:
    secretName: rustfs-ca
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: rustfs-tls
  namespace: %[1]s
spec:
  secretName: rustfs-tls-cert
  commonName: rustfs-tls.%[1]s.svc.cluster.local
  dnsNames:
    - rustfs-tls.%[1]s.svc.cluster.local
  issuerRef:
    name: rustfs-ca
    kind: Issuer
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: rustfs-tls-proxy
  namespace: %[1]s
data:
  default.conf: |
    server {
      listen 8443 ssl;
      ssl_certificate /tls/tls.crt;
      ssl_certificate_key /tls/tls.key;
      client_max_body_size 0;
      proxy_request_buffering off;
      proxy_buffering off;
      location / {
        proxy_pass http://rustfs.%[1]s.svc.cluster.local:9000;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
      }
    }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: rustfs-tls
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: rustfs-tls
  template:
    metadata:
      labels:
        app: rustfs-tls
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 101
        runAsGroup: 101
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: nginx
          image: nginxinc/nginx-unprivileged:1.29-alpine
          ports:
            - containerPort: 8443
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: conf
              mountPath: /etc/nginx/conf.d
            - name: tls
              mountPath: /tls
      volumes:
        - name: conf
          configMap:
            name: rustfs-tls-proxy
        - name: tls
          secret:
            secretName: rustfs-tls-cert
---
apiVersion: v1
kind: Service
metadata:
  name: rustfs-tls
  namespace: %[1]s
spec:
  selector:
    app: rustfs-tls
  ports:
    - name: https
      port: 443
      targetPort: 8443
`, ns)
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = newStringReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy the RustFS TLS proxy")

	Eventually(func(g Gomega) {
		_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/rustfs-tls", "-n", ns, "--timeout=1m"))
		g.Expect(err).NotTo(HaveOccurred())
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
}
