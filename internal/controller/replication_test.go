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
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

const (
	testReplicationNamespace = "default"
	testStandby1Addr         = "10.0.0.11:5432"
	testStandby2Addr         = "10.0.0.12:5432"
)

var _ = Describe("Replication helpers", func() {
	cluster := func(replicas int32, tls *postgresv1alpha1.ClusterTLSSpec) *postgresv1alpha1.Cluster {
		return &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "ns"},
			Spec:       postgresv1alpha1.ClusterSpec{Replicas: replicas, TLS: tls},
		}
	}

	It("parses only canonical ordinals", func() {
		c := cluster(3, nil)
		for name, want := range map[string]int{"pg-0": 0, "pg-1": 1, "pg-12": 12} {
			got, ok := podOrdinal(c, name)
			Expect(ok).To(BeTrue(), name)
			Expect(got).To(Equal(want))
		}
		for _, name := range []string{"pg-", "pg-01", "pg-a", "pg-x-1", "other-1", "pg--1", "pg-+1"} {
			_, ok := podOrdinal(c, name)
			Expect(ok).To(BeFalse(), name)
		}
		got, ok := pvcOrdinal(c, "data-pg-2")
		Expect(ok).To(BeTrue())
		Expect(got).To(Equal(2))
		_, ok = pvcOrdinal(c, "data-pg-other-2")
		Expect(ok).To(BeFalse())
		got, ok = slotOrdinal("pgop_replica_4")
		Expect(ok).To(BeTrue())
		Expect(got).To(Equal(4))
		_, ok = slotOrdinal("user_slot")
		Expect(ok).To(BeFalse())
		Expect(podRole(0)).To(Equal(LabelRolePrimary))
		Expect(podRole(2)).To(Equal(LabelRoleReplica))
	})

	It("defaults max_slot_wal_keep_size to a quarter of the volume", func() {
		c := cluster(2, nil)
		Expect(defaultMaxSlotWALKeepSize(c)).To(Equal("256MB"))
		c.Spec.Storage.Size = "20Gi"
		Expect(defaultMaxSlotWALKeepSize(c)).To(Equal("5120MB"))
		c.Spec.Storage.Size = "100Mi"
		Expect(defaultMaxSlotWALKeepSize(c)).To(Equal("64MB"))
		c.Spec.Storage.Size = "bogus"
		Expect(defaultMaxSlotWALKeepSize(c)).To(Equal("64MB"))
	})

	It("builds primary_conninfo against the read-write Service", func() {
		Expect(primaryConnInfo(cluster(2, nil))).To(Equal(
			"host=pg.ns.svc.cluster.local port=5432 user=pgop_replicator passfile=/run/pgop/replication.pgpass " +
				"application_name=$(POD_NAME) sslmode=disable"))
		Expect(primaryConnInfo(cluster(2, &postgresv1alpha1.ClusterTLSSpec{}))).To(HaveSuffix(
			"sslmode=verify-full sslrootcert=/etc/pgop/tls/ca.crt"))
	})

	It("adds hba_file only when spec.tls.requireTLS does not already load it", func() {
		args := replicationServerArgs(cluster(2, nil))
		Expect(args).To(ContainElement("hba_file=/etc/pgop/hba/pg_hba.conf"))
		Expect(args).To(ContainElement("max_slot_wal_keep_size=256MB"))

		tlsArgs := replicationServerArgs(cluster(2, &postgresv1alpha1.ClusterTLSSpec{}))
		Expect(tlsArgs).NotTo(ContainElement(ContainSubstring("hba_file")))

		withParam := cluster(2, nil)
		withParam.Spec.Parameters = map[string]string{"Max_Slot_WAL_Keep_Size": "-1"}
		Expect(replicationServerArgs(withParam)).NotTo(ContainElement(ContainSubstring("max_slot_wal_keep_size")))
	})

	It("renders pg_hba with a replication-only replicator", func() {
		Expect(renderPgHBA(cluster(1, &postgresv1alpha1.ClusterTLSSpec{}), false)).To(Equal(managedPgHBA))

		plain := renderPgHBA(cluster(2, nil), true)
		Expect(plain).To(MatchRegexp(`(?m)^local\s+all\s+all\s+trust$`))
		Expect(plain).To(MatchRegexp(`(?m)^host\s+replication\s+pgop_replicator\s+0\.0\.0\.0/0\s+scram-sha-256$`))
		Expect(plain).To(MatchRegexp(`(?m)^host\s+all\s+pgop_replicator\s+all\s+reject$`))
		Expect(plain).To(MatchRegexp(`(?m)^host\s+all\s+all\s+::/0\s+scram-sha-256$`))
		Expect(plain).NotTo(ContainSubstring("hostssl"))
		// The replicator's reject comes before the catch-all rule.
		Expect(strings.Index(plain, "pgop_replicator  all")).To(BeNumerically("<", strings.Index(plain, "all         all             0.0.0.0/0")))

		tls := renderPgHBA(cluster(2, &postgresv1alpha1.ClusterTLSSpec{}), true)
		Expect(tls).To(MatchRegexp(`(?m)^hostssl\s+replication\s+pgop_replicator\s+::/0\s+scram-sha-256$`))
		Expect(tls).To(MatchRegexp(`(?m)^hostnossl\s+all\s+all\s+all\s+reject$`))
		Expect(tls).To(MatchRegexp(`(?m)^hostnossl\s+replication\s+all\s+all\s+reject$`))
		Expect(tls).NotTo(MatchRegexp(`(?m)^host\s+replication`))

		noRequire := renderPgHBA(cluster(2, &postgresv1alpha1.ClusterTLSSpec{RequireTLS: new(false)}), true)
		Expect(noRequire).To(MatchRegexp(`(?m)^host\s+replication\s+pgop_replicator`))
		Expect(noRequire).NotTo(ContainSubstring("hostnossl"))
	})

	It("guards the password-sync hook against recovery", func() {
		cmd := strings.Join(replicationPasswordSyncLifecycle().PostStart.Exec.Command, " ")
		Expect(cmd).To(ContainSubstring(`[ -e "$PGDATA/standby.signal" ]`))
		Expect(cmd).To(ContainSubstring(`[ -e "$PGDATA/recovery.signal" ]`))
		Expect(cmd).To(ContainSubstring("pg_is_in_recovery()"))
		Expect(cmd).To(ContainSubstring("pg_isready -q -U"))
		// The guard comes before ALTER ROLE.
		Expect(strings.Index(cmd, "pg_is_in_recovery()")).To(BeNumerically("<", strings.Index(cmd, "ALTER ROLE")))
	})

	It("clones standbys through a staging directory and a slot", func() {
		Expect(bootstrapScript).To(ContainSubstring(`slot="pgop_replica_$ordinal"`))
		Expect(bootstrapScript).To(ContainSubstring(`pg_basebackup -D "$staging" -X stream -S "$slot"`))
		Expect(bootstrapScript).To(ContainSubstring(`touch "$staging/standby.signal"`))
		Expect(bootstrapScript).To(ContainSubstring("primary_slot_name"))
		Expect(bootstrapScript).To(ContainSubstring("refusing to initialize an empty primary"))
		Expect(bootstrapScript).NotTo(ContainSubstring(" -R"))
	})

	It("keeps a single instance until the primary runs the replication template", func() {
		c := cluster(3, nil)
		Expect(statefulSetReplicas(c, nil)).To(Equal(int32(3)))

		sts := &appsv1.StatefulSet{}
		sts.Generation = 2
		sts.Spec.Replicas = new(int32(1))
		Expect(statefulSetReplicas(c, sts)).To(Equal(int32(1)))

		sts.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: bootstrapContainerName}}
		sts.Status = appsv1.StatefulSetStatus{ObservedGeneration: 2, CurrentRevision: "a", UpdateRevision: "b", ReadyReplicas: 1}
		Expect(statefulSetReplicas(c, sts)).To(Equal(int32(1)))

		sts.Status.UpdateRevision = "a"
		Expect(statefulSetReplicas(c, sts)).To(Equal(int32(3)))

		sts.Status.ObservedGeneration = 1
		Expect(statefulSetReplicas(c, sts)).To(Equal(int32(1)))

		// Already scaled: further changes apply directly.
		sts.Spec.Replicas = new(int32(2))
		Expect(statefulSetReplicas(c, sts)).To(Equal(int32(3)))
		Expect(statefulSetReplicas(cluster(1, nil), sts)).To(Equal(int32(1)))
	})

	It("adds -ro names to operator-requested certificates only with standbys", func() {
		Expect(serverDNSNames(cluster(1, nil))).NotTo(ContainElement(ContainSubstring("-ro")))
		Expect(serverDNSNames(cluster(2, nil))).To(ContainElements("pg-ro.ns.svc.cluster.local", "pg-ro"))
		Expect(serverDNSNames(cluster(2, nil))[0]).To(Equal("pg.ns.svc.cluster.local"))
	})
})

