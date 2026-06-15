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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ruckc/pgop/test/utils"
)

// rustfsNamespace is where RustFS (S3-compatible) is deployed for backup tests
const rustfsNamespace = "rustfs"

var _ = Describe("Backup", Ordered, func() {
	BeforeAll(func() {
		By("creating rustfs namespace")
		cmd := exec.Command("kubectl", "create", "ns", rustfsNamespace)
		_, _ = utils.Run(cmd) // ignore error if namespace already exists

		By("deploying RustFS as S3-compatible storage")
		deployRustFS()

		By("waiting for RustFS to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "rollout", "status", "deployment/rustfs", "-n", rustfsNamespace, "--timeout=2m")
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("creating RustFS S3 bucket for backups")
		createRustFSBucket()
	})

	AfterAll(func() {
		By("cleaning up backup resources")
		cmd := exec.Command("kubectl", "delete", "backup.pgop.ruck.io", "myapp-backup", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("removing rustfs namespace")
		cmd = exec.Command("kubectl", "delete", "ns", rustfsNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(5 * time.Second)

	Context("Logical Backup via pg_dump", func() {
		It("should create CronJobs for schema and data backups", func() {
			By("creating RustFS credentials secret in pgop-system namespace")
			cmd := exec.Command("kubectl", "create", "secret", "generic", "rustfs-credentials",
				"--from-literal=AWS_ACCESS_KEY_ID=minioadmin",
				"--from-literal=AWS_SECRET_ACCESS_KEY=minioadmin",
				"-n", namespace,
				"--dry-run=client", "-o", "yaml",
			)
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			applyCmd := exec.Command("kubectl", "apply", "-f", "-")
			applyCmd.Stdin = stringReader(out)
			_, err = utils.Run(applyCmd)
			Expect(err).NotTo(HaveOccurred())

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

			applyCmd = exec.Command("kubectl", "apply", "-f", "-")
			applyCmd.Stdin = stringReader(backupYAML)
			_, err = utils.Run(applyCmd)
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
			cmd = exec.Command("kubectl", "create", "job", "schema-backup-manual",
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
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "run", "check-backup", "--restart=Never",
					"-n", rustfsNamespace,
					"--image=amazon/aws-cli:2.27.46",
					"--env=AWS_ACCESS_KEY_ID=minioadmin",
					"--env=AWS_SECRET_ACCESS_KEY=minioadmin",
					"--env=AWS_DEFAULT_REGION=us-east-1",
					"--env=AWS_ENDPOINT_URL=http://rustfs:9000",
					"--command", "--",
					"aws", "s3", "ls", "s3://pgop-backups/myapp/",
				)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty(), "no backup files found in RustFS")
				// clean up the check pod
				_ = exec.Command("kubectl", "delete", "pod", "check-backup", "-n", rustfsNamespace, "--ignore-not-found").Run()
			}, 2*time.Minute, 15*time.Second).Should(Succeed())
		})
	})
})

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
            readOnlyRootFilesystem: false
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
	cmd.Stdin = stringReader(rustfsYAML)
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
            - |
              aws s3 mb s3://pgop-backups --region us-east-1
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
	cmd.Stdin = stringReader(bucketJobYAML)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create bucket job")

	// Wait for bucket creation job to complete
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "job", "create-bucket", "-n", rustfsNamespace,
			"-o", "jsonpath={.status.succeeded}")
		out, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(Equal("1"), "bucket creation job did not succeed")
	}, 3*time.Minute, 5*time.Second).Should(Succeed())
}

// stringReader creates an *strings.Reader that implements io.Reader for use as cmd.Stdin.
func stringReader(s string) *stringReaderImpl {
	return &stringReaderImpl{data: s, pos: 0}
}

type stringReaderImpl struct {
	data string
	pos  int
}

func (r *stringReaderImpl) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
