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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// expectJobTLS asserts that a pod spec's container connects with the sslmode
// and CA from the credentials Secret (see job_tls.go).
func expectJobTLS(pod corev1.PodSpec, container corev1.Container, credentialsSecret string) {
	GinkgoHelper()

	var sslMode, rootCert *corev1.EnvVar
	for i := range container.Env {
		switch container.Env[i].Name {
		case envPGSSLMode:
			sslMode = &container.Env[i]
		case envPGSSLRootCert:
			rootCert = &container.Env[i]
		}
	}
	Expect(sslMode).NotTo(BeNil(), "PGSSLMODE not set")
	Expect(sslMode.ValueFrom).NotTo(BeNil())
	Expect(sslMode.ValueFrom.SecretKeyRef).NotTo(BeNil())
	Expect(sslMode.ValueFrom.SecretKeyRef.Name).To(Equal(credentialsSecret))
	Expect(sslMode.ValueFrom.SecretKeyRef.Key).To(Equal(SecretKeySSLMode))
	Expect(rootCert).NotTo(BeNil(), "PGSSLROOTCERT not set")
	Expect(rootCert.Value).To(Equal(jobTLSCAPath))

	Expect(container.VolumeMounts).To(ContainElement(corev1.VolumeMount{
		Name: jobTLSVolumeName, MountPath: jobTLSMountPath, ReadOnly: true,
	}))

	var vol *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == jobTLSVolumeName {
			vol = &pod.Volumes[i]
		}
	}
	Expect(vol).NotTo(BeNil(), "CA volume missing")
	Expect(vol.Secret).NotTo(BeNil())
	Expect(vol.Secret.SecretName).To(Equal(credentialsSecret))
	Expect(vol.Secret.Optional).To(HaveValue(BeTrue()))
	Expect(vol.Secret.Items).To(ConsistOf(corev1.KeyToPath{Key: SecretKeyCACert, Path: SecretKeyCACert}))
}

var _ = Describe("Backup Controller", func() {
	const ns = "default"

	var (
		ctx        context.Context
		reconciler *BackupReconciler
		suffix     string
	)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &BackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	newLogicalBackup := func(tls bool) (*postgresv1alpha1.Backup, *postgresv1alpha1.Cluster) {
		cluster := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-cluster-" + suffix, Namespace: ns},
			Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage},
		}
		if tls {
			cluster.Spec.TLS = &postgresv1alpha1.ClusterTLSSpec{}
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		db := &postgresv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-db-" + suffix, Namespace: ns},
			Spec: postgresv1alpha1.DatabaseSpec{
				ClusterRef: postgresv1alpha1.ClusterReference{Name: cluster.Name},
				Owner:      "app-user",
			},
		}
		Expect(k8sClient.Create(ctx, db)).To(Succeed())

		backup := &postgresv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-" + suffix, Namespace: ns},
			Spec: postgresv1alpha1.BackupSpec{
				Type:        postgresv1alpha1.BackupTypeLogical,
				DatabaseRef: &postgresv1alpha1.ClusterReference{Name: db.Name},
				Destination: postgresv1alpha1.DestinationSpec{
					Type: postgresv1alpha1.DestinationTypeS3,
					S3:   &postgresv1alpha1.S3Destination{Bucket: "b", Region: "eu-west-1"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		return backup, cluster
	}

	reconcileBackup := func(backup *postgresv1alpha1.Backup) {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: backup.Name, Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	getCronJob := func(name string) *batchv1.CronJob {
		GinkgoHelper()
		cj := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cj)).To(Succeed())
		return cj
	}

	for _, tls := range []bool{false, true} {
		It(fmt.Sprintf("wires pg_dump to the credentials Secret's sslmode and CA (spec.tls set: %t)", tls), func() {
			backup, cluster := newLogicalBackup(tls)
			reconcileBackup(backup)

			for _, runType := range []string{"schema", "data"} {
				cj := getCronJob(backup.Name + "-" + runType)
				pod := cj.Spec.JobTemplate.Spec.Template.Spec
				Expect(pod.InitContainers).To(HaveLen(1))
				dump := pod.InitContainers[0]
				expectJobTLS(pod, dump, cluster.Name+"-credentials")
				Expect(dump.Env).To(ContainElement(corev1.EnvVar{
					Name: envPGHost, Value: fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, ns),
				}))
				Expect(cj.Annotations).To(HaveKey(annotationSpecHash))
			}
		})
	}

	It("does not rewrite an up-to-date CronJob", func() {
		backup, _ := newLogicalBackup(true)
		reconcileBackup(backup)
		before := getCronJob(backup.Name + "-data").ResourceVersion

		reconcileBackup(backup)
		Expect(getCronJob(backup.Name + "-data").ResourceVersion).To(Equal(before))
	})

	It("converges a CronJob created by an older operator, keeping spec.suspend", func() {
		backup, cluster := newLogicalBackup(true)
		reconcileBackup(backup)

		By("reverting the CronJob to the pre-TLS shape (no annotation, no TLS env or CA volume) and suspending it")
		cj := getCronJob(backup.Name + "-data")
		delete(cj.Annotations, annotationSpecHash)
		pod := &cj.Spec.JobTemplate.Spec.Template.Spec
		pod.Volumes = pod.Volumes[:1]
		pod.InitContainers[0].Env = pod.InitContainers[0].Env[:5]
		pod.InitContainers[0].VolumeMounts = pod.InitContainers[0].VolumeMounts[:1]
		cj.Spec.Suspend = new(true)
		Expect(k8sClient.Update(ctx, cj)).To(Succeed())

		reconcileBackup(backup)

		cj = getCronJob(backup.Name + "-data")
		expectJobTLS(cj.Spec.JobTemplate.Spec.Template.Spec, cj.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0],
			cluster.Name+"-credentials")
		Expect(cj.Annotations).To(HaveKey(annotationSpecHash))
		Expect(cj.Spec.Suspend).To(HaveValue(BeTrue()), "spec.suspend must be preserved")
	})

	It("follows a Cluster port change", func() {
		backup, cluster := newLogicalBackup(false)
		reconcileBackup(backup)

		cluster.Spec.Port = 6543
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		reconcileBackup(backup)

		cj := getCronJob(backup.Name + "-schema")
		Expect(cj.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0].Env).
			To(ContainElement(corev1.EnvVar{Name: envPGPort, Value: "6543"}))
	})

	It("maps a Cluster to the Backups that read from it", func() {
		logical, cluster := newLogicalBackup(false)
		physical := &postgresv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "bk-phys-" + suffix, Namespace: ns},
			Spec: postgresv1alpha1.BackupSpec{
				Type:       postgresv1alpha1.BackupTypePhysical,
				ClusterRef: &postgresv1alpha1.ClusterReference{Name: cluster.Name},
				Destination: postgresv1alpha1.DestinationSpec{
					Type: postgresv1alpha1.DestinationTypeS3,
					S3:   &postgresv1alpha1.S3Destination{Bucket: "b", Region: "eu-west-1"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, physical)).To(Succeed())

		Expect(reconciler.backupsForCluster(ctx, cluster)).To(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Name: logical.Name, Namespace: ns}},
			reconcile.Request{NamespacedName: types.NamespacedName{Name: physical.Name, Namespace: ns}},
		))
		other := &postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-" + suffix, Namespace: ns}}
		Expect(reconciler.backupsForCluster(ctx, other)).To(BeEmpty())
	})
})
