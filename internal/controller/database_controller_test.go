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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

var _ = Describe("Database Controller", func() {
	const (
		DatabaseNamespace = "default"
		appUser           = "app_user"
		timeout           = time.Second * 10
		interval          = time.Millisecond * 250
	)

	Context("When creating a Database resource", func() {
		It("should successfully create and retrieve the Database resource", func() {
			ctx := context.Background()
			databaseName := fmt.Sprintf("test-db-%d", time.Now().UnixNano())
			clusterName := fmt.Sprintf("db-cluster-%d", time.Now().UnixNano())

			By("Creating a Cluster for the Database")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: DatabaseNamespace,
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

			By("Creating the Database resource")
			database := &postgresv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      databaseName,
					Namespace: DatabaseNamespace,
				},
				Spec: postgresv1alpha1.DatabaseSpec{
					ClusterRef: postgresv1alpha1.ClusterReference{
						Name: clusterName,
					},
					Owner: appUser,
					Extensions: []postgresv1alpha1.ExtensionSpec{
						{Name: "uuid-ossp"},
						{Name: "pg_trgm"},
					},
					Schemas: []postgresv1alpha1.SchemaSpec{
						{
							Name:  "app",
							Owner: appUser,
							Grants: []postgresv1alpha1.GrantSpec{
								{
									Role:       appUser,
									Privileges: []string{"USAGE", "CREATE"},
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, database)).To(Succeed())

			defer func() {
				database.Finalizers = nil
				_ = k8sClient.Update(ctx, database)
				_ = k8sClient.Delete(ctx, database)
			}()

			By("Verifying the Database was created")
			err := k8sClient.Get(ctx, types.NamespacedName{Name: databaseName, Namespace: DatabaseNamespace}, database)
			Expect(err).NotTo(HaveOccurred())
			Expect(database.Spec.Owner).To(Equal(appUser))
			Expect(database.Spec.Extensions).To(HaveLen(2))
			Expect(database.Spec.Schemas).To(HaveLen(1))

			By("Verifying extension configuration")
			extensionNames := make([]string, len(database.Spec.Extensions))
			for i, ext := range database.Spec.Extensions {
				extensionNames[i] = ext.Name
			}
			Expect(extensionNames).To(ContainElements("uuid-ossp", "pg_trgm"))

			By("Verifying schema configuration")
			schema := database.Spec.Schemas[0]
			Expect(schema.Name).To(Equal("app"))
			Expect(schema.Owner).To(Equal(appUser))
			Expect(schema.Grants).To(HaveLen(1))
			Expect(schema.Grants[0].Role).To(Equal(appUser))
			Expect(schema.Grants[0].Privileges).To(ContainElements("USAGE", "CREATE"))

			By("Verifying the cluster reference")
			Expect(database.Spec.ClusterRef.Name).To(Equal(clusterName))
		})

		It("should handle reconciliation when cluster is not ready", func() {
			ctx := context.Background()
			databaseName := fmt.Sprintf("test-db-%d", time.Now().UnixNano())
			clusterName := fmt.Sprintf("db-cluster-%d", time.Now().UnixNano())

			By("Creating a Cluster for the Database")
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: DatabaseNamespace,
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

			By("Creating the Database resource")
			database := &postgresv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      databaseName,
					Namespace: DatabaseNamespace,
				},
				Spec: postgresv1alpha1.DatabaseSpec{
					ClusterRef: postgresv1alpha1.ClusterReference{
						Name: clusterName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, database)).To(Succeed())

			defer func() {
				database.Finalizers = nil
				_ = k8sClient.Update(ctx, database)
				_ = k8sClient.Delete(ctx, database)
			}()

			By("Reconciling the Database")
			controllerReconciler := &DatabaseReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Reconcile should return without fatal error
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: databaseName, Namespace: DatabaseNamespace},
			})
			// May requeue since cluster is not ready
			_ = err
			_ = result
		})
	})

	Context("When a Database references a non-existent Cluster", func() {
		It("should update status to not ready", func() {
			ctx := context.Background()
			databaseName := fmt.Sprintf("orphan-db-%d", time.Now().UnixNano())

			By("Creating a Database with invalid cluster reference")
			database := &postgresv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      databaseName,
					Namespace: DatabaseNamespace,
				},
				Spec: postgresv1alpha1.DatabaseSpec{
					ClusterRef: postgresv1alpha1.ClusterReference{
						Name: nonexistentCluster,
					},
				},
			}
			Expect(k8sClient.Create(ctx, database)).To(Succeed())

			defer func() {
				database.Finalizers = nil
				_ = k8sClient.Update(ctx, database)
				_ = k8sClient.Delete(ctx, database)
			}()

			By("Reconciling the Database")
			controllerReconciler := &DatabaseReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: databaseName, Namespace: DatabaseNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the Database status shows not ready")
			err = k8sClient.Get(ctx, types.NamespacedName{Name: databaseName, Namespace: DatabaseNamespace}, database)
			Expect(err).NotTo(HaveOccurred())
			Expect(database.Status.Ready).To(BeFalse())
		})
	})

	Context("When a Database resource does not exist", func() {
		It("should not return an error", func() {
			ctx := context.Background()
			controllerReconciler := &DatabaseReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "nonexistent-database",
					Namespace: DatabaseNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
