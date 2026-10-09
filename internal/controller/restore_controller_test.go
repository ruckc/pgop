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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		Expect(job.Spec.Template.Spec.Containers[0].Command[2]).To(ContainSubstring(`-d "$PGDATABASE"`))
		Expect(job.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: envPGDatabase, Value: dbName}))
		Expect(job.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{
			Name: envPGHost, Value: fmt.Sprintf("%s.%s.svc.cluster.local", clusterName, RestoreNamespace),
		}))

		By("Verifying pg_restore uses the sslmode and CA from the credentials Secret")
		expectJobTLS(job.Spec.Template.Spec, job.Spec.Template.Spec.Containers[0], clusterName+"-credentials")
	})

	It("should restore into the Database's effective PostgreSQL name", func() {
		runName := newBackupRun(suffix, true)
		clusterName := "cluster-" + suffix
		newCluster(clusterName)
		db := &postgresv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: "db-" + suffix, Namespace: RestoreNamespace},
			Spec: postgresv1alpha1.DatabaseSpec{
				ClusterRef:   postgresv1alpha1.ClusterReference{Name: clusterName},
				DatabaseName: testPGDatabaseName,
			},
		}
		Expect(k8sClient.Create(ctx, db)).To(Succeed())

		restore := &postgresv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: "restore-" + suffix, Namespace: RestoreNamespace},
			Spec: postgresv1alpha1.RestoreSpec{
				Type:         postgresv1alpha1.BackupTypeLogical,
				BackupRunRef: postgresv1alpha1.ClusterReference{Name: runName},
				ClusterRef:   postgresv1alpha1.ClusterReference{Name: clusterName},
				DatabaseRef:  &postgresv1alpha1.ClusterReference{Name: db.Name},
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: restore.Name, Namespace: RestoreNamespace},
		})
		Expect(err).NotTo(HaveOccurred())

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restore.Name + "-restore", Namespace: RestoreNamespace}, job)).To(Succeed())
		Expect(job.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: envPGDatabase, Value: testPGDatabaseName}))
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

	// physicalFixture creates a Cluster, its physical Backup, a Succeeded
	// BackupRun with a backup label and the Cluster's data volumes (ordinals
	// 0..volumes-1). Returns the Cluster and BackupRun names.
	physicalFixture := func(volumes int) (string, string) {
		clusterName := "cluster-" + suffix
		newCluster(clusterName)
		backup := &postgresv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "pbackup-" + suffix, Namespace: RestoreNamespace},
			Spec:       physicalBackupSpec(clusterName),
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		run := &postgresv1alpha1.BackupRun{
			ObjectMeta: metav1.ObjectMeta{Name: "prun-" + suffix, Namespace: RestoreNamespace},
			Spec: postgresv1alpha1.BackupRunSpec{
				BackupRef: postgresv1alpha1.ClusterReference{Name: backup.Name},
				Type:      postgresv1alpha1.BackupRunTypeFull,
			},
		}
		Expect(k8sClient.Create(ctx, run)).To(Succeed())
		run.Status.Phase = postgresv1alpha1.BackupRunPhaseSucceeded
		run.Status.Location = "s3://bucket/pg/" + clusterName + "/backup/main/20260101-020000F"
		Expect(k8sClient.Status().Update(ctx, run)).To(Succeed())
		for ordinal := range volumes {
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("data-%s-%d", clusterName, ordinal),
					Namespace: RestoreNamespace,
					Labels:    map[string]string{LabelAppName: AppNamePostgresql, LabelAppInstance: clusterName},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		}
		return clusterName, run.Name
	}
	getCluster := func(name string) *postgresv1alpha1.Cluster {
		c := &postgresv1alpha1.Cluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: RestoreNamespace}, c)).To(Succeed())
		return c
	}
	annotate := func(clusterName, key, value string) {
		c := getCluster(clusterName)
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		c.Annotations[key] = value
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
	}
	newPhysicalRestore := func(name, clusterName, runName string, targetTime *metav1.Time) (*postgresv1alpha1.Restore, reconcile.Request) {
		restore := &postgresv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: RestoreNamespace},
			Spec: postgresv1alpha1.RestoreSpec{
				Type:         postgresv1alpha1.BackupTypePhysical,
				BackupRunRef: postgresv1alpha1.ClusterReference{Name: runName},
				ClusterRef:   postgresv1alpha1.ClusterReference{Name: clusterName},
				TargetTime:   targetTime,
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())
		return restore, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(restore)}
	}
	reconcileRestore := func(req reconcile.Request, restore *postgresv1alpha1.Restore) {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		err = k8sClient.Get(ctx, req.NamespacedName, restore)
		Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
	}
	// runToJob drives a confirmed Restore up to its restore Job.
	runToJob := func(req reconcile.Request, restore *postgresv1alpha1.Restore) *batchv1.Job {
		reconcileRestore(req, restore) // stop the Cluster
		reconcileRestore(req, restore) // create the Job (envtest has no StatefulSet)
		Expect(restore.Status.JobName).NotTo(BeEmpty())
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restore.Status.JobName, Namespace: RestoreNamespace}, job)).To(Succeed())
		return job
	}

	It("should wait for confirmation, restore the data volume and start the Cluster again", func() {
		clusterName, runName := physicalFixture(2)
		restore, req := newPhysicalRestore("restore-"+suffix, clusterName, runName, nil)

		By("waiting for the allow-restore annotation")
		reconcileRestore(req, restore)
		Expect(restore.Status.Phase).To(Equal(postgresv1alpha1.RestorePhasePending))
		Expect(restore.Status.Conditions[0].Reason).To(Equal(reasonAwaitingConfirmation))
		Expect(getCluster(clusterName).Annotations).NotTo(HaveKey(AnnotationRestoreInProgress))

		By("stopping the Cluster once confirmed")
		annotate(clusterName, AnnotationAllowRestore, restore.Name)
		reconcileRestore(req, restore)
		Expect(getCluster(clusterName).Annotations).To(HaveKeyWithValue(AnnotationRestoreInProgress, restore.Name))
		Expect(restore.Status.Phase).To(Equal(postgresv1alpha1.RestorePhaseRunning))
		Expect(restore.Finalizers).To(ContainElement(restoreFinalizer))

		By("restoring once no pod runs (envtest has no StatefulSet)")
		reconcileRestore(req, restore)
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: restore.Status.JobName, Namespace: RestoreNamespace}, job)).To(Succeed())
		c := job.Spec.Template.Spec.Containers[0]
		Expect(c.Image).To(Equal(DefaultPgbackrestImage))
		Expect(c.Command[2]).To(ContainSubstring(`exec /usr/bin/pgbackrest restore "$@"`))
		Expect(c.Command[4:]).To(Equal(physicalRestoreArgs(restore, "20260101-020000F")))
		Expect(c.Command).To(ContainElement("--set=20260101-020000F"))
		Expect(c.VolumeMounts).To(ContainElement(corev1.VolumeMount{Name: dataVolumeName, MountPath: "/var/lib/postgresql"}))
		Expect(job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("data-" + clusterName + "-0"))
		Expect(*job.Spec.Template.Spec.SecurityContext.RunAsUser).To(Equal(int64(999)))
		standby := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "data-" + clusterName + "-1", Namespace: RestoreNamespace},
			standby)).To(Succeed())
		Expect(standby.DeletionTimestamp).To(BeNil(), "standby volumes are kept until the restore succeeded")

		By("deleting the standby volume once the Job succeeded, then starting the Cluster")
		job.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileRestore(req, restore)
		Expect(getCluster(clusterName).Annotations).To(HaveKey(AnnotationRestoreInProgress))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(standby), standby)).To(Succeed())
		Expect(standby.DeletionTimestamp).NotTo(BeNil())
		// envtest runs no PVC protection controller to release the volume.
		standby.Finalizers = nil
		Expect(k8sClient.Update(ctx, standby)).To(Succeed())
		reconcileRestore(req, restore)
		Expect(restore.Status.Phase).To(Equal(postgresv1alpha1.RestorePhaseSucceeded))
		cluster := getCluster(clusterName)
		Expect(cluster.Annotations).NotTo(HaveKey(AnnotationRestoreInProgress))
		Expect(cluster.Annotations).NotTo(HaveKey(AnnotationAllowRestore), "the confirmation is used up")
		Expect(cluster.Status.LastRestore).NotTo(BeNil())
		Expect(cluster.Status.LastRestore.UID).To(Equal(string(restore.UID)))
		Expect(cluster.Status.LastRestore.Result).To(Equal(reasonSucceeded))

		By("an identical Restore re-created right after needs a UID confirmation")
		Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
		reconcileRestore(req, restore)
		again, req2 := newPhysicalRestore("restore-"+suffix, clusterName, runName, nil)
		annotate(clusterName, AnnotationAllowRestore, again.Name)
		reconcileRestore(req2, again)
		Expect(again.Status.Phase).To(Equal(postgresv1alpha1.RestorePhasePending))
		Expect(again.Status.Conditions[0].Message).To(ContainSubstring("identical Restore"))
		Expect(getCluster(clusterName).Annotations).NotTo(HaveKey(AnnotationRestoreInProgress))
		annotate(clusterName, AnnotationAllowRestore, again.Name+"/"+string(again.UID))
		reconcileRestore(req2, again)
		Expect(getCluster(clusterName).Annotations).To(HaveKeyWithValue(AnnotationRestoreInProgress, again.Name))
	})

	It("should keep the Cluster stopped when a Restore is deleted while its Job runs", func() {
		clusterName, runName := physicalFixture(1)
		restore, req := newPhysicalRestore("restore-"+suffix, clusterName, runName, nil)
		annotate(clusterName, AnnotationAllowRestore, restore.Name)
		job := runToJob(req, restore)

		Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
		reconcileRestore(req, restore)
		cluster := getCluster(clusterName)
		Expect(cluster.Annotations).To(HaveKeyWithValue(AnnotationRestoreInterrupted, restore.Name))
		Expect(cluster.Annotations).NotTo(HaveKey(AnnotationRestoreInProgress))
		Expect(pausedForRestore(cluster)).To(BeTrue())
		Expect(restore.Finalizers).To(ContainElement(restoreFinalizer), "waits for the Job to stop")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), job)).To(Succeed())
		Expect(job.DeletionTimestamp).NotTo(BeNil(), "the Job is deleted in the foreground")

		By("a pod of the Job still exists")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: RestoreNamespace,
				Labels: map[string]string{labelJobName: job.Name}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		job.Finalizers = nil
		Expect(k8sClient.Update(ctx, job)).To(Succeed())
		reconcileRestore(req, restore)
		Expect(restore.Finalizers).To(ContainElement(restoreFinalizer))

		By("finishing once the pod is gone; the Cluster stays stopped")
		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
		}).Should(BeTrue())
		reconcileRestore(req, restore)
		err := k8sClient.Get(ctx, req.NamespacedName, &postgresv1alpha1.Restore{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		cluster = getCluster(clusterName)
		Expect(cluster.Annotations).To(HaveKeyWithValue(AnnotationRestoreInterrupted, restore.Name))
		Expect(cluster.Status.LastRestore.Result).To(Equal("Interrupted"))

		By("a new confirmed Restore that succeeds clears the interruption")
		next, req2 := newPhysicalRestore("restore2-"+suffix, clusterName, runName, nil)
		annotate(clusterName, AnnotationAllowRestore, next.Name)
		job2 := runToJob(req2, next)
		job2.Status.Succeeded = 1
		Expect(k8sClient.Status().Update(ctx, job2)).To(Succeed())
		reconcileRestore(req2, next)
		Expect(next.Status.Phase).To(Equal(postgresv1alpha1.RestorePhaseSucceeded))
		Expect(pausedForRestore(getCluster(clusterName))).To(BeFalse())
	})

	It("should start the Cluster again when a Restore is deleted before its Job ran", func() {
		clusterName, runName := physicalFixture(0)
		restore, req := newPhysicalRestore("restore-"+suffix, clusterName, runName, nil)
		annotate(clusterName, AnnotationAllowRestore, restore.Name)
		reconcileRestore(req, restore)
		Expect(getCluster(clusterName).Annotations).To(HaveKey(AnnotationRestoreInProgress))
		Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
		reconcileRestore(req, restore)
		cluster := getCluster(clusterName)
		Expect(pausedForRestore(cluster)).To(BeFalse())
		Expect(cluster.Annotations).NotTo(HaveKey(AnnotationAllowRestore))
	})

	It("should keep the Cluster stopped as interrupted when the restore Job fails", func() {
		clusterName, runName := physicalFixture(2)
		target := metav1.NewTime(time.Unix(1767240000, 0).UTC())
		restore, req := newPhysicalRestore("restore-"+suffix, clusterName, runName, &target)
		annotate(clusterName, AnnotationAllowRestore, restore.Name)
		job := runToJob(req, restore)
		Expect(job.Spec.Template.Spec.Containers[0].Command).To(ContainElement("--target=2026-01-01 04:00:00+00"))

		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded", LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileRestore(req, restore)
		Expect(restore.Status.Phase).To(Equal(postgresv1alpha1.RestorePhaseFailed))
		Expect(restore.Status.Conditions[0].Message).To(ContainSubstring("Run a new physical Restore"))
		cluster := getCluster(clusterName)
		Expect(cluster.Annotations).To(HaveKeyWithValue(AnnotationRestoreInterrupted, restore.Name))
		Expect(cluster.Annotations).NotTo(HaveKey(AnnotationAllowRestore))
		Expect(cluster.Status.LastRestore.Result).To(Equal("Failed"))
		standby := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "data-" + clusterName + "-1", Namespace: RestoreNamespace},
			standby)).To(Succeed())
		Expect(standby.DeletionTimestamp).To(BeNil(), "the standbys survive a failed restore")

		By("deleting the failed Restore does not start the Cluster")
		Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
		reconcileRestore(req, restore)
		Expect(pausedForRestore(getCluster(clusterName))).To(BeTrue())
	})

	It("should not follow a Job of an earlier Restore with the same name", func() {
		clusterName, runName := physicalFixture(1)
		old := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "restore-" + suffix + "-restore", Namespace: RestoreNamespace},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "c", Image: "x"}},
			}}},
		}
		Expect(k8sClient.Create(ctx, old)).To(Succeed())
		restore, req := newPhysicalRestore("restore-"+suffix, clusterName, runName, nil)
		annotate(clusterName, AnnotationAllowRestore, restore.Name)
		reconcileRestore(req, restore)
		reconcileRestore(req, restore)
		Expect(restore.Status.JobName).To(BeEmpty())
		Expect(restore.Status.Conditions[0].Reason).To(Equal(reasonWaitingForOldJob))
	})

	DescribeTable("should reject a physical restore that cannot work",
		func(mutate func(run *postgresv1alpha1.BackupRun, backup *postgresv1alpha1.Backup), restoreCluster, msg string) {
			clusterName, runName := physicalFixture(0)
			run := &postgresv1alpha1.BackupRun{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: runName, Namespace: RestoreNamespace}, run)).To(Succeed())
			backup := &postgresv1alpha1.Backup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: run.Spec.BackupRef.Name, Namespace: RestoreNamespace}, backup)).To(Succeed())
			mutate(run, backup)
			Expect(k8sClient.Status().Update(ctx, run)).To(Succeed())
			Expect(k8sClient.Update(ctx, backup)).To(Succeed())
			if restoreCluster == "" {
				restoreCluster = clusterName
			} else {
				newCluster(restoreCluster + suffix)
				restoreCluster += suffix
			}
			restore, req := newPhysicalRestore("restore-"+suffix, restoreCluster, runName, nil)
			annotate(restoreCluster, AnnotationAllowRestore, restore.Name)
			reconcileRestore(req, restore)
			Expect(restore.Status.Phase).To(Equal(postgresv1alpha1.RestorePhaseFailed))
			Expect(restore.Status.Conditions[0].Message).To(ContainSubstring(msg))
			cluster := getCluster(restoreCluster)
			Expect(pausedForRestore(cluster)).To(BeFalse())
			Expect(cluster.Annotations).NotTo(HaveKey(AnnotationAllowRestore))
		},
		Entry("a Failed BackupRun", func(run *postgresv1alpha1.BackupRun, _ *postgresv1alpha1.Backup) {
			run.Status.Phase = postgresv1alpha1.BackupRunPhaseFailed
		}, "", "is Failed, not Succeeded"),
		Entry("a BackupRun without location", func(run *postgresv1alpha1.BackupRun, _ *postgresv1alpha1.Backup) {
			run.Status.Location = ""
		}, "", "has no backup location"),
		Entry("another Cluster", func(*postgresv1alpha1.BackupRun, *postgresv1alpha1.Backup) {}, "other-",
			"the Cluster the Backup was taken from"),
	)

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
