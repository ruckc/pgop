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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ruckc/pgop/test/utils"
)

// namespace where the project is deployed in
const namespace = "pgop-system"

// serviceAccountName created for the project
const serviceAccountName = "pgop-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "pgop-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "pgop-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("patching controller for coverage")
		// Determine correct path for coverage patch
		var coveragePatch string
		if _, err := os.Stat("../../config/manager/manager_coverage_patch.yaml"); err == nil {
			coveragePatch = "../../config/manager/manager_coverage_patch.yaml"
		} else {
			coveragePatch = "config/manager/manager_coverage_patch.yaml"
		}
		absCoveragePatch, _ := filepath.Abs(coveragePatch)
		cmd = exec.Command("kubectl", "patch", "deployment", "pgop-controller-manager", "-n", namespace, "--patch-file", absCoveragePatch)
		_, err = utils.Run(cmd)
		// We expect this might fail if the deployment isn't ready or named differently, but standardized names should work.
		// However, it's better to apply this patch via Kustomize in the makefile or just kubectl patch here.
		// Since 'make deploy' runs 'kustomize build', we should arguably add the patch to the kustomize build.
		// BUT, 'make deploy' is a single command. simpler is to patch the deployment immediately after applied.
		Expect(err).NotTo(HaveOccurred(), "Failed to patch controller for coverage")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		if controllerPodName != "" {
			By("dumping coverage data")
			// Create local coverage directory
			if err := os.MkdirAll("coverage-dump", 0755); err != nil {
				fmt.Fprintf(GinkgoWriter, "Failed to create coverage directory: %s\n", err)
			} else {
				// Copy coverage directory from pod
				// We need to stop the manager to flush coverage data (covcounters)
				// Since we use emptyDir, we must restart the container, not the pod.
				// Sending SIGTERM to PID 1 (manager) should cause it to exit and flush.
				By("triggering manager shutdown to flush coverage")
				termCmd := exec.Command("kubectl", "exec", "-n", namespace, controllerPodName, "-c", "manager", "--", "/bin/sh", "-c", "kill -TERM 1")
				output, err := utils.Run(termCmd)
				if err != nil {
					fmt.Fprintf(GinkgoWriter, "Failed to send SIGTERM to manager: %s\nOutput: %s\n", err, output)
				}

				// Wait for covcounters file to appear
				timeout := 30 * time.Second
				start := time.Now()
				found := false
				for time.Since(start) < timeout {
					cmd := exec.Command("kubectl", "exec", "-n", namespace, controllerPodName, "-c", "manager", "--", "/bin/ls", "/tmp/coverage")
					output, err := utils.Run(cmd)
					if err == nil && strings.Contains(output, "covcounters") {
						found = true
						break
					}
					time.Sleep(2 * time.Second)
				}

				if !found {
					fmt.Fprintf(GinkgoWriter, "Timed out waiting for covcounters file\n")
				}

				cmd := exec.Command("kubectl", "exec", "-n", namespace, controllerPodName, "-c", "manager", "--", "/bin/ls", "/tmp/coverage")
				output, err = utils.Run(cmd)
				if err != nil {
					fmt.Fprintf(GinkgoWriter, "Failed to list coverage files: %s\nOutput: %s\n", err, output)
				} else {
					files := strings.Split(strings.TrimSpace(string(output)), "\n")
					for _, file := range files {
						if file == "" {
							continue
						}
						By(fmt.Sprintf("copying coverage file %s", file))
						// Use base64 to copy file content safely (avoiding binary corruption)
						catCmd := exec.Command("kubectl", "exec", "-n", namespace, controllerPodName, "-c", "manager", "--", "base64", fmt.Sprintf("/tmp/coverage/%s", file))
						contentBase64, err := utils.Run(catCmd)
						if err != nil {
							fmt.Fprintf(GinkgoWriter, "Failed to base64 coverage file %s: %s\n", file, err)
							continue
						}
						// base64 output might contain n newlines, remove them
						contentBase64 = strings.ReplaceAll(strings.TrimSpace(contentBase64), "\n", "")
						contentBase64 = strings.ReplaceAll(contentBase64, "\r", "")
						
						content, err := base64.StdEncoding.DecodeString(contentBase64)
						if err != nil {
							fmt.Fprintf(GinkgoWriter, "Failed to decode base64 content for %s: %s\n", file, err)
							continue
						}

						if err := os.WriteFile(filepath.Join("coverage-dump", file), content, 0644); err != nil {
							fmt.Fprintf(GinkgoWriter, "Failed to write local file %s: %s\n", file, err)
						}
					}

					// Process coverage data locally
					cmd = exec.Command("go", "tool", "covdata", "textfmt", "-i=coverage-dump", "-o=../../e2e-cover.out")
					output, err = utils.Run(cmd)
					if err != nil {
						fmt.Fprintf(GinkgoWriter, "Failed to process coverage data: %s\nOutput: %s\n", err, output)
					}
				}
			}
		}

		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("removing custom resources to avoid finalizer deadlock")
		// Clean up Database
		cmd = exec.Command("kubectl", "delete", "database.pgop.ruck.io", "myapp", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		// Clean up Role
		cmd = exec.Command("kubectl", "delete", "role.pgop.ruck.io", "app-user", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		// Clean up Cluster
		cmd = exec.Command("kubectl", "delete", "cluster.postgres.ruck.io", "example-cluster", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("cleaning up the ClusterRoleBinding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}

			By("Fetching PostgreSQL pod logs")
			cmd = exec.Command("kubectl", "logs", "-l", "app.kubernetes.io/name=postgresql", "-n", namespace, "--all-containers=true")
			pgLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "PostgreSQL logs:\n %s", pgLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get PostgreSQL logs: %s", err)
			}
			
			By("Describing PostgreSQL pods")
			cmd = exec.Command("kubectl", "describe", "pods", "-l", "app.kubernetes.io/name=postgresql", "-n", namespace)
			pgDescribe, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "PostgreSQL pods description:\n %s", pgDescribe)
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=pgop-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})
	Context("Custom Resources", func() {
		var (
			clusterSample  string
			roleSample     string
			databaseSample string
		)

		BeforeEach(func() {
			// Determine the correct path to samples
			if _, err := os.Stat("../../config/samples/postgres_v1alpha1_cluster.yaml"); err == nil {
				clusterSample = "../../config/samples/postgres_v1alpha1_cluster.yaml"
				roleSample = "../../config/samples/postgres_v1alpha1_role.yaml"
				databaseSample = "../../config/samples/postgres_v1alpha1_database.yaml"
			} else {
				clusterSample = "config/samples/postgres_v1alpha1_cluster.yaml"
				roleSample = "config/samples/postgres_v1alpha1_role.yaml"
				databaseSample = "config/samples/postgres_v1alpha1_database.yaml"
			}
		})

		It("should Create Cluster, Role and Database successfully", func() {
			absClusterSample, _ := filepath.Abs(clusterSample)
			absRoleSample, _ := filepath.Abs(roleSample)
			absDatabaseSample, _ := filepath.Abs(databaseSample)

			By("Applying the Cluster sample")
			cmd := exec.Command("kubectl", "apply", "-f", absClusterSample, "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Cluster sample")

			By("Verifying the Cluster becomes Ready")
			verifyClusterReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cluster", "example-cluster", "-n", namespace,
					"-o", "jsonpath={.status.ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("true"), "Cluster not ready")
			}
			Eventually(verifyClusterReady, 5*time.Minute, time.Second).Should(Succeed())

			By("Verifying the StatefulSet is created and ready")
			verifyStatefulSet := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", "example-cluster", "-n", namespace,
					"-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"), "StatefulSet not ready")
			}
			Eventually(verifyStatefulSet, 5*time.Minute, time.Second).Should(Succeed())

			By("Verifying the Service is created")
			cmd = exec.Command("kubectl", "get", "service", "example-cluster", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Service not found")

			By("Verifying the Superuser Secret is created")
			cmd = exec.Command("kubectl", "get", "secret", "example-cluster-credentials", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Superuser secret not found")

			By("Applying the Role sample")
			cmd = exec.Command("kubectl", "apply", "-f", absRoleSample, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Role sample")

			By("Verifying the Role becomes Ready")
			verifyRoleReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "role.pgop.ruck.io", "app-user", "-n", namespace,
					"-o", "jsonpath={.status.ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("true"), "Role not ready")
			}
			Eventually(verifyRoleReady, 2*time.Minute, time.Second).Should(Succeed())

			By("Verifying the Role Secret is created")
			cmd = exec.Command("kubectl", "get", "secret", "example-cluster-app-user-credentials", "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Role secret not found")

			By("Applying the Database sample")
			cmd = exec.Command("kubectl", "apply", "-f", absDatabaseSample, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply Database sample")

			By("Verifying the Database becomes Ready")
			verifyDatabaseReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "database.pgop.ruck.io", "myapp", "-n", namespace,
					"-o", "jsonpath={.status.ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("true"), "Database not ready")
			}
			Eventually(verifyDatabaseReady, 2*time.Minute, time.Second).Should(Succeed())
		})
	})

	RegisterBackupTests()
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
