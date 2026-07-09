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

package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

var _ = Describe("Cluster Controller", func() {
	const (
		ClusterNamespace = "default"
		timeout          = time.Second * 10
		interval         = time.Millisecond * 250
	)

	Context("When reconciling a Cluster resource", func() {
		It("should create a credentials Secret with all required keys", func() {
			ctx := context.Background()
			clusterName := fmt.Sprintf("test-cluster-%d", time.Now().UnixNano())

			By("Creating the Cluster resource")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: ClusterNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{
					Image:    DefaultPostgresImage,
					Replicas: 1,
					Port:     5432,
					Storage: postgresv1alpha1.StorageSpec{
						Size: "1Gi",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			defer func() {
				cluster.Finalizers = nil
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}()

			By("Reconciling the Cluster")
			controllerReconciler := &ClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking that the Secret was created")
			secret := &corev1.Secret{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      clusterName + "-credentials",
				Namespace: ClusterNamespace,
			}, secret)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Secret contents")
			Expect(secret.Data).To(HaveKey("username"))
			Expect(secret.Data).To(HaveKey("password"))
			Expect(secret.Data).To(HaveKey("host"))
			Expect(secret.Data).To(HaveKey("port"))
			Expect(secret.Data).To(HaveKey("database"))
			Expect(string(secret.Data["username"])).To(Equal("pgop_operator"))
			Expect(string(secret.Data["password"])).NotTo(BeEmpty())
			Expect(string(secret.Data["database"])).To(Equal("postgres"))
		})

		It("should create a Service with correct port", func() {
			ctx := context.Background()
			clusterName := fmt.Sprintf("test-cluster-%d", time.Now().UnixNano())

			By("Creating the Cluster resource")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: ClusterNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{
					Image:    DefaultPostgresImage,
					Replicas: 1,
					Port:     5432,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			defer func() {
				cluster.Finalizers = nil
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}()

			By("Reconciling the Cluster")
			controllerReconciler := &ClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking that the Service was created")
			service := &corev1.Service{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      clusterName,
				Namespace: ClusterNamespace,
			}, service)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Service spec")
			Expect(service.Spec.Ports).To(HaveLen(1))
			Expect(service.Spec.Ports[0].Port).To(Equal(int32(5432)))
			Expect(service.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/instance", clusterName))
		})

		It("should create a StatefulSet with correct image", func() {
			ctx := context.Background()
			clusterName := fmt.Sprintf("test-cluster-%d", time.Now().UnixNano())

			By("Creating the Cluster resource")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: ClusterNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{
					Image:    DefaultPostgresImage,
					Replicas: 1,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			defer func() {
				cluster.Finalizers = nil
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}()

			By("Reconciling the Cluster")
			controllerReconciler := &ClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking that the StatefulSet was created")
			sts := &appsv1.StatefulSet{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      clusterName,
				Namespace: ClusterNamespace,
			}, sts)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying StatefulSet spec")
			Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
			Expect(sts.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal(DefaultPostgresImage))
		})

		It("should set a password-sync postStart lifecycle hook on the container", func() {
			ctx := context.Background()
			clusterName := fmt.Sprintf("test-cluster-%d", time.Now().UnixNano())

			By("Creating the Cluster resource")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: ClusterNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{
					Image: DefaultPostgresImage,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			defer func() {
				cluster.Finalizers = nil
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}()

			controllerReconciler := &ClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the postStart hook converges the operator password")
			sts := &appsv1.StatefulSet{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace}, sts)
			Expect(err).NotTo(HaveOccurred())
			lifecycle := sts.Spec.Template.Spec.Containers[0].Lifecycle
			Expect(lifecycle).NotTo(BeNil())
			Expect(lifecycle.PostStart).NotTo(BeNil())
			Expect(lifecycle.PostStart.Exec.Command).To(ContainElement(ContainSubstring("ALTER ROLE")))
		})

		It("should backfill the lifecycle hook on a StatefulSet that lacks it", func() {
			ctx := context.Background()
			clusterName := fmt.Sprintf("test-cluster-%d", time.Now().UnixNano())

			By("Creating the Cluster resource")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: ClusterNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			defer func() {
				cluster.Finalizers = nil
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}()

			controllerReconciler := &ClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Simulating a pre-fix StatefulSet by clearing the lifecycle hook")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace}, sts)).To(Succeed())
			sts.Spec.Template.Spec.Containers[0].Lifecycle = nil
			Expect(k8sClient.Update(ctx, sts)).To(Succeed())

			By("Reconciling again")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the lifecycle hook was backfilled")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace}, sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.Containers[0].Lifecycle).NotTo(BeNil())
		})

		It("should update status after reconciliation", func() {
			ctx := context.Background()
			clusterName := fmt.Sprintf("test-cluster-%d", time.Now().UnixNano())

			By("Creating the Cluster resource")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: ClusterNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{
					Image: DefaultPostgresImage,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			defer func() {
				cluster.Finalizers = nil
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}()

			By("Reconciling the Cluster")
			controllerReconciler := &ClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the Cluster status")
			err = k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace}, cluster)
			Expect(err).NotTo(HaveOccurred())

			Expect(cluster.Status.SecretName).To(Equal(clusterName + "-credentials"))
			Expect(cluster.Status.Endpoint).To(ContainSubstring(clusterName))
			Expect(cluster.Status.Conditions).To(HaveLen(1))
		})

		It("should add a finalizer", func() {
			ctx := context.Background()
			clusterName := fmt.Sprintf("test-cluster-%d", time.Now().UnixNano())

			By("Creating the Cluster resource")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: ClusterNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{
					Image: DefaultPostgresImage,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			defer func() {
				cluster.Finalizers = nil
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}()

			By("Reconciling the Cluster")
			controllerReconciler := &ClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the finalizer was added")
			err = k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace}, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(cluster.Finalizers).To(ContainElement("pgop.ruck.io/cluster-finalizer"))
		})

		It("should not recreate existing resources on second reconcile", func() {
			ctx := context.Background()
			clusterName := fmt.Sprintf("test-cluster-%d", time.Now().UnixNano())

			By("Creating the Cluster resource")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: ClusterNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{
					Image: DefaultPostgresImage,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			defer func() {
				cluster.Finalizers = nil
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}()

			controllerReconciler := &ClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("First reconcile")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Get the secret to check its password
			secret := &corev1.Secret{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      clusterName + "-credentials",
				Namespace: ClusterNamespace,
			}, secret)
			Expect(err).NotTo(HaveOccurred())
			originalPassword := string(secret.Data["password"])

			By("Second reconcile")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Password should be the same (secret not recreated)
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      clusterName + "-credentials",
				Namespace: ClusterNamespace,
			}, secret)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(secret.Data["password"])).To(Equal(originalPassword))
		})

		It("should use default values when spec is minimal", func() {
			ctx := context.Background()
			clusterName := fmt.Sprintf("test-cluster-%d", time.Now().UnixNano())

			By("Creating a minimal Cluster resource")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: ClusterNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			defer func() {
				cluster.Finalizers = nil
				_ = k8sClient.Update(ctx, cluster)
				_ = k8sClient.Delete(ctx, cluster)
			}()

			By("Reconciling the Cluster")
			controllerReconciler := &ClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying default image is used")
			sts := &appsv1.StatefulSet{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      clusterName,
				Namespace: ClusterNamespace,
			}, sts)
			Expect(err).NotTo(HaveOccurred())
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal(DefaultPostgresImage))
		})

		It("should handle cluster deletion", func() {
			ctx := context.Background()
			clusterName := fmt.Sprintf("test-cluster-%d", time.Now().UnixNano())

			By("Creating the Cluster resource")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: ClusterNamespace,
				},
				Spec: postgresv1alpha1.ClusterSpec{
					Image: DefaultPostgresImage,
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			controllerReconciler := &ClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("First reconcile to add finalizer")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Deleting the cluster")
			err = k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace}, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())

			By("Reconciling after deletion")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: clusterName, Namespace: ClusterNamespace},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When a Cluster resource does not exist", func() {
		It("should not return an error", func() {
			ctx := context.Background()
			controllerReconciler := &ClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      nonexistentCluster,
					Namespace: ClusterNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

var _ = Describe("Cluster Password Generation", func() {
	It("should generate a random password of correct length", func() {
		password1, err := generatePassword(32)
		Expect(err).NotTo(HaveOccurred())
		Expect(password1).To(HaveLen(32))

		password2, err := generatePassword(32)
		Expect(err).NotTo(HaveOccurred())
		Expect(password2).To(HaveLen(32))

		// Passwords should be different
		Expect(password1).NotTo(Equal(password2))
	})

	It("should generate passwords of various lengths", func() {
		password16, err := generatePassword(16)
		Expect(err).NotTo(HaveOccurred())
		Expect(password16).To(HaveLen(16))

		password64, err := generatePassword(64)
		Expect(err).NotTo(HaveOccurred())
		Expect(password64).To(HaveLen(64))
	})
})
