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
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ruckc/pgop/test/utils"
)

// rustfsNamespace is where RustFS (S3-compatible) is deployed for backup tests
const rustfsNamespace = "rustfs"

// RegisterBackupTests adds backup test specs into the caller's Describe/Context block.
// Must be called inside a Describe/Context that has already deployed the controller into `namespace`.
func RegisterBackupTests() {
	Context("Backup — logical pg_dump via RustFS", Ordered, func() {
		BeforeAll(func() {
			By("creating rustfs namespace")
			cmd := exec.Command("kubectl", "create", "ns", rustfsNamespace)
			_, _ = utils.Run(cmd) // ignore error if already exists

			By("deploying RustFS as S3-compatible storage")
			deployRustFS()

			By("waiting for RustFS to be ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "rollout", "status", "deployment/rustfs",
					"-n", rustfsNamespace, "--timeout=2m")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("creating S3 bucket in RustFS")
			createRustFSBucket()

			By("creating RustFS credentials secret in manager namespace")
			cmd = exec.Command("kubectl", "create", "secret", "generic", "rustfs-credentials",
				"--from-literal=AWS_ACCESS_KEY_ID=minioadmin",
				"--from-literal=AWS_SECRET_ACCESS_KEY=minioadmin",
				"-n", namespace,
				"--dry-run=client", "-o", "yaml",
			)
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			applyCmd := exec.Command("kubectl", "apply", "-f", "-")
			applyCmd.Stdin = newStringReader(out)
			_, err = utils.Run(applyCmd)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("cleaning up Restore and BackupRun CRs")
			_, _ = utils.Run(exec.Command("kubectl", "delete", "restore.pgop.ruck.io", "myapp-restore",
				"-n", namespace, "--ignore-not-found"))
			_, _ = utils.Run(exec.Command("kubectl", "delete", "backuprun.pgop.ruck.io", "myapp-restore-src",
				"-n", namespace, "--ignore-not-found"))

			By("cleaning up Backup CR")
			cmd := exec.Command("kubectl", "delete", "backup.pgop.ruck.io", "myapp-backup",
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)

			By("removing rustfs namespace")
			cmd = exec.Command("kubectl", "delete", "ns", rustfsNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		SetDefaultEventuallyTimeout(5 * time.Minute)
		SetDefaultEventuallyPollingInterval(5 * time.Second)

		It("should create CronJobs and successfully upload backup artifacts to S3", func() {
			By("applying the Backup CR")
			backupYAML := fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Backup
metadata:
  name: myapp-backup
  namespace: %s
spec:
  type: logical
  databaseRef:
    name: myapp
  schedule: "*/5 * * * *"
  retention:
    disabled: true
  backupRunTTL: "1h"
  destination:
    type: s3
    s3:
      bucket: pgop-backups
      prefix: myapp
      region: us-east-1
      endpoint: http://rustfs.%s.svc.cluster.local:9000
      credentialsSecretRef:
        name: rustfs-credentials
`, namespace, rustfsNamespace)

			applyCmd := exec.Command("kubectl", "apply", "-f", "-")
			applyCmd.Stdin = newStringReader(backupYAML)
			_, err := utils.Run(applyCmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Backup CR")

			By("verifying schema CronJob is created")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", "myapp-backup-schema", "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}).Should(Succeed())

			By("verifying data CronJob is created")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", "myapp-backup-data", "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}).Should(Succeed())

			By("triggering a manual schema backup job")
			cmd := exec.Command("kubectl", "create", "job", "schema-backup-manual",
				"--from=cronjob/myapp-backup-schema", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for schema backup job to succeed")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "job", "schema-backup-manual",
					"-n", namespace,
					"-o", "jsonpath={.status.succeeded}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("1"), "schema backup job did not succeed")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("triggering a manual data backup job")
			cmd = exec.Command("kubectl", "create", "job", "data-backup-manual",
				"--from=cronjob/myapp-backup-data", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for data backup job to succeed")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "job", "data-backup-manual",
					"-n", namespace,
					"-o", "jsonpath={.status.succeeded}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("1"), "data backup job did not succeed")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying backup artifacts exist in RustFS")
			// Clean up any prior check pod first
			_ = exec.Command("kubectl", "delete", "pod", "check-backup",
				"-n", rustfsNamespace, "--ignore-not-found").Run()

			checkYAML := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: check-backup
  namespace: %s
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    runAsGroup: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: check
      image: amazon/aws-cli:2.27.46
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
      command: ["/bin/sh", "-c", "aws s3 ls s3://pgop-backups/myapp/ && echo 'found'"]
      env:
        - name: AWS_ACCESS_KEY_ID
          value: minioadmin
        - name: AWS_SECRET_ACCESS_KEY
          value: minioadmin
        - name: AWS_DEFAULT_REGION
          value: us-east-1
        - name: AWS_ENDPOINT_URL
          value: http://rustfs:9000
`, rustfsNamespace)

			applyCmd = exec.Command("kubectl", "apply", "-f", "-")
			applyCmd.Stdin = newStringReader(checkYAML)
			_, err = utils.Run(applyCmd)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", "check-backup",
					"-n", rustfsNamespace, "-o", "jsonpath={.status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("Succeeded"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			// Verify the pod logged 'found'
			cmd = exec.Command("kubectl", "logs", "check-backup", "-n", rustfsNamespace)
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("found"), "backup artifacts not found in RustFS")

			By("capturing the uploaded data artifact location from the backup job logs")
			cmd = exec.Command("kubectl", "logs", "job/data-backup-manual", "-n", namespace, "-c", "s3-upload")
			logs, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			location := parseUploadedLocation(logs)
			Expect(location).To(HavePrefix("s3://pgop-backups/myapp/"), "could not parse uploaded artifact location from logs")

			By("recording a BackupRun pointing at the uploaded artifact")
			backupRunYAML := fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: BackupRun
metadata:
  name: myapp-restore-src
  namespace: %s
spec:
  backupRef:
    name: myapp-backup
  type: data
`, namespace)
			applyCmd = exec.Command("kubectl", "apply", "-f", "-")
			applyCmd.Stdin = newStringReader(backupRunYAML)
			_, err = utils.Run(applyCmd)
			Expect(err).NotTo(HaveOccurred())

			// The artifact location lives on status, so patch the status subresource.
			cmd = exec.Command("kubectl", "patch", "backuprun.pgop.ruck.io", "myapp-restore-src",
				"-n", namespace, "--subresource=status", "--type=merge",
				"-p", fmt.Sprintf(`{"status":{"location":%q}}`, location))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("applying a logical Restore into the myapp database")
			restoreYAML := fmt.Sprintf(`
apiVersion: pgop.ruck.io/v1alpha1
kind: Restore
metadata:
  name: myapp-restore
  namespace: %s
spec:
  type: logical
  backupRunRef:
    name: myapp-restore-src
  clusterRef:
    name: example-cluster
  databaseRef:
    name: myapp
`, namespace)
			applyCmd = exec.Command("kubectl", "apply", "-f", "-")
			applyCmd.Stdin = newStringReader(restoreYAML)
			_, err = utils.Run(applyCmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the Restore to reach Succeeded")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "restore.pgop.ruck.io", "myapp-restore",
					"-n", namespace, "-o", "jsonpath={.status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("Succeeded"), "restore did not succeed")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())
		})
	})
}

// parseUploadedLocation extracts the "Uploaded to <s3-url>" line emitted by the
// backup upload script.
func parseUploadedLocation(logs string) string {
	for _, line := range strings.Split(logs, "\n") {
		const marker = "Uploaded to "
		if idx := strings.Index(line, marker); idx >= 0 {
			return strings.TrimSpace(line[idx+len(marker):])
		}
	}
	return ""
}

// deployRustFS deploys RustFS (S3-compatible object storage) into the rustfs namespace.
func deployRustFS() {
	rustfsYAML := fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata:
  name: rustfs
  namespace: %s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: rustfs
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: rustfs
  template:
    metadata:
      labels:
        app: rustfs
    spec:
      serviceAccountName: rustfs
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: rustfs
          image: rustfs/rustfs:latest
          args:
            - server
            - /data
            - --console-address
            - ":9001"
          ports:
            - containerPort: 9000
              name: s3
            - containerPort: 9001
              name: console
          env:
            - name: RUSTFS_ROOT_USER
              value: minioadmin
            - name: RUSTFS_ROOT_PASSWORD
              value: minioadmin
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: rustfs
  namespace: %s
spec:
  selector:
    app: rustfs
  ports:
    - name: s3
      port: 9000
      targetPort: 9000
`, rustfsNamespace, rustfsNamespace, rustfsNamespace)

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = newStringReader(rustfsYAML)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy RustFS")
}

// createRustFSBucket creates the pgop-backups bucket in RustFS using a Job.
func createRustFSBucket() {
	bucketJobYAML := fmt.Sprintf(`
apiVersion: batch/v1
kind: Job
metadata:
  name: create-bucket
  namespace: %s
spec:
  template:
    spec:
      restartPolicy: OnFailure
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: create-bucket
          image: amazon/aws-cli:2.27.46
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          command:
            - /bin/sh
            - -c
            - aws s3 mb s3://pgop-backups --region us-east-1
          env:
            - name: AWS_ACCESS_KEY_ID
              value: minioadmin
            - name: AWS_SECRET_ACCESS_KEY
              value: minioadmin
            - name: AWS_DEFAULT_REGION
              value: us-east-1
            - name: AWS_ENDPOINT_URL
              value: http://rustfs:9000
`, rustfsNamespace)

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = newStringReader(bucketJobYAML)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create bucket job")

	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "job", "create-bucket", "-n", rustfsNamespace,
			"-o", "jsonpath={.status.succeeded}")
		out, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(Equal("1"), "bucket creation job did not succeed")
	}, 3*time.Minute, 5*time.Second).Should(Succeed())
}

type stringReaderImpl struct {
	data string
	pos  int
}

func newStringReader(s string) *stringReaderImpl {
	return &stringReaderImpl{data: s}
}

func (r *stringReaderImpl) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