// fakeReplicationServer stands in for the primary in envtest.
type fakeReplicationServer struct {
	inRecovery  bool
	roleExists  bool
	createdRole []postgres.RoleOptions
	slots       []postgres.ReplicationSlot
	created     []string
	dropped     []string
	standbys    []postgres.StandbyStatus
	err         error
}

func (f *fakeReplicationServer) InRecovery(context.Context) (bool, error) { return f.inRecovery, f.err }
func (f *fakeReplicationServer) RoleExists(context.Context, string) (bool, error) {
	return f.roleExists, f.err
}
func (f *fakeReplicationServer) CreateRole(_ context.Context, name string, opts postgres.RoleOptions) error {
	if name != ReplicationUsername {
		return fmt.Errorf("unexpected role %q", name)
	}
	f.createdRole = append(f.createdRole, opts)
	f.roleExists = true
	return nil
}
func (f *fakeReplicationServer) ReplicationSlots(context.Context) ([]postgres.ReplicationSlot, error) {
	return slices.Clone(f.slots), f.err
}
func (f *fakeReplicationServer) CreatePhysicalReplicationSlot(_ context.Context, name string) error {
	f.created = append(f.created, name)
	f.slots = append(f.slots, postgres.ReplicationSlot{Name: name, Physical: true})
	return nil
}
func (f *fakeReplicationServer) DropReplicationSlot(_ context.Context, name string) error {
	for _, s := range f.slots {
		if s.Name == name && s.Active {
			return errors.New("slot is active")
		}
	}
	f.dropped = append(f.dropped, name)
	f.slots = slices.DeleteFunc(f.slots, func(s postgres.ReplicationSlot) bool { return s.Name == name })
	return nil
}
func (f *fakeReplicationServer) ReplicationStatus(context.Context) ([]postgres.StandbyStatus, error) {
	return f.standbys, f.err
}
func (f *fakeReplicationServer) Close() error { return nil }

