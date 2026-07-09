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
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

var _ = Describe("Restore Controller", func() {
	const RestoreNamespace = "default"

	var (
		ctx        context.Context
		reconciler *RestoreReconciler
		suffix     string
	)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &RestoreReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	// newBackupRun creates a Backup + BackupRun pair and sets the run's artifact
	// location on status. Returns the BackupRun name.
	newBackupRun := func(name string, withLocation bool) string {
		backupName := "backup-" + name
		backup := &postgresv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: backupName, Namespace: RestoreNamespace},
			Spec: postgresv1alpha1.BackupSpec{
				Type: postgresv1alpha1.BackupTypeLogical,
				Destination: postgresv1alpha1.DestinationSpec{
					Type: postgresv1alpha1.DestinationTypeS3,
					S3: &postgresv1alpha1.S3Destination{
						Bucket:   "my-bucket",
						Region:   "us-east-1",
						Endpoint: "https://minio.example.com",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())

		runName := "run-" + name
		run := &postgresv1alpha1.BackupRun{
			ObjectMeta: metav1.ObjectMeta{Name: runName, Namespace: RestoreNamespace},
			Spec: postgresv1alpha1.BackupRunSpec{
				BackupRef: postgresv1alpha1.ClusterReference{Name: backupName},
				Type:      postgresv1alpha1.BackupRunTypeData,
			},
		}
		Expect(k8sClient.Create(ctx, run)).To(Succeed())
		if withLocation {
			run.Status.Location = "s3://my-bucket/backup/20260101T020000.dump"
			Expect(k8sClient.Status().Update(ctx, run)).To(Succeed())
		}
		return runName
	}

	newCluster := func(name string) {
		cluster := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: RestoreNamespace},
			Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	}

	newDatabase := func(name, cluster string) {
		db := &postgresv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: RestoreNamespace},
			Spec: postgresv1alpha1.DatabaseSpec{
				ClusterRef: postgresv1alpha1.ClusterReference{Name: cluster},
				Owner:      "app-user",
			},
		}
		Expect(k8sClient.Create(ctx, db)).To(Succeed())
	}

	It("should create a logical restore Job and mark the Restore Running", func() {
		runName := newBackupRun(suffix, true)
		clusterName := "cluster-" + suffix
		dbName := "db-" + suffix
		newCluster(clusterName)
		newDatabase(dbName, clusterName)

		restore := &postgresv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: "restore-" + suffix, Namespace: RestoreNamespace},
			Spec: postgresv1alpha1.RestoreSpec{
				Type:         postgresv1alpha1.BackupTypeLogical,
				BackupRunRef: postgresv1alpha1.ClusterReference{Name: runName},
				ClusterRef:   postgresv1alpha1.ClusterReference{Name: clusterName},
				DatabaseRef:  &postgresv1alpha1.ClusterReference{Name: dbName},
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: restore.Name, Namespace: RestoreNamespace},
		})
		Expect(err).NotTo(HaveOccurred())

		By("Verifying status")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restore.Name, Namespace: RestoreNamespace}, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(postgresv1alpha1.RestorePhaseRunning))
		Expect(restore.Status.JobName).To(Equal(restore.Name + "-restore"))

		By("Verifying the Job")
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restore.Status.JobName, Namespace: RestoreNamespace}, job)).To(Succeed())
		Expect(job.Spec.Template.Spec.InitContainers).To(HaveLen(1))
		Expect(job.Spec.Template.Spec.InitContainers[0].Command[2]).To(ContainSubstring("aws s3 cp"))
		Expect(job.Spec.Template.Spec.InitContainers[0].Command[2]).To(ContainSubstring("--endpoint-url https://minio.example.com"))
		Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(job.Spec.Template.Spec.Containers[0].Image).To(Equal(DefaultPostgresImage))
		Expect(job.Spec.Template.Spec.Containers[0].Command[2]).To(ContainSubstring("pg_restore"))
		Expect(job.Spec.Template.Spec.Containers[0].Command[2]).To(ContainSubstring(dbName))
	})

	It("should fail a logical restore that omits databaseRef", func() {
		runName := newBackupRun(suffix, true)
		clusterName := "cluster-" + suffix
		newCluster(clusterName)

		restore := &postgresv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: "restore-" + suffix, Namespace: RestoreNamespace},
			Spec: postgresv1alpha1.RestoreSpec{
				Type:         postgresv1alpha1.BackupTypeLogical,
				BackupRunRef: postgresv1alpha1.ClusterReference{Name: runName},
				ClusterRef:   postgresv1alpha1.ClusterReference{Name: clusterName},
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: restore.Name, Namespace: RestoreNamespace},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restore.Name, Namespace: RestoreNamespace}, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(postgresv1alpha1.RestorePhaseFailed))
	})

	It("should create a physical restore Job with a PITR target", func() {
		runName := newBackupRun(suffix, true)
		clusterName := "cluster-" + suffix
		newCluster(clusterName)

		targetTime := metav1.NewTime(time.Unix(1767240000, 0).UTC())
		restore := &postgresv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: "restore-" + suffix, Namespace: RestoreNamespace},
			Spec: postgresv1alpha1.RestoreSpec{
				Type:         postgresv1alpha1.BackupTypePhysical,
				BackupRunRef: postgresv1alpha1.ClusterReference{Name: runName},
				ClusterRef:   postgresv1alpha1.ClusterReference{Name: clusterName},
				TargetTime:   &targetTime,
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: restore.Name, Namespace: RestoreNamespace},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restore.Name, Namespace: RestoreNamespace}, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(postgresv1alpha1.RestorePhaseRunning))

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restore.Status.JobName, Namespace: RestoreNamespace}, job)).To(Succeed())
		Expect(job.Spec.Template.Spec.Containers[0].Image).To(Equal(pgbackrestImage))
		cmd := job.Spec.Template.Spec.Containers[0].Command[2]
		Expect(cmd).To(ContainSubstring("pgbackrest"))
		Expect(cmd).To(ContainSubstring("restore"))
		Expect(cmd).To(ContainSubstring("--type=time"))
	})

	It("should mark the Restore Succeeded when its Job succeeds", func() {
		runName := newBackupRun(suffix, true)
		clusterName := "cluster-" + suffix
		dbName := "db-" + suffix
		newCluster(clusterName)
		newDatabase(dbName, clusterName)

		restore := &postgresv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: "restore-" + suffix, Namespace: RestoreNamespace},
			Spec: postgresv1alpha1.RestoreSpec{
				Type:         postgresv1alpha1.BackupTypeLogical,
				BackupRunRef: postgresv1alpha1.ClusterReference{Name: runName},
				ClusterRef:   postgresv1alpha1.ClusterReference{Name: clusterName},
				DatabaseRef:  &postgresv1alpha1.ClusterReference{Name: dbName},
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: restore.Name, Namespace: RestoreNamespace}}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restore.Name, Namespace: RestoreNamespace}, restore)).To(Succeed())
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restore.Status.JobName, Namespace: RestoreNamespace}, job)).To(Succeed())

		By("Marking the Job succeeded")
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restore.Name, Namespace: RestoreNamespace}, restore)).To(Succeed())
		Expect(restore.Status.Phase).To(Equal(postgresv1alpha1.RestorePhaseSucceeded))
		Expect(restore.Status.CompletionTime).NotTo(BeNil())
	})
})
