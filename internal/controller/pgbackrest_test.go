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
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// envValue returns the value of the named variable in env ("" if unset).
func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func physicalBackupSpec(clusterName string) postgresv1alpha1.BackupSpec {
	return postgresv1alpha1.BackupSpec{
		Type:       postgresv1alpha1.BackupTypePhysical,
		ClusterRef: &postgresv1alpha1.ClusterReference{Name: clusterName},
		Destination: postgresv1alpha1.DestinationSpec{
			Type: postgresv1alpha1.DestinationTypeS3,
			S3: &postgresv1alpha1.S3Destination{
				Bucket:               "bucket",
				Prefix:               "pg/" + clusterName,
				Region:               "eu-central-1",
				Endpoint:             "https://s3.example.com:9443",
				CredentialsSecretRef: &corev1.LocalObjectReference{Name: "s3-creds"},
				CASecretRef:          &postgresv1alpha1.SecretKeySelector{Name: "s3-ca", Key: "ca.crt"},
			},
		},
	}
}

var _ = Describe("pgBackRest", func() {
	It("pins the same pgBackRest version as images/pgbackrest/Dockerfile", func() {
		dockerfile, err := os.ReadFile("../../images/pgbackrest/Dockerfile")
		Expect(err).NotTo(HaveOccurred())
		versions := regexp.MustCompile(`(?m)^ARG PGBACKREST_VERSION=(\S+)$`).FindAllStringSubmatch(string(dockerfile), -1)
		Expect(versions).NotTo(BeEmpty())
		for _, v := range versions {
			Expect(v[1]).To(Equal(pgbackrestVersion))
		}
	})

	DescribeTable("parses the S3 endpoint for pgBackRest",
		func(endpoint, region, host string, port int, wantErr bool) {
			ep, err := parseS3Endpoint(&postgresv1alpha1.S3Destination{Endpoint: endpoint, Region: region})
			if wantErr {
				Expect(err).To(HaveOccurred())
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(ep).To(Equal(s3Endpoint{Host: host, Port: port}))
		},
		Entry("AWS default", "", "eu-west-1", "s3.eu-west-1.amazonaws.com", 443, false),
		Entry("https URL", "https://minio.example.com", "x", "minio.example.com", 443, false),
		Entry("https URL with port", "https://minio.example.com:9000/", "x", "minio.example.com", 9000, false),
		Entry("bare host and port", "minio:9443", "x", "minio", 9443, false),
		Entry("http is rejected", "http://minio:9000", "x", "", 0, true),
		Entry("path is rejected", "https://minio/bucket", "x", "", 0, true),
	)

	DescribeTable("picks the image of a Cluster with physical backups",
		func(image string, major *int32, optIn bool, want string) {
			c := &postgresv1alpha1.Cluster{Spec: postgresv1alpha1.ClusterSpec{Image: image, PostgresMajorVersion: major}}
			b := &postgresv1alpha1.Backup{Spec: postgresv1alpha1.BackupSpec{
				Physical: &postgresv1alpha1.PhysicalBackupConfig{PostgresImageIncludesPgbackrest: optIn}}}
			got, err := postgresImageForBackups(c, b)
			if want == "" {
				Expect(err).To(MatchError(errUnsupportedBackupImage))
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("default", "", nil, false, "ghcr.io/ruckc/pgop-postgres:18-"+pgbackrestVersion),
		Entry("official major", "postgres:17", nil, false, "ghcr.io/ruckc/pgop-postgres:17-"+pgbackrestVersion),
		Entry("official minor, trixie", "docker.io/library/postgres:16.4-trixie", nil, false, "ghcr.io/ruckc/pgop-postgres:16-"+pgbackrestVersion),
		Entry("alpine is rejected (musl)", "postgres:18-alpine", nil, false, ""),
		Entry("alpine with minor is rejected", "postgres:17.2-alpine3.21", nil, false, ""),
		Entry("bookworm is rejected (other glibc)", "postgres:16-bookworm", nil, false, ""),
		Entry("digest only is rejected (variant unknown)", "postgres@sha256:abc", new(int32(18)), false, ""),
		Entry("latest is rejected", "postgres:latest", new(int32(18)), false, ""),
		Entry("unsupported major is rejected", "postgres:15", nil, false, ""),
		Entry("custom image is rejected", "registry.example.com/pg:18-pgbackrest", nil, false, ""),
		Entry("custom image with opt-in is kept", "registry.example.com/pg:18-pgbackrest", nil, true, "registry.example.com/pg:18-pgbackrest"),
		Entry("alpine with opt-in is kept", "postgres:18-alpine", nil, true, "postgres:18-alpine"),
	)

	It("bounds the archive push queue", func() {
		c := &postgresv1alpha1.Cluster{Spec: postgresv1alpha1.ClusterSpec{Storage: postgresv1alpha1.StorageSpec{Size: "10Gi"}}}
		b := &postgresv1alpha1.Backup{}
		Expect(archivePushQueueMax(b, c)).To(Equal(int64(10<<30) / 4))
		c.Spec.Storage.Size = "100Mi"
		Expect(archivePushQueueMax(b, c)).To(Equal(int64(64 << 20)))
		q := resource.MustParse("2Gi")
		b.Spec.Physical = &postgresv1alpha1.PhysicalBackupConfig{ArchivePushQueueMax: &q}
		Expect(archivePushQueueMax(b, c)).To(Equal(int64(2 << 30)))

		b = &postgresv1alpha1.Backup{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: physicalBackupSpec("c")}
		small := resource.MustParse("1Mi")
		b.Spec.Physical = &postgresv1alpha1.PhysicalBackupConfig{ArchivePushQueueMax: &small}
		Expect(validatePhysicalBackup(b)).To(MatchError(ContainSubstring("archivePushQueueMax")))
	})

	DescribeTable("reports WAL archiving health",
		func(s postgres.ArchiverStats, status metav1.ConditionStatus, reason string) {
			gotStatus, gotReason, _ := archivingCondition(s)
			Expect(gotStatus).To(Equal(status))
			Expect(gotReason).To(Equal(reason))
		},
		Entry("nothing archived yet", postgres.ArchiverStats{}, metav1.ConditionUnknown, reasonNoWALArchivedYet),
		Entry("archiving", postgres.ArchiverStats{ArchivedCount: 3, LastArchivedTime: time.Now()},
			metav1.ConditionTrue, reasonArchiving),
		Entry("failing before the stanza exists", postgres.ArchiverStats{FailedCount: 2, LastFailedTime: time.Now()},
			metav1.ConditionFalse, reasonArchiveFailing),
		Entry("failing after earlier success", postgres.ArchiverStats{ArchivedCount: 3, LastArchivedTime: time.Now().Add(-time.Hour),
			FailedCount: 1, LastFailedTime: time.Now()}, metav1.ConditionFalse, reasonArchiveFailing),
		Entry("recovered after a failure", postgres.ArchiverStats{ArchivedCount: 3, LastArchivedTime: time.Now(),
			FailedCount: 1, LastFailedTime: time.Now().Add(-time.Hour)}, metav1.ConditionTrue, reasonArchiving),
	)

	It("maps retention onto full backup retention", func() {
		b := &postgresv1alpha1.Backup{}
		Expect(pgbackrestRetentionEnv(b)).To(ConsistOf(envVar("PGBACKREST_REPO1_RETENTION_FULL", "9999999")))
		b.Spec.Retention = postgresv1alpha1.RetentionSpec{Disabled: new(false), KeepLast: new(int32(4))}
		Expect(pgbackrestRetentionEnv(b)).To(ConsistOf(envVar("PGBACKREST_REPO1_RETENTION_FULL", "4")))
		b.Spec.Retention = postgresv1alpha1.RetentionSpec{Disabled: new(false), KeepDays: new(int32(30))}
		Expect(pgbackrestRetentionEnv(b)).To(ConsistOf(
			envVar("PGBACKREST_REPO1_RETENTION_FULL_TYPE", "time"),
			envVar("PGBACKREST_REPO1_RETENTION_FULL", "30")))
	})

	It("configures the repository through the environment", func() {
		b := &postgresv1alpha1.Backup{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: physicalBackupSpec("c")}
		b.Spec.Encryption = &postgresv1alpha1.EncryptionSpec{Enabled: true,
			KeySecretRef: &postgresv1alpha1.SecretKeySelector{Name: "enc", Key: "pass"}}
		env := pgbackrestRepoEnv(b)
		Expect(envValue(env, "PGBACKREST_REPO1_PATH")).To(Equal("/pg/c"))
		Expect(envValue(env, "PGBACKREST_REPO1_S3_ENDPOINT")).To(Equal("s3.example.com"))
		Expect(envValue(env, "PGBACKREST_REPO1_STORAGE_PORT")).To(Equal("9443"))
		Expect(envValue(env, "PGBACKREST_REPO1_S3_URI_STYLE")).To(Equal("path"))
		Expect(envValue(env, "PGBACKREST_REPO1_STORAGE_CA_FILE")).To(Equal("/etc/pgop/s3-ca/ca.crt"))
		Expect(envValue(env, "PGBACKREST_REPO1_CIPHER_TYPE")).To(Equal("aes-256-cbc"))
		Expect(env).To(ContainElement(secretEnv("PGBACKREST_REPO1_CIPHER_PASS", "enc", "pass")))
		Expect(env).To(ContainElement(secretEnv("PGBACKREST_REPO1_S3_KEY", "s3-creds", envAWSAccessKeyID)))

		b.Spec.Destination.S3.Prefix = ""
		b.Namespace = "ns"
		Expect(repoPath(b)).To(Equal("/ns/c/b"))
	})

	It("builds the restore options", func() {
		const delta, promote = "--delta", "--target-action=promote"
		r := &postgresv1alpha1.Restore{}
		Expect(physicalRestoreArgs(r, "")).To(Equal([]string{delta}))
		Expect(physicalRestoreArgs(r, "20260101-020000F")).To(Equal(
			[]string{delta, "--set=20260101-020000F", "--type=immediate", promote}))
		t := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
		r.Spec.TargetTime = &t
		Expect(physicalRestoreArgs(r, "20260101-020000F")).To(Equal(
			[]string{delta, "--type=time", "--target=2026-01-02 03:04:05+00", promote}))
	})

	It("records and parses backup locations", func() {
		b := &postgresv1alpha1.Backup{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: physicalBackupSpec("c")}
		pod := func(msg string, code int32) corev1.Pod {
			return corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  pgbackrestContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: msg}},
			}}}}
		}
		Expect(physicalBackupLocation(b, []corev1.Pod{pod("label=20260101-020000F_20260102-020000I\n", 0)})).
			To(Equal("s3://bucket/pg/c/backup/main/20260101-020000F_20260102-020000I"))
		Expect(physicalBackupLocation(b, []corev1.Pod{pod("label=20260101-020000F\n", 1)})).To(BeEmpty())
		Expect(physicalBackupLocation(b, []corev1.Pod{pod("label=$(rm -rf /)\n", 0)})).To(BeEmpty())

		label, err := backupLabelFromLocation("s3://bucket/pg/c/backup/main/20260101-020000F")
		Expect(err).NotTo(HaveOccurred())
		Expect(label).To(Equal("20260101-020000F"))
		_, err = backupLabelFromLocation("s3://bucket/x/20260101T020000.dump")
		Expect(err).To(HaveOccurred())
		label, err = backupLabelFromLocation("")
		Expect(err).NotTo(HaveOccurred())
		Expect(label).To(BeEmpty())
	})

	It("issues and keeps the pgBackRest TLS material", func() {
		names := []string{"c.ns.svc.cluster.local", "c.ns.svc", "c.ns", "c"}
		now := time.Now()
		data, next, err := planPgbackrestTLS(nil, names, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(HaveKey(pgbackrestTLSKeyCAKey))
		Expect(next).To(BeTemporally("~", now.Add(pgbackrestLeafValidity-pgbackrestLeafRenewBefor), time.Hour))

		parse := func(key string) *x509.Certificate {
			b, _ := pem.Decode(data[key])
			Expect(b).NotTo(BeNil())
			c, err := x509.ParseCertificate(b.Bytes)
			Expect(err).NotTo(HaveOccurred())
			return c
		}
		roots := x509.NewCertPool()
		roots.AddCert(parse(TLSSecretKeyCA))
		_, err = parse(TLSSecretKeyCert).Verify(x509.VerifyOptions{DNSName: names[0], Roots: roots,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		Expect(err).NotTo(HaveOccurred())
		client := parse(pgbackrestTLSKeyClientCert)
		Expect(client.Subject.CommonName).To(Equal(pgbackrestClientCN))
		_, err = client.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		Expect(err).NotTo(HaveOccurred())

		By("keeping valid material as it is")
		again, _, err := planPgbackrestTLS(data, names, now.Add(time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(data))

		By("renewing the certificates when they are due, keeping the CA")
		later, _, err := planPgbackrestTLS(data, names, now.Add(pgbackrestLeafValidity-pgbackrestLeafRenewBefor+time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(later[TLSSecretKeyCA]).To(Equal(data[TLSSecretKeyCA]))
		Expect(later[TLSSecretKeyCert]).NotTo(Equal(data[TLSSecretKeyCert]))
		Expect(later[pgbackrestTLSKeyClientCert]).NotTo(Equal(data[pgbackrestTLSKeyClientCert]))
	})
})

var _ = Describe("Physical backups (envtest)", func() {
	const ns = "default"
	var (
		ctx    context.Context
		suffix string
	)
	BeforeEach(func() {
		ctx = context.Background()
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	It("adds pgBackRest to the Cluster while it has a physical Backup, and removes it again", func() {
		clusterName := "pbk-" + suffix
		cluster := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage, Replicas: 1, Port: 5432},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() {
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)
			cluster.Finalizers = nil
			_ = k8sClient.Update(ctx, cluster)
			_ = k8sClient.Delete(ctx, cluster)
		}()
		r := &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}
		sts := func() *appsv1.StatefulSet {
			s := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, s)).To(Succeed())
			return s
		}
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		before := sts()
		Expect(before.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(before.Spec.Template.Spec.Containers[0].Image).To(Equal(DefaultPostgresImage))

		By("creating a physical Backup")
		backup := &postgresv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "pbk-backup-" + suffix, Namespace: ns},
			Spec:       physicalBackupSpec(clusterName),
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		s := sts()
		containers := s.Spec.Template.Spec.Containers
		Expect(containers).To(HaveLen(2))
		pg, sidecar := containers[0], containers[1]
		Expect(pg.Image).To(Equal(PgopPostgresImage(18)))
		Expect(sidecar.Name).To(Equal(pgbackrestContainerName))
		Expect(sidecar.Image).To(Equal(pg.Image))
		Expect(strings.Join(pg.Args, " ")).To(ContainSubstring("-c archive_mode=on -c archive_command=/usr/bin/pgbackrest --stanza=main archive-push %p"))
		Expect(envValue(pg.Env, "PGBACKREST_REPO1_S3_BUCKET")).To(Equal("bucket"))
		Expect(envValue(sidecar.Env, "PGBACKREST_TLS_SERVER_AUTH")).To(Equal(pgbackrestClientCN + "=main"))
		Expect(pg.VolumeMounts).To(ContainElement(HaveField("MountPath", postgresSocketDir)))
		Expect(s.Spec.Template.Spec.Volumes).To(ContainElement(HaveField("Name", pgbackrestTLSVolumeName)))
		Expect(s.Spec.Template.Spec.Volumes).To(ContainElement(HaveField("Name", s3CAVolumeName)))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, svc)).To(Succeed())
		Expect(svc.Spec.Ports).To(ContainElement(HaveField("Port", int32(pgbackrestTLSPort))))

		tlsSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pgbackrestTLSSecretName(cluster), Namespace: ns}, tlsSecret)).To(Succeed())
		Expect(tlsSecret.Data).To(HaveKey(pgbackrestTLSKeyClientCert))
		Expect(metav1.IsControlledBy(tlsSecret, cluster)).To(BeTrue())

		By("reconciling again changes nothing")
		rv := s.ResourceVersion
		secretRV := tlsSecret.ResourceVersion
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(sts().ResourceVersion).To(Equal(rv))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(tlsSecret), tlsSecret)).To(Succeed())
		Expect(tlsSecret.ResourceVersion).To(Equal(secretRV))

		By("pausing the Cluster for a restore")
		Expect(k8sClient.Get(ctx, req.NamespacedName, cluster)).To(Succeed())
		cluster.Annotations = map[string]string{AnnotationRestoreInProgress: "r1"}
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(*sts().Spec.Replicas).To(Equal(int32(0)))
		Expect(k8sClient.Get(ctx, req.NamespacedName, cluster)).To(Succeed())
		delete(cluster.Annotations, AnnotationRestoreInProgress)
		Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(*sts().Spec.Replicas).To(Equal(int32(1)))

		By("deleting the Backup")
		Expect(k8sClient.Delete(ctx, backup)).To(Succeed())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(backup), &postgresv1alpha1.Backup{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		after := sts()
		Expect(after.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(after.Spec.Template.Spec.Containers[0].Image).To(Equal(DefaultPostgresImage))
		Expect(after.Spec.Template.Spec.Containers[0].Args).To(BeEmpty())
		Expect(after.Spec.Template.Spec.Volumes).To(BeEmpty())
		Expect(k8sClient.Get(ctx, req.NamespacedName, svc)).To(Succeed())
		Expect(svc.Spec.Ports).To(HaveLen(1))
		err = k8sClient.Get(ctx, client.ObjectKeyFromObject(tlsSecret), &corev1.Secret{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("schedules pgBackRest backup Jobs and records them as BackupRuns", func() {
		clusterName := "pbkj-" + suffix
		cluster := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage, Port: 5433},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		backup := &postgresv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "pbkj-backup-" + suffix, Namespace: ns},
			Spec:       physicalBackupSpec(clusterName),
		}
		backup.Spec.BackupRunTTL = "1h"
		backup.Spec.Physical = &postgresv1alpha1.PhysicalBackupConfig{Image: "example.com/pgbackrest:custom"}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())

		r := &BackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(backup)}
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		cj := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backup.Name + "-full", Namespace: ns}, cj)).To(Succeed())
		Expect(cj.Spec.Schedule).To(Equal("0 2 * * 0"))
		Expect(cj.Spec.JobTemplate.Labels).To(HaveKeyWithValue(labelBackupType, "full"))
		pod := cj.Spec.JobTemplate.Spec.Template.Spec
		c := pod.Containers[0]
		Expect(c.Image).To(Equal("example.com/pgbackrest:custom"))
		Expect(envValue(c.Env, "PGBACKREST_PG1_HOST")).To(Equal(clusterHost(cluster)))
		Expect(envValue(c.Env, "PGBACKREST_PG1_HOST_TYPE")).To(Equal("tls"))
		Expect(envValue(c.Env, "PGBACKREST_PG1_PORT")).To(Equal("5433"))
		Expect(envValue(c.Env, "PGOP_BACKUP_TYPE")).To(Equal("full"))
		Expect(*pod.SecurityContext.RunAsUser).To(Equal(int64(999)))
		Expect(slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool {
			return v.Secret != nil && v.Secret.SecretName == pgbackrestTLSSecretName(cluster)
		})).To(BeTrue())
		incr := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backup.Name + "-incremental", Namespace: ns}, incr)).To(Succeed())
		Expect(envValue(incr.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env, "PGOP_BACKUP_TYPE")).To(Equal("incr"))

		Expect(k8sClient.Get(ctx, req.NamespacedName, backup)).To(Succeed())
		Expect(backup.Status.Conditions).To(ContainElement(HaveField("Status", metav1.ConditionTrue)))

		By("a Job created from the CronJob is recorded as a BackupRun")
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "manual-" + suffix, Namespace: ns, Labels: cj.Spec.JobTemplate.Labels},
			Spec:       *cj.Spec.JobTemplate.Spec.DeepCopy(),
		}
		Expect(k8sClient.Create(ctx, job)).To(Succeed())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		run := &postgresv1alpha1.BackupRun{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), run)).To(Succeed())
		Expect(run.Spec.Type).To(Equal(postgresv1alpha1.BackupRunTypeFull))
		Expect(run.Spec.TTL.Duration).To(Equal(time.Hour))
		Expect(run.Status.JobName).To(Equal(job.Name))
		Expect(metav1.IsControlledBy(run, backup)).To(BeTrue())

		By("a second physical Backup of the same Cluster is rejected")
		second := &postgresv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "pbkj-second-" + suffix, Namespace: ns},
			Spec:       physicalBackupSpec(clusterName),
		}
		Expect(k8sClient.Create(ctx, second)).To(Succeed())
		expectInvalid := func(b *postgresv1alpha1.Backup, msg string) {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(b)})
			Expect(err).NotTo(HaveOccurred(), "an invalid Backup is reported, not retried")
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(b), b)).To(Succeed())
			c := meta.FindStatusCondition(b.Status.Conditions, ConditionTypeAvailable)
			Expect(c).NotTo(BeNil())
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal(reasonInvalid))
			Expect(c.Message).To(ContainSubstring(msg))
			err = k8sClient.Get(ctx, types.NamespacedName{Name: b.Name + "-full", Namespace: ns}, &batchv1.CronJob{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "nothing is scheduled")
		}
		expectInvalid(second, "only one physical Backup per Cluster")

		By("an http:// endpoint is rejected")
		third := &postgresv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "pbkj-third-" + suffix, Namespace: ns},
			Spec:       physicalBackupSpec("other-" + suffix),
		}
		third.Spec.Destination.S3.Endpoint = "http://minio:9000"
		Expect(k8sClient.Create(ctx, third)).To(Succeed())
		expectInvalid(third, "https://")

		By("a Cluster image without pgBackRest is rejected")
		alpine := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "pbka-" + suffix, Namespace: ns},
			Spec:       postgresv1alpha1.ClusterSpec{Image: "postgres:18-alpine"},
		}
		Expect(k8sClient.Create(ctx, alpine)).To(Succeed())
		fourth := &postgresv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "pbka-backup-" + suffix, Namespace: ns},
			Spec:       physicalBackupSpec(alpine.Name),
		}
		Expect(k8sClient.Create(ctx, fourth)).To(Succeed())
		expectInvalid(fourth, "Debian trixie variant")
	})

	It("leaves the pod of a Cluster with an unsupported image alone", func() {
		cluster := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "pbku-" + suffix, Namespace: ns},
			Spec:       postgresv1alpha1.ClusterSpec{Image: "postgres:17-alpine", Replicas: 1, Port: 5432},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		defer func() {
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)
			cluster.Finalizers = nil
			_ = k8sClient.Update(ctx, cluster)
			_ = k8sClient.Delete(ctx, cluster)
		}()
		backup := &postgresv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "pbku-backup-" + suffix, Namespace: ns},
			Spec:       physicalBackupSpec(cluster.Name),
		}
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		r := &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		Expect(err).NotTo(HaveOccurred())
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("postgres:17-alpine"))
		Expect(sts.Spec.Template.Spec.Containers[0].Args).To(BeEmpty(), "no archive_command without pgBackRest")
	})

	It("reports WAL archiving health from pg_stat_archiver", func() {
		cluster := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "pbkh-" + suffix, Namespace: ns},
			Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: cluster.Name + "-credentials", Namespace: ns},
			Data:       map[string][]byte{SecretKeyUsername: []byte("pgop_operator"), SecretKeyPassword: []byte("pw")},
		})).To(Succeed())
		backup := &postgresv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "pbkh-backup-" + suffix, Namespace: ns},
			Spec:       physicalBackupSpec(cluster.Name),
		}
		stats := postgres.ArchiverStats{ArchivedCount: 1, LastArchivedTime: time.Now().Add(-time.Hour),
			FailedCount: 4, LastFailedWAL: "000000010000000000000007", LastFailedTime: time.Now()}
		r := &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			ConnectArchiveServer: func(context.Context, postgres.ConnectionConfig) (ArchiveServer, error) {
				return fakeArchiveServer{stats: stats}, nil
			}}
		Expect(r.reconcileArchiveHealth(ctx, cluster, backup, true)).To(Equal(archiveHealthInterval))
		c := meta.FindStatusCondition(cluster.Status.Conditions, ConditionTypeWALArchiving)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Message).To(ContainSubstring("000000010000000000000007"))

		By("the Backup mirrors the condition")
		mirrorArchivingCondition(backup, cluster)
		Expect(meta.IsStatusConditionFalse(backup.Status.Conditions, ConditionTypeWALArchiving)).To(BeTrue())

		By("the condition goes away without a physical Backup")
		r.reconcileArchiveHealth(ctx, cluster, nil, true)
		Expect(meta.FindStatusCondition(cluster.Status.Conditions, ConditionTypeWALArchiving)).To(BeNil())
	})
})

type fakeArchiveServer struct{ stats postgres.ArchiverStats }

func (f fakeArchiveServer) ArchiverStats(context.Context) (postgres.ArchiverStats, error) {
	return f.stats, nil
}
func (f fakeArchiveServer) Close() error { return nil }