var _ = Describe("Cluster replication", func() {
	var (
		name string
		fake *fakeReplicationServer
	)

	BeforeEach(func() {
		name = fmt.Sprintf("repl-%d", time.Now().UnixNano())
		fake = &fakeReplicationServer{}
	})

	key := func(n string) types.NamespacedName {
		return types.NamespacedName{Name: n, Namespace: testReplicationNamespace}
	}
	newCluster := func(replicas int32) *postgresv1alpha1.Cluster {
		return &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testReplicationNamespace},
			Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage, Replicas: replicas},
		}
	}
	create := func(c *postgresv1alpha1.Cluster) {
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			got := &postgresv1alpha1.Cluster{}
			if k8sClient.Get(ctx, key(name), got) == nil {
				got.Finalizers = nil
				_ = k8sClient.Update(ctx, got)
				_ = k8sClient.Delete(ctx, got)
			}
		})
	}
	getCluster := func() *postgresv1alpha1.Cluster {
		c := &postgresv1alpha1.Cluster{}
		Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
		return c
	}
	setReplicas := func(n int32) {
		c := getCluster()
		c.Spec.Replicas = n
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
	}
	getSTS := func() *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
		return sts
	}
	// markRolledOut fakes the StatefulSet controller: every pod runs the
	// current template and is ready.
	markRolledOut := func() {
		sts := getSTS()
		sts.Status.ObservedGeneration = sts.Generation
		sts.Status.Replicas = *sts.Spec.Replicas
		sts.Status.ReadyReplicas = *sts.Spec.Replicas
		sts.Status.CurrentRevision = fmt.Sprintf("rev-%d", sts.Generation)
		sts.Status.UpdateRevision = sts.Status.CurrentRevision
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
	}
	newReconciler := func() *ClusterReconciler {
		return &ClusterReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			ConnectReplicationServer: func(context.Context, postgres.ConnectionConfig) (ReplicationServer, error) {
				return fake, nil
			},
		}
	}
	reconcileOnce := func(r *ClusterReconciler) time.Duration {
		res, err := r.Reconcile(ctx, reconcileRequest(getCluster()))
		Expect(err).NotTo(HaveOccurred())
		return res.RequeueAfter
	}
	getService := func(n string) (*corev1.Service, error) {
		svc := &corev1.Service{}
		return svc, k8sClient.Get(ctx, key(n), svc)
	}
	// createPod creates pod <name>-<ordinal> owned by the StatefulSet, as the
	// StatefulSet controller would.
	createPod := func(ordinal int, ready bool) *corev1.Pod {
		sts := getSTS()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%d", name, ordinal),
				Namespace: testReplicationNamespace,
				Labels: map[string]string{
					LabelAppName:            AppNamePostgresql,
					LabelAppInstance:        name,
					LabelAppManagedBy:       LabelValuePgop,
					LabelStatefulSetPodName: fmt.Sprintf("%s-%d", name, ordinal),
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts.Name, UID: sts.UID,
					Controller: new(true),
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: AppNamePostgresql, Image: DefaultPostgresImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)) })
		if ready {
			pod.Status.PodIP = fmt.Sprintf("10.0.0.%d", ordinal+10)
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		}
		return pod
	}
	createPVC := func(ordinal int) *corev1.PersistentVolumeClaim {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("data-%s-%d", name, ordinal),
				Namespace: testReplicationNamespace,
				Labels:    map[string]string{LabelAppName: AppNamePostgresql, LabelAppInstance: name},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		DeferCleanup(func() {
			// envtest runs no PVC protection controller; drop its finalizer.
			got := &corev1.PersistentVolumeClaim{}
			if k8sClient.Get(ctx, key(pvc.Name), got) == nil {
				got.Finalizers = nil
				_ = k8sClient.Update(ctx, got)
				_ = k8sClient.Delete(ctx, got)
			}
		})
		return pvc
	}
	pvcGone := func(n string) bool {
		got := &corev1.PersistentVolumeClaim{}
		err := k8sClient.Get(ctx, key(n), got)
		return apierrors.IsNotFound(err) || (err == nil && got.DeletionTimestamp != nil)
	}
	condition := func() *metav1.Condition {
		return meta.FindStatusCondition(getCluster().Status.Conditions, ConditionTypeReplicationHealthy)
	}

	Context("validation", func() {
		It("accepts up to 10 instances and rejects more", func() {
			c := newCluster(10)
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
			err := k8sClient.Create(ctx, newCluster(11))
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		})
	})

	It("leaves the pod template of a single-instance Cluster unchanged", func() {
		create(newCluster(1))
		r := newReconciler()
		reconcileOnce(r)

		sts := getSTS()
		Expect(sts.Spec.Template.Spec.InitContainers).To(BeEmpty())
		Expect(sts.Spec.Template.Spec.Volumes).To(BeEmpty())
		c := sts.Spec.Template.Spec.Containers[0]
		Expect(c.Args).To(BeNil())
		Expect(c.Lifecycle).To(Equal(operatorPasswordSyncLifecycle()))
		Expect(c.Env).NotTo(ContainElement(HaveField("Name", envPodName)))
		Expect(sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled).
			To(Equal(appsv1.RetainPersistentVolumeClaimRetentionPolicyType))

		svc, err := getService(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Spec.Selector).To(Equal(map[string]string{
			LabelAppName: AppNamePostgresql, LabelAppInstance: name, LabelStatefulSetPodName: name + "-0",
		}))
		_, err = getService(name + "-ro")
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(k8sClient.Get(ctx, key(name+"-hba"), &corev1.ConfigMap{})).NotTo(Succeed())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(name+"-credentials"), secret)).To(Succeed())
		Expect(secret.Data[SecretKeyReplicationPassword]).To(HaveLen(32))

		got := getCluster()
		Expect(got.Status.CurrentPrimary).To(Equal(name + "-0"))
		Expect(got.Status.ReadOnlyEndpoint).To(BeEmpty())
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionTypeReplicationHealthy)).To(BeNil())

		By("a second reconcile does not touch the StatefulSet")
		rv := sts.ResourceVersion
		reconcileOnce(r)
		Expect(getSTS().ResourceVersion).To(Equal(rv))
	})

	It("creates a replication-enabled StatefulSet and both Services for several instances", func() {
		create(newCluster(3))
		r := newReconciler()
		reconcileOnce(r)

		sts := getSTS()
		Expect(*sts.Spec.Replicas).To(Equal(int32(3)))
		Expect(sts.Spec.Template.Spec.InitContainers).To(HaveLen(1))
		init := sts.Spec.Template.Spec.InitContainers[0]
		Expect(init.Name).To(Equal(bootstrapContainerName))
		Expect(init.Image).To(Equal(DefaultPostgresImage))
		Expect(init.Command).To(Equal([]string{shBin, "-c", bootstrapScript}))
		Expect(init.Env).To(ContainElement(HaveField("Name", "PGPASSWORD")))
		Expect(init.Env).To(ContainElement(corev1.EnvVar{Name: envPGHost, Value: name + ".default.svc.cluster.local"}))
		Expect(init.Env).To(ContainElement(corev1.EnvVar{Name: "PGOP_READ_ONLY_HOST", Value: name + "-ro.default.svc.cluster.local"}))
		Expect(init.VolumeMounts).To(ContainElement(HaveField("Name", dataVolumeName)))

		c := sts.Spec.Template.Spec.Containers[0]
		Expect(c.Args[0]).To(Equal(postgresBinary))
		Expect(c.Args).To(ContainElement("hba_file=/etc/pgop/hba/pg_hba.conf"))
		Expect(c.Args).To(ContainElement(HavePrefix("primary_conninfo=host=" + name + ".default.svc.cluster.local ")))
		Expect(c.Env).To(ContainElement(HaveField("Name", envPodName)))
		Expect(c.Lifecycle).To(Equal(replicationPasswordSyncLifecycle()))
		Expect(c.VolumeMounts).To(ContainElements(HaveField("Name", hbaVolumeName), HaveField("Name", runVolumeName)))
		Expect(sts.Spec.Template.Spec.Volumes).To(ContainElements(HaveField("Name", hbaVolumeName), HaveField("Name", runVolumeName)))

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, key(name+"-hba"), cm)).To(Succeed())
		Expect(cm.Data[hbaFileName]).To(ContainSubstring("pgop_replicator"))

		ro, err := getService(name + "-ro")
		Expect(err).NotTo(HaveOccurred())
		Expect(ro.Spec.Selector).To(HaveKeyWithValue(LabelRole, LabelRoleReplica))
		Expect(ro.Spec.Ports[0].Port).To(Equal(int32(5432)))
		Expect(metav1.IsControlledBy(ro, getCluster())).To(BeTrue())
		rw, err := getService(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(rw.Spec.Selector).To(HaveKeyWithValue(LabelStatefulSetPodName, name+"-0"))
		Expect(rw.Spec.Selector).NotTo(HaveKey(LabelRole))

		Expect(getCluster().Status.ReadOnlyEndpoint).To(Equal(name + "-ro.default.svc.cluster.local:5432"))

		By("a second reconcile does not touch the StatefulSet (no diff against server defaults)")
		rv := getSTS().ResourceVersion
		reconcileOnce(r)
		Expect(getSTS().ResourceVersion).To(Equal(rv))
	})

	It("passes TLS settings to replication and does not load pg_hba twice", func() {
		c := newCluster(2)
		c.Spec.TLS = &postgresv1alpha1.ClusterTLSSpec{SecretName: name + "-tls"}
		create(c)
		// The TLS Secret is missing: the StatefulSet is not created. Build the
		// pod template directly instead.
		layout, err := resolvePostgresLayout(c)
		Expect(err).NotTo(HaveOccurred())
		main := buildPostgresContainer(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-credentials"}},
			DefaultPostgresImage, 5432, corev1.ResourceRequirements{}, layout)
		volumes, mounts := postgresTLSVolumes(c)
		main.VolumeMounts = append(main.VolumeMounts, mounts...)
		main.Args = postgresServerArgs(c)
		inits, volumes := applyReplicationTemplate(c, name+"-credentials", layout, &main, volumes)

		hbaArgs := 0
		for _, a := range main.Args {
			if strings.HasPrefix(a, "hba_file=") {
				hbaArgs++
			}
		}
		Expect(hbaArgs).To(Equal(1))
		hbaVolumes := 0
		for _, v := range volumes {
			if v.Name == hbaVolumeName {
				hbaVolumes++
			}
		}
		Expect(hbaVolumes).To(Equal(1))
		Expect(main.Args).To(ContainElement(ContainSubstring("sslmode=verify-full sslrootcert=/etc/pgop/tls/ca.crt")))
		Expect(inits[0].Env).To(ContainElements(
			corev1.EnvVar{Name: "PGSSLMODE", Value: postgres.SSLModeVerifyFull},
			corev1.EnvVar{Name: "PGSSLROOTCERT", Value: "/etc/pgop/tls/ca.crt"}))
		Expect(inits[0].VolumeMounts).To(ContainElement(HaveField("Name", tlsVolumeName)))
		Expect(renderPgHBA(c, true)).To(ContainSubstring("hostssl   replication"))
	})

	It("narrows the selector of a pre-upgrade Service in place", func() {
		create(newCluster(1))
		old := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testReplicationNamespace},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{LabelAppName: AppNamePostgresql, LabelAppInstance: name},
				Ports:    servicePorts(getCluster()),
			},
		}
		Expect(k8sClient.Create(ctx, old)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, old) })

		reconcileOnce(newReconciler())
		svc, err := getService(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.UID).To(Equal(old.UID))
		Expect(svc.Spec.Selector).To(HaveKeyWithValue(LabelStatefulSetPodName, name+"-0"))
	})

	It("scales up from one instance only after the primary runs the replication template", func() {
		create(newCluster(1))
		r := newReconciler()
		reconcileOnce(r)
		markRolledOut()

		setReplicas(2)
		reconcileOnce(r)
		sts := getSTS()
		Expect(hasReplicationTemplate(sts)).To(BeTrue())
		Expect(*sts.Spec.Replicas).To(Equal(int32(1)), "the primary must restart onto the template first")

		By("the primary rolled out with the replication template")
		markRolledOut()
		reconcileOnce(r)
		Expect(*getSTS().Spec.Replicas).To(Equal(int32(2)))

		By("scaling back to one instance keeps the template (no second restart)")
		setReplicas(1)
		reconcileOnce(r)
		sts = getSTS()
		Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
		Expect(hasReplicationTemplate(sts)).To(BeTrue())
		_, err := getService(name + "-ro")
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(k8sClient.Get(ctx, key(name+"-hba"), &corev1.ConfigMap{})).To(Succeed())
		Expect(condition()).To(BeNil())
	})

	It("labels pods primary and replica", func() {
		create(newCluster(3))
		r := newReconciler()
		reconcileOnce(r)
		p0 := createPod(0, false)
		p1 := createPod(1, false)
		reconcileOnce(r)

		got := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, key(p0.Name), got)).To(Succeed())
		Expect(got.Labels).To(HaveKeyWithValue(LabelRole, LabelRolePrimary))
		Expect(k8sClient.Get(ctx, key(p1.Name), got)).To(Succeed())
		Expect(got.Labels).To(HaveKeyWithValue(LabelRole, LabelRoleReplica))
		Expect(condition().Reason).To(Equal(ReasonWaitingForPrimary))
	})

	It("manages the replication role, slots and health on the primary", func() {
		create(newCluster(3))
		r := newReconciler()
		reconcileOnce(r)
		markRolledOut()
		createPod(0, true)
		createPod(1, true)
		createPod(2, true)
		fake.slots = []postgres.ReplicationSlot{{Name: "user_slot", Physical: false}}

		reconcileOnce(r)
		Expect(fake.createdRole).To(HaveLen(1))
		opts := fake.createdRole[0]
		Expect(opts.Login).To(BeTrue())
		Expect(opts.Replication).To(BeTrue())
		Expect(opts.Superuser).To(BeFalse())
		Expect(opts.ConnectionLimit).To(Equal(int32(-1)))
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(name+"-credentials"), secret)).To(Succeed())
		Expect(opts.Password).To(Equal(string(secret.Data[SecretKeyReplicationPassword])))
		Expect(secret.Annotations).To(HaveKey(AnnotationReplicationPasswordFingerprint))
		Expect(fake.created).To(ConsistOf("pgop_replica_1", "pgop_replica_2"))
		Expect(fake.dropped).To(BeEmpty())
		cond := condition()
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonStandbyNotStreaming))
		Expect(cond.Message).To(ContainSubstring(name + "-1"))

		By("every standby streams")
		fake.standbys = []postgres.StandbyStatus{
			{ApplicationName: name + "-1", State: walSenderStreaming},
			{ApplicationName: name + "-2", State: walSenderStreaming},
			{ApplicationName: "pgop-bootstrap", State: "backup"},
		}
		requeue := reconcileOnce(r)
		Expect(condition().Status).To(Equal(metav1.ConditionTrue))
		Expect(condition().Reason).To(Equal(ReasonStreaming))
		Expect(requeue).To(BeNumerically(">", 0))
		Expect(fake.createdRole).To(HaveLen(1), "the password is not re-sent while the fingerprint matches")
		Expect(fake.created).To(HaveLen(2))
		got := getCluster()
		Expect(got.Status.ReadyInstances).To(Equal(int32(3)))
		Expect(got.Status.Ready).To(BeTrue())

		By("an edited replication password is re-sent")
		Expect(k8sClient.Get(ctx, key(name+"-credentials"), secret)).To(Succeed())
		secret.Data[SecretKeyReplicationPassword] = []byte("changed-password")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		reconcileOnce(r)
		Expect(fake.createdRole).To(HaveLen(2))
		Expect(fake.createdRole[1].Password).To(Equal("changed-password"))
	})

	It("reports an error instead of managing slots on an instance in recovery", func() {
		create(newCluster(2))
		r := newReconciler()
		reconcileOnce(r)
		createPod(0, true)
		fake.inRecovery = true
		reconcileOnce(r)
		Expect(condition().Reason).To(Equal(ReasonReplicationError))
		Expect(condition().Message).To(ContainSubstring("in recovery"))
		Expect(fake.created).To(BeEmpty())
		Expect(fake.createdRole).To(BeEmpty())
	})

	It("removes the volume and slot of a scaled-away standby, never the primary's", func() {
		create(newCluster(3))
		r := newReconciler()
		reconcileOnce(r)
		markRolledOut()
		createPod(0, true)
		createPod(1, true)
		p2 := createPod(2, true)
		pvc0, pvc1, pvc2 := createPVC(0), createPVC(1), createPVC(2)
		reconcileOnce(r)
		Expect(fake.created).To(ConsistOf("pgop_replica_1", "pgop_replica_2"))

		By("scaling to 2 while pod 2 is still running keeps its volume and slot")
		setReplicas(2)
		fake.slots[1].Active = true // pgop_replica_2 still streaming
		reconcileOnce(r)
		Expect(*getSTS().Spec.Replicas).To(Equal(int32(2)))
		Expect(pvcGone(pvc2.Name)).To(BeFalse())
		Expect(fake.dropped).To(BeEmpty())

		By("once pod 2 is gone its volume and then its slot are removed")
		Expect(k8sClient.Delete(ctx, p2, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key(p2.Name), &corev1.Pod{}))
		}).Should(BeTrue())
		fake.slots[1].Active = false
		reconcileOnce(r)
		Expect(pvcGone(pvc2.Name)).To(BeTrue())
		Expect(pvcGone(pvc1.Name)).To(BeFalse())
		Expect(pvcGone(pvc0.Name)).To(BeFalse())
		Expect(fake.dropped).To(ConsistOf("pgop_replica_2"))

		By("scaling to a single instance never touches the primary's volume")
		setReplicas(1)
		reconcileOnce(r)
		Expect(pvcGone(pvc0.Name)).To(BeFalse())
		Expect(pvcGone(pvc1.Name)).To(BeFalse(), "pod 1 is still running")
		Expect(fake.created).To(HaveLen(2))
	})

	Context("standbys and reloads", func() {
		var pods []corev1.Pod
		BeforeEach(func() {
			create(newCluster(4))
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-credentials", Namespace: testReplicationNamespace},
				Data: map[string][]byte{
					SecretKeyUsername: []byte(DefaultOperatorUsername), SecretKeyPassword: []byte("pw"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })
			pod := func(ordinal int, ready bool) corev1.Pod {
				p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", name, ordinal)}}
				p.Status.PodIP = fmt.Sprintf("10.0.0.%d", 10+ordinal)
				if ready {
					p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				}
				return p
			}
			pods = []corev1.Pod{pod(0, true), pod(1, true), pod(2, true), pod(3, false)}
		})

		It("lists only ready standbys by pod address", func() {
			Expect(readyStandbyAddresses(getCluster(), pods)).To(Equal(map[string]string{
				name + "-1": testStandby1Addr,
				name + "-2": testStandby2Addr,
			}))
		})

		It("reloads standbys that present an outdated certificate", func() {
			host := name + ".default.svc.cluster.local"
			ca := newTestCA()
			current, old := ca.issueFor(host), ca.issueFor(host)
			material, err := parseTLSMaterial(current, host, time.Now())
			Expect(err).NotTo(HaveOccurred())
			presented := map[string][]byte{
				testStandby1Addr: leafDER(current),
				testStandby2Addr: leafDER(old),
			}
			probe := func(_ context.Context, addr, serverName string) ([]byte, error) {
				Expect(serverName).To(Equal(host))
				return presented[addr], nil
			}
			var reloads []postgres.ConnectionConfig
			r := &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
				ReloadServerConfig: func(_ context.Context, cfg postgres.ConnectionConfig) error {
					reloads = append(reloads, cfg)
					return nil
				}}

			pending, msg := r.reconcileStandbyCertificates(ctx, getCluster(), material, pods, probe)
			Expect(pending).To(BeTrue())
			Expect(msg).To(ContainSubstring(name + "-2"))
			Expect(msg).NotTo(ContainSubstring(name + "-1 "))
			Expect(reloads).To(HaveLen(1))
			Expect(reloads[0].DialAddress).To(Equal(testStandby2Addr))
			Expect(reloads[0].Host).To(Equal(host))
			Expect(reloads[0].SSLMode).To(Equal(postgres.SSLModeVerifyFull))

			By("all standbys present the current certificate")
			presented[testStandby2Addr] = leafDER(current)
			pending, _ = r.reconcileStandbyCertificates(ctx, getCluster(), material, pods, probe)
			Expect(pending).To(BeFalse())

			By("a certificate from another CA is not reloaded over an unverified connection")
			presented[testStandby2Addr] = leafDER(newTestCA().issueFor(host))
			pending, msg = r.reconcileStandbyCertificates(ctx, getCluster(), material, pods, probe)
			Expect(pending).To(BeTrue())
			Expect(msg).To(ContainSubstring("does not verify"))
			Expect(reloads).To(HaveLen(1))
		})

		It("reloads spec.parameters on the primary and every ready standby", func() {
			var dialed []string
			r := &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
				ConnectParameterServer: func(_ context.Context, cfg postgres.ConnectionConfig) (ParameterServer, error) {
					dialed = append(dialed, cfg.DialAddress)
					return &fakeParameterServer{}, nil
				}}
			Expect(r.reloadForParameters(ctx, getCluster(), pods)).To(Succeed())
			Expect(dialed).To(Equal([]string{"", testStandby1Addr, testStandby2Addr}))
		})
	})
})
