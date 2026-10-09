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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/lib/pq"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Streaming replication (spec.replicas > 1, issue #26 milestone 1).
//
// Pod <cluster>-0 is the primary; pods 1..N-1 are asynchronous hot standbys.
// The bootstrap init container clones a standby from the primary with
// pg_basebackup over a dedicated replication slot; afterwards the standby
// streams from the "<cluster>" Service (primary_conninfo), which only ever
// routes to the primary. The operator manages the replication role, the
// slots, the pod role labels, the read-only Service and the data volumes of
// scaled-away standbys. There is no automated failover.
//
// A single-instance Cluster that never had more than one instance keeps
// exactly the pod template it had before replication existed, so upgrading
// the operator does not restart it. The replication settings are added to the
// pod template on the first scale-up (restarting the primary once) and are
// kept when scaling back to one instance, so later scaling never restarts the
// primary.

const (
	// bootstrapContainerName is the init container that clones standbys. Its
	// presence in the pod template marks a StatefulSet as replication-enabled.
	bootstrapContainerName = "pgop-bootstrap"

	// runVolumeName is an emptyDir holding the replication password file the
	// standby's WAL receiver reads (passfile= in primary_conninfo).
	runVolumeName       = "pgop-run"
	runMountPath        = "/run/pgop"
	replicationPassFile = runMountPath + "/replication.pgpass"

	// replicationSlotPrefix + ordinal is the physical slot of each standby.
	replicationSlotPrefix = "pgop_replica_"
	// readOnlyServiceSuffix is appended to the Cluster name for the Service
	// that routes to the standbys.
	readOnlyServiceSuffix = "-ro"

	envPodName = "POD_NAME"

	// minMaxSlotWALKeepSizeMB is the floor of the default
	// max_slot_wal_keep_size.
	minMaxSlotWALKeepSizeMB = 64

	// replicationCheckInterval is how often a Cluster with standbys is
	// checked again (standbys that stop streaming are not watchable).
	replicationCheckInterval = 30 * time.Second
	// walSenderStreaming is the pg_stat_replication state of a standby that
	// streams.
	walSenderStreaming = "streaming"

	// pg_hba.conf keywords.
	hbaAll         = "all"
	hbaReplication = "replication"
	hbaScram       = "scram-sha-256"
	hbaReject      = "reject"

	// replicationRetryInterval is the retry delay while replication cannot be
	// set up or is not streaming yet.
	replicationRetryInterval = 10 * time.Second

	// maxWALSenders and maxReplicationSlots are set on every
	// replication-enabled instance (standbys need at least the primary's
	// values). They are fixed, not derived from spec.replicas, so scaling
	// never restarts the pods, and leave room for two concurrent clones per
	// standby at the maximum of 10 instances.
	maxWALSenders       = 32
	maxReplicationSlots = 32

	// initializedMarkerKey is a key of the pg_hba ConfigMap, mounted into the
	// bootstrap init container: once present, the primary refuses to
	// initialize an empty data directory (see markInitialized).
	initializedMarkerKey   = "pgop-initialized"
	initializedMarkerValue = "The Cluster holds data; pgop refuses to initialize an empty primary data directory.\n"

	// pqCodeInvalidPassword is SQLSTATE 28P01 (invalid_password).
	pqCodeInvalidPassword = "28P01"
)

// desiredReplicas returns spec.replicas, defaulting to 1.
func desiredReplicas(cluster *postgresv1alpha1.Cluster) int32 {
	if cluster.Spec.Replicas < 1 {
		return 1
	}
	return cluster.Spec.Replicas
}

// hasReplicationTemplate reports whether the StatefulSet's pod template
// already carries the replication settings (see replicationEnabled).
func hasReplicationTemplate(sts *appsv1.StatefulSet) bool {
	if sts == nil {
		return false
	}
	return slices.ContainsFunc(sts.Spec.Template.Spec.InitContainers,
		func(c corev1.Container) bool { return c.Name == bootstrapContainerName })
}

// replicationEnabled reports whether the pod template must carry the
// replication settings: while the Cluster has more than one instance, and,
// once added, for good (so scaling back to one instance does not restart the
// primary). sts is the existing StatefulSet or nil.
func replicationEnabled(cluster *postgresv1alpha1.Cluster, sts *appsv1.StatefulSet) bool {
	return desiredReplicas(cluster) > 1 || hasReplicationTemplate(sts)
}

// primaryPodName is the pod that runs the primary.
func primaryPodName(cluster *postgresv1alpha1.Cluster) string {
	return cluster.Name + "-0"
}

// readOnlyServiceName is the Service that routes to the standbys.
func readOnlyServiceName(cluster *postgresv1alpha1.Cluster) string {
	return cluster.Name + readOnlyServiceSuffix
}

// readOnlyHost is the in-cluster DNS name of the read-only Service.
func readOnlyHost(cluster *postgresv1alpha1.Cluster) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", readOnlyServiceName(cluster), cluster.Namespace)
}

// replicationSlotName is the physical replication slot of the standby with
// the given StatefulSet ordinal.
func replicationSlotName(ordinal int) string {
	return replicationSlotPrefix + strconv.Itoa(ordinal)
}

// parseOrdinal parses the ordinal suffix of name after prefix, accepting
// only the canonical decimal form StatefulSets use.
func parseOrdinal(name, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 || strconv.Itoa(n) != rest {
		return 0, false
	}
	return n, true
}

// podOrdinal returns the StatefulSet ordinal of one of the Cluster's pods.
func podOrdinal(cluster *postgresv1alpha1.Cluster, podName string) (int, bool) {
	return parseOrdinal(podName, cluster.Name+"-")
}

// pvcOrdinal returns the StatefulSet ordinal of one of the Cluster's data
// PVCs.
func pvcOrdinal(cluster *postgresv1alpha1.Cluster, pvcName string) (int, bool) {
	return parseOrdinal(pvcName, dataVolumeName+"-"+cluster.Name+"-")
}

// slotOrdinal returns the ordinal of an operator-managed replication slot.
func slotOrdinal(slot string) (int, bool) {
	return parseOrdinal(slot, replicationSlotPrefix)
}

// podRole is the role label value of the pod with the given ordinal. In
// milestone 1 the primary is always ordinal 0.
func podRole(ordinal int) string {
	if ordinal == 0 {
		return LabelRolePrimary
	}
	return LabelRoleReplica
}

// hasParameter reports whether spec.parameters sets name (case-insensitive,
// as PostgreSQL compares names).
func hasParameter(params map[string]string, name string) bool {
	for k := range params {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// defaultMaxSlotWALKeepSize is the max_slot_wal_keep_size used unless
// spec.parameters sets it: a quarter of the data volume, so a standby that is
// down for long cannot fill the primary's disk with retained WAL (its slot is
// invalidated instead and the standby must be re-cloned).
func defaultMaxSlotWALKeepSize(cluster *postgresv1alpha1.Cluster) string {
	size := cluster.Spec.Storage.Size
	if size == "" {
		size = defaultStorageSize
	}
	mb := int64(minMaxSlotWALKeepSizeMB)
	if q, err := resource.ParseQuantity(size); err == nil {
		mb = max(mb, q.Value()/4/(1024*1024))
	}
	return fmt.Sprintf("%dMB", mb)
}

// replicationSSLMode is the sslmode of replication connections: verify-full
// against the Cluster's CA when spec.tls is set, otherwise disable (the
// server does not serve TLS).
func replicationSSLMode(cluster *postgresv1alpha1.Cluster) string {
	if cluster.Spec.TLS != nil {
		return postgres.SSLModeVerifyFull
	}
	return postgres.SSLModeDisable
}

// primaryConnInfo is the primary_conninfo of every instance (ignored by the
// primary). It names the read-write Service, so standbys follow the primary
// wherever it runs. The password comes from the passfile the bootstrap init
// container writes, and application_name is the pod name (expanded by the
// kubelet from $(POD_NAME)) so pg_stat_replication identifies each standby.
func primaryConnInfo(cluster *postgresv1alpha1.Cluster) string {
	parts := []string{
		"host=" + clusterHost(cluster),
		"port=" + strconv.Itoa(int(clusterPort(cluster))),
		"user=" + ReplicationUsername,
		"passfile=" + replicationPassFile,
		"application_name=$(" + envPodName + ")",
		"sslmode=" + replicationSSLMode(cluster),
	}
	if cluster.Spec.TLS != nil {
		parts = append(parts, "sslrootcert="+tlsMountPath+"/"+TLSSecretKeyCA)
	}
	return strings.Join(parts, " ")
}

// replicationServerArgs are the server options added for replication: the
// managed pg_hba (unless spec.tls.requireTLS already loads it),
// primary_conninfo, and the default max_slot_wal_keep_size.
func replicationServerArgs(cluster *postgresv1alpha1.Cluster) []string {
	var args []string
	if !tlsRequired(cluster) {
		args = append(args, "-c", hbaFileArg())
	}
	args = append(args,
		"-c", "primary_conninfo="+primaryConnInfo(cluster),
		"-c", fmt.Sprintf("max_wal_senders=%d", maxWALSenders),
		"-c", fmt.Sprintf("max_replication_slots=%d", maxReplicationSlots))
	if !hasParameter(cluster.Spec.Parameters, "max_slot_wal_keep_size") {
		args = append(args, "-c", "max_slot_wal_keep_size="+defaultMaxSlotWALKeepSize(cluster))
	}
	return args
}

// replicationVolumes are the pod volumes and postgres container mounts added
// for replication: the managed pg_hba (unless spec.tls.requireTLS already
// mounts it) and the emptyDir holding the replication password file.
func replicationVolumes(cluster *postgresv1alpha1.Cluster) ([]corev1.Volume, []corev1.VolumeMount) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if !tlsRequired(cluster) {
		volumes = append(volumes, hbaVolume(cluster))
		mounts = append(mounts, hbaVolumeMount())
	}
	volumes = append(volumes, corev1.Volume{
		Name:         runVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	mounts = append(mounts, runVolumeMount())
	return volumes, mounts
}

func runVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: runVolumeName, MountPath: runMountPath}
}

// podNameEnv exposes the pod name to the containers.
func podNameEnv() corev1.EnvVar {
	return corev1.EnvVar{
		Name: envPodName,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"},
		},
	}
}

// renderPgHBA renders the operator-managed pg_hba.conf. Without replication
// it is managedPgHBA (spec.tls.requireTLS), unchanged. With replication,
// pgop_replicator may only open replication connections (with SCRAM, and
// over TLS when requireTLS is set); every other TCP connection keeps the
// rules it had.
func renderPgHBA(cluster *postgresv1alpha1.Cluster, replication bool) string {
	requireTLS := tlsRequired(cluster)
	if !replication {
		return managedPgHBA
	}
	host, managedBy := "host", "streaming replication"
	if requireTLS {
		host, managedBy = "hostssl", "streaming replication, spec.tls.requireTLS"
	}
	rows := [][]string{
		{"local", hbaAll, hbaAll, "", "trust"},
		{"local", hbaReplication, hbaAll, "", "trust"},
		{host, hbaReplication, ReplicationUsername, "0.0.0.0/0", hbaScram},
		{host, hbaReplication, ReplicationUsername, "::/0", hbaScram},
		{"host", hbaAll, ReplicationUsername, hbaAll, hbaReject},
		{host, hbaAll, hbaAll, "0.0.0.0/0", hbaScram},
		{host, hbaAll, hbaAll, "::/0", hbaScram},
	}
	if requireTLS {
		rows = append(rows,
			[]string{"hostnossl", hbaAll, hbaAll, hbaAll, hbaReject},
			[]string{"hostnossl", hbaReplication, hbaAll, hbaAll, hbaReject})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by pgop (%s). Changes are overwritten.\n", managedBy)
	fmt.Fprintf(&b, "%-9s %-11s %-15s %-9s %s\n", "# TYPE", "DATABASE", "USER", "ADDRESS", "METHOD")
	for _, r := range rows {
		b.WriteString(strings.TrimRight(fmt.Sprintf("%-9s %-11s %-15s %-9s %s", r[0], r[1], r[2], r[3], r[4]), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// bootstrapScript runs in the bootstrap init container of every pod:
//
//   - it writes the replication password file for primary_conninfo;
//   - a data directory that already exists is used as it is;
//   - the primary (ordinal 0) with an empty data directory is initialized by
//     the image entrypoint (initdb), unless standbys of the Cluster are still
//     running: then the primary's volume was lost, and starting an empty
//     primary would discard the data the standbys hold;
//   - a standby with an empty data directory is cloned from the primary with
//     pg_basebackup over its replication slot, retried until it succeeds.
//
// The clone is written to a staging directory inside PGDATA and only moved
// into place when complete, so an interrupted clone is detected (the staging
// directory exists) and redone instead of starting on partial data.
const bootstrapScript = `set -eu
ordinal="${POD_NAME##*-}"
staging="$PGDATA/pgop-basebackup.tmp"
mkdir -p "$PGDATA" "$(dirname "$PGOP_PASSFILE")"
chmod 0700 "$PGDATA" 2>/dev/null || true
umask 077
printf '*:*:*:%s:%s\n' "$PGUSER" "$(printf '%s' "$PGPASSWORD" | sed -e 's/[\\:]/\\&/g')" > "$PGOP_PASSFILE"
if [ "$ordinal" != 0 ] && [ -e "$staging" ]; then
  echo "pgop: removing an incomplete clone of the primary"
  find "$PGDATA" -mindepth 1 -maxdepth 1 ! -name lost+found -exec rm -rf {} +
fi
if [ -s "$PGDATA/PG_VERSION" ]; then
  echo "pgop: using the existing data directory $PGDATA"
  exit 0
fi
if [ "$ordinal" = 0 ]; then
  if [ -e "$PGOP_INITIALIZED_MARKER" ]; then
    echo "pgop: refusing to initialize an empty primary data directory: this Cluster already held data, so the primary's volume was lost or replaced." >&2
    echo "pgop: restore the primary's volume, or set the annotation pgop.ruck.io/allow-primary-init=true on the Cluster to start over with an empty database" >&2
    exit 1
  fi
  if PGSSLMODE=prefer pg_isready -q -h "$PGOP_READ_ONLY_HOST" -p "$PGPORT" -t 5; then
    echo "pgop: refusing to initialize an empty primary data directory while standbys of this Cluster are running;" >&2
    echo "pgop: restore the primary's volume, or scale the Cluster to 1 instance to start over" >&2
    exit 1
  fi
  exit 0
fi
slot="pgop_replica_$ordinal"
echo "pgop: cloning the primary $PGHOST into $PGDATA (replication slot $slot)"
until pg_basebackup -D "$staging" -X stream -S "$slot" -c fast --no-password; do
  echo "pgop: cloning the primary failed; retrying in 5s"
  rm -rf "$staging"
  sleep 5
done
touch "$staging/standby.signal"
printf "primary_slot_name = '%s'\n" "$slot" >> "$staging/postgresql.auto.conf"
find "$staging" -mindepth 1 -maxdepth 1 -exec mv {} "$PGDATA"/ \;
rmdir "$staging"
echo "pgop: standby data directory ready"
`

// bootstrapInitContainer builds the init container running bootstrapScript.
// It uses the Cluster's image (pg_basebackup matches the server version) and
// the same data-directory layout and security context as the postgresql
// container.
func bootstrapInitContainer(cluster *postgresv1alpha1.Cluster, secretName string, main corev1.Container, layout postgresLayout) corev1.Container {
	env := []corev1.EnvVar{
		podNameEnv(),
		{Name: "PGDATA", Value: layout.PGDATA},
		{Name: envPGHost, Value: clusterHost(cluster)},
		{Name: envPGPort, Value: strconv.Itoa(int(clusterPort(cluster)))},
		{Name: "PGUSER", Value: ReplicationUsername},
		secretEnv("PGPASSWORD", secretName, SecretKeyReplicationPassword),
		{Name: "PGSSLMODE", Value: replicationSSLMode(cluster)},
		{Name: "PGCONNECT_TIMEOUT", Value: "10"},
		{Name: "PGAPPNAME", Value: "pgop-bootstrap"},
		{Name: "PGOP_READ_ONLY_HOST", Value: readOnlyHost(cluster)},
		{Name: "PGOP_PASSFILE", Value: replicationPassFile},
		{Name: "PGOP_INITIALIZED_MARKER", Value: hbaMountPath + "/" + initializedMarkerKey},
	}
	mounts := []corev1.VolumeMount{{Name: dataVolumeName, MountPath: layout.MountPath}}
	if cluster.Spec.TLS != nil {
		env = append(env, corev1.EnvVar{Name: "PGSSLROOTCERT", Value: tlsMountPath + "/" + TLSSecretKeyCA})
		mounts = append(mounts, tlsVolumeMount())
	}
	mounts = append(mounts, hbaVolumeMount(), runVolumeMount())
	return corev1.Container{
		Name:            bootstrapContainerName,
		Image:           main.Image,
		Command:         []string{shBin, "-c", bootstrapScript},
		Env:             env,
		VolumeMounts:    mounts,
		Resources:       main.Resources,
		SecurityContext: main.SecurityContext,
	}
}

// replicationPasswordSyncLifecycle is operatorPasswordSyncLifecycle for
// replication-enabled pods: it does nothing on an instance in recovery (a
// standby, or a server recovering a backup), where ALTER ROLE would fail and
// make the kubelet kill the container. A standby gets the password from the
// primary through replication.
func replicationPasswordSyncLifecycle() *corev1.Lifecycle {
	script := `if [ -e "$PGDATA/standby.signal" ] || [ -e "$PGDATA/recovery.signal" ]; then exit 0; fi; ` +
		`until pg_isready -q -U "$POSTGRES_USER" -d postgres 2>/dev/null; do sleep 1; done; ` +
		`if [ "$(psql -U "$POSTGRES_USER" -d postgres -tAc 'SELECT pg_is_in_recovery()')" = t ]; then exit 0; fi; ` +
		`psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d postgres ` +
		`-c "ALTER ROLE \"$POSTGRES_USER\" WITH PASSWORD '$POSTGRES_PASSWORD'"`
	return &corev1.Lifecycle{
		PostStart: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{Command: []string{"sh", "-c", script}},
		},
	}
}

// applyReplicationTemplate adds the replication settings to the postgresql
// container and the pod: the bootstrap init container, the recovery-aware
// postStart hook, $(POD_NAME), the server options and the volumes.
func applyReplicationTemplate(cluster *postgresv1alpha1.Cluster, secretName string, layout postgresLayout,
	container *corev1.Container, volumes []corev1.Volume) ([]corev1.Container, []corev1.Volume) {
	replVolumes, replMounts := replicationVolumes(cluster)
	container.Env = append(container.Env, podNameEnv())
	container.Lifecycle = replicationPasswordSyncLifecycle()
	container.VolumeMounts = append(container.VolumeMounts, replMounts...)
	args := container.Args
	if len(args) == 0 {
		args = []string{postgresBinary}
	}
	container.Args = append(args, replicationServerArgs(cluster)...)
	init := bootstrapInitContainer(cluster, secretName, *container, layout)
	return []corev1.Container{init}, append(volumes, replVolumes...)
}

// replicationRolledOut reports whether the StatefulSet already runs the
// replication-enabled pod template on every pod. The primary must allow
// replication connections (managed pg_hba) before the first standby is
// created; otherwise the StatefulSet controller would wait forever for the
// standby to become ready before updating the primary.
func replicationRolledOut(sts *appsv1.StatefulSet) bool {
	return hasReplicationTemplate(sts) &&
		sts.Status.ObservedGeneration >= sts.Generation &&
		sts.Status.CurrentRevision != "" &&
		sts.Status.UpdateRevision == sts.Status.CurrentRevision &&
		sts.Status.ReadyReplicas >= 1
}

// statefulSetReplicas is the StatefulSet replica count for the Cluster. When
// a single-instance StatefulSet is scaled up, it stays at its current size
// until the primary runs the replication-enabled template.
func statefulSetReplicas(cluster *postgresv1alpha1.Cluster, existing *appsv1.StatefulSet) int32 {
	want := desiredReplicas(cluster)
	if existing == nil || existing.Spec.Replicas == nil || want <= 1 {
		return want
	}
	cur := *existing.Spec.Replicas
	if cur <= 1 && !replicationRolledOut(existing) {
		// max: also restores a StatefulSet scaled to 0 by hand.
		return max(cur, 1)
	}
	return want
}

// primaryServiceSelector selects the primary pod by the name the StatefulSet
// controller labels it with, so the read-write Service never routes to a
// standby and keeps working without the operator.
func primaryServiceSelector(cluster *postgresv1alpha1.Cluster) map[string]string {
	return map[string]string{
		LabelAppName:            AppNamePostgresql,
		LabelAppInstance:        cluster.Name,
		LabelStatefulSetPodName: primaryPodName(cluster),
	}
}

// readOnlyServiceSelector selects the pods the operator labels as replicas.
func readOnlyServiceSelector(cluster *postgresv1alpha1.Cluster) map[string]string {
	return map[string]string{
		LabelAppName:     AppNamePostgresql,
		LabelAppInstance: cluster.Name,
		LabelRole:        LabelRoleReplica,
		LabelStreaming:   labelValueTrue,
	}
}

// servicePorts are the ports of the Cluster's Services.
func servicePorts(cluster *postgresv1alpha1.Cluster) []corev1.ServicePort {
	port := clusterPort(cluster)
	return []corev1.ServicePort{{
		Name:       AppNamePostgresql,
		Port:       port,
		TargetPort: intstr.FromInt32(port),
		Protocol:   corev1.ProtocolTCP,
	}}
}

// reconcileReadOnlyService creates or converges the "<cluster>-ro" Service
// while the Cluster has more than one instance and deletes it otherwise.
func (r *ClusterReconciler) reconcileReadOnlyService(ctx context.Context, cluster *postgresv1alpha1.Cluster) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: readOnlyServiceName(cluster), Namespace: cluster.Namespace}, svc)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	exists := err == nil
	if desiredReplicas(cluster) <= 1 {
		if exists && metav1.IsControlledBy(svc, cluster) {
			return client.IgnoreNotFound(r.Delete(ctx, svc))
		}
		return nil
	}
	if exists {
		if !metav1.IsControlledBy(svc, cluster) {
			return fmt.Errorf("service %q exists and is not owned by the Cluster", svc.Name)
		}
		return r.convergeServiceSpec(ctx, svc, readOnlyServiceSelector(cluster), servicePorts(cluster))
	}
	svc = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      readOnlyServiceName(cluster),
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				LabelAppName:      AppNamePostgresql,
				LabelAppInstance:  cluster.Name,
				LabelAppManagedBy: LabelValuePgop,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: readOnlyServiceSelector(cluster),
			Ports:    servicePorts(cluster),
			Type:     corev1.ServiceTypeClusterIP,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, svc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, svc)
}

// convergeServiceSpec updates a Service's selector and ports when they
// differ from the desired ones.
func (r *ClusterReconciler) convergeServiceSpec(ctx context.Context, svc *corev1.Service, selector map[string]string, ports []corev1.ServicePort) error {
	if apiequality.Semantic.DeepEqual(svc.Spec.Selector, selector) && apiequality.Semantic.DeepEqual(svc.Spec.Ports, ports) {
		return nil
	}
	svc.Spec.Selector = selector
	svc.Spec.Ports = ports
	return r.Update(ctx, svc)
}

// listClusterPods returns the Cluster's PostgreSQL pods.
func (r *ClusterReconciler) listClusterPods(ctx context.Context, cluster *postgresv1alpha1.Cluster) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		LabelAppName:     AppNamePostgresql,
		LabelAppInstance: cluster.Name,
	}); err != nil {
		return nil, err
	}
	out := pods.Items[:0]
	for _, p := range pods.Items {
		ref := metav1.GetControllerOf(&p)
		if ref == nil || ref.Kind != "StatefulSet" || ref.Name != cluster.Name {
			continue
		}
		if _, ok := podOrdinal(cluster, p.Name); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// labelPods sets the role label (primary/replica) on the Cluster's pods. The
// StatefulSet pod template cannot carry per-pod labels, so the operator sets
// them; the "<cluster>-ro" Service selects role=replica.
func (r *ClusterReconciler) labelPods(ctx context.Context, cluster *postgresv1alpha1.Cluster, pods []corev1.Pod) error {
	for i := range pods {
		pod := &pods[i]
		ordinal, _ := podOrdinal(cluster, pod.Name)
		role := podRole(ordinal)
		if pod.Labels[LabelRole] == role || pod.DeletionTimestamp != nil {
			continue
		}
		base := pod.DeepCopy()
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[LabelRole] = role
		if err := r.Patch(ctx, pod, client.MergeFrom(base)); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to label pod %s: %w", pod.Name, err)
		}
	}
	return nil
}

// isPodReady reports whether the pod is ready and not being deleted.
func isPodReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// readyStandbyAddresses returns the pod IP:port of every ready standby,
// keyed by pod name.
func readyStandbyAddresses(cluster *postgresv1alpha1.Cluster, pods []corev1.Pod) map[string]string {
	out := map[string]string{}
	for i := range pods {
		ordinal, _ := podOrdinal(cluster, pods[i].Name)
		if ordinal == 0 || !isPodReady(&pods[i]) || pods[i].Status.PodIP == "" {
			continue
		}
		out[pods[i].Name] = net.JoinHostPort(pods[i].Status.PodIP, strconv.Itoa(int(clusterPort(cluster))))
	}
	return out
}

// ReplicationServer is the PostgreSQL access needed to manage replication on
// the primary. *postgres.Client implements it.
type ReplicationServer interface {
	InRecovery(ctx context.Context) (bool, error)
	RoleExists(ctx context.Context, name string) (bool, error)
	CreateRole(ctx context.Context, name string, opts postgres.RoleOptions) error
	ReplicationSlots(ctx context.Context) ([]postgres.ReplicationSlot, error)
	CreatePhysicalReplicationSlot(ctx context.Context, name string) error
	DropReplicationSlot(ctx context.Context, name string) error
	ReplicationStatus(ctx context.Context) ([]postgres.StandbyStatus, error)
	Close() error
}

func connectReplicationServer(_ context.Context, cfg postgres.ConnectionConfig) (ReplicationServer, error) {
	return postgres.NewClient(cfg)
}

// replicationPasswordFingerprint is the salted fingerprint of the replication
// password recorded on the credentials Secret once it is set in PostgreSQL.
func replicationPasswordFingerprint(cluster *postgresv1alpha1.Cluster, password string) string {
	sum := sha256.Sum256([]byte(string(cluster.UID) + "\x00replication\x00" + password))
	return hex.EncodeToString(sum[:])
}

// setReplicationCondition sets ReplicationHealthy on the in-memory Cluster.
func setReplicationCondition(cluster *postgresv1alpha1.Cluster, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReplicationHealthy,
		Status:             status,
		ObservedGeneration: cluster.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// reconcileReplication manages replication for a replication-enabled
// Cluster: it removes the data PVCs of scaled-away standbys, and on the
// primary records that the Cluster holds data (see initializedMarkerKey),
// converges the replication role and password, creates the replication slot
// of every standby pod (re-creating invalidated ones), drops the slots of
// scaled-away standbys, labels the standbys by whether they stream (only
// streaming standbys are in the read-only Service), and reports whether every
// standby streams (ReplicationHealthy). Problems are reported in the
// condition and never fail the reconcile. Returns when to check again (0: no
// need).
func (r *ClusterReconciler) reconcileReplication(ctx context.Context, cluster *postgresv1alpha1.Cluster,
	secret *corev1.Secret, sts *appsv1.StatefulSet, pods []corev1.Pod) time.Duration {
	want := desiredReplicas(cluster)
	if want <= 1 {
		meta.RemoveStatusCondition(&cluster.Status.Conditions, ConditionTypeReplicationHealthy)
	}
	if !replicationEnabled(cluster, sts) {
		return 0
	}
	log := logf.FromContext(ctx)

	// Slots are only dropped when the PVC listing is complete: a partial
	// result could drop the slot of a standby whose volume still exists.
	pvcs, err := r.cleanupStandbyVolumes(ctx, cluster, sts, pods)
	allowSlotDrops := err == nil
	if err != nil {
		log.Error(err, "Failed to clean up the volumes of removed standbys")
	}

	var primary *corev1.Pod
	for i := range pods {
		if pods[i].Name == primaryPodName(cluster) {
			primary = &pods[i]
		}
	}
	if primary == nil || !isPodReady(primary) {
		if want > 1 {
			setReplicationCondition(cluster, metav1.ConditionFalse, ReasonWaitingForPrimary,
				"Waiting for the primary pod to become ready")
		}
		return replicationRetryInterval
	}

	fail := func(msg string, err error) time.Duration {
		log.Error(err, msg)
		if want > 1 {
			setReplicationCondition(cluster, metav1.ConditionFalse, ReasonReplicationError, fmt.Sprintf("%s: %v", msg, err))
		}
		return replicationRetryInterval
	}

	srv, err := r.connectPrimary(ctx, cluster, secret, primary)
	if err != nil {
		return fail("Cannot connect to the primary", err)
	}
	defer func() { _ = srv.Close() }()

	// The read-write Service must reach the primary; never manage slots on
	// (or report health of) a standby.
	if inRecovery, err := srv.InRecovery(ctx); err != nil {
		return fail("Cannot check the primary", err)
	} else if inRecovery {
		return fail("Cannot manage replication", fmt.Errorf("the instance behind Service %q is in recovery", cluster.Name))
	}
	if err := r.markInitialized(ctx, cluster); err != nil {
		return fail("Cannot record that the Cluster is initialized", err)
	}

	if err := r.ensureReplicationRole(ctx, cluster, secret, srv, pods); err != nil {
		return fail("Cannot set up the replication role", err)
	}
	podExists := map[int]bool{}
	for _, p := range pods {
		if ordinal, ok := podOrdinal(cluster, p.Name); ok {
			podExists[ordinal] = true
		}
	}
	recreated, err := r.reconcileReplicationSlots(ctx, cluster, srv, want, podExists, pvcs, allowSlotDrops)
	if err != nil {
		return fail("Cannot manage replication slots", err)
	}

	if want <= 1 {
		return 0
	}
	standbys, err := srv.ReplicationStatus(ctx)
	if err != nil {
		return fail("Cannot read pg_stat_replication", err)
	}
	streaming := map[string]bool{}
	for _, s := range standbys {
		if s.State == walSenderStreaming {
			streaming[s.ApplicationName] = true
		}
	}
	if err := r.labelStreaming(ctx, cluster, pods, streaming); err != nil {
		return fail("Cannot label the standbys", err)
	}
	return reportStreaming(cluster, want, streaming, recreated)
}

// reportStreaming sets ReplicationHealthy from the standbys that stream and
// returns when to check again.
func reportStreaming(cluster *postgresv1alpha1.Cluster, want int32, streaming map[string]bool, recreated []string) time.Duration {
	var missing []string
	for i := 1; i < int(want); i++ {
		if name := fmt.Sprintf("%s-%d", cluster.Name, i); !streaming[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		msg := fmt.Sprintf("%d of %d standbys streaming; not streaming: %s", int(want)-1-len(missing), want-1,
			strings.Join(missing, ", "))
		if len(recreated) > 0 {
			msg += fmt.Sprintf("; the replication slots of %s were invalidated (max_slot_wal_keep_size) and "+
				"re-created: re-clone these standbys by deleting their data PVC and pod", strings.Join(recreated, ", "))
		}
		setReplicationCondition(cluster, metav1.ConditionFalse, ReasonStandbyNotStreaming, msg)
		return replicationRetryInterval
	}
	setReplicationCondition(cluster, metav1.ConditionTrue, ReasonStreaming,
		fmt.Sprintf("All %d standbys stream from the primary %s", want-1, primaryPodName(cluster)))
	return replicationCheckInterval
}

// connectPrimary connects to the primary as the operator. When the primary
// rejects the operator password, it is restarted once (see
// restartPrimaryForPasswordSync).
func (r *ClusterReconciler) connectPrimary(ctx context.Context, cluster *postgresv1alpha1.Cluster,
	secret *corev1.Secret, primary *corev1.Pod) (ReplicationServer, error) {
	cluster.Status.SecretName = cluster.Name + "-credentials"
	cfg, err := operatorConnectionConfig(ctx, r.Client, cluster, defaultDatabaseName)
	if err != nil {
		return nil, err
	}
	connect := r.ConnectReplicationServer
	if connect == nil {
		connect = connectReplicationServer
	}
	srv, err := connect(ctx, cfg)
	if err != nil {
		if pqErr, ok := errors.AsType[*pq.Error](err); ok && pqErr.Code == pqCodeInvalidPassword {
			r.restartPrimaryForPasswordSync(ctx, cluster, secret, primary)
		}
		return nil, err
	}
	return srv, nil
}

// labelStreaming sets pgop.ruck.io/streaming on the standby pods from
// pg_stat_replication. The read-only Service only selects standbys that
// stream, so a standby that fell out of replication (an invalidated slot,
// data from another primary, a wrong password) stops serving reads instead
// of serving stale data indefinitely.
func (r *ClusterReconciler) labelStreaming(ctx context.Context, cluster *postgresv1alpha1.Cluster,
	pods []corev1.Pod, streaming map[string]bool) error {
	for i := range pods {
		pod := &pods[i]
		if ordinal, _ := podOrdinal(cluster, pod.Name); ordinal == 0 || pod.DeletionTimestamp != nil {
			continue
		}
		want := strconv.FormatBool(streaming[pod.Name])
		if pod.Labels[LabelStreaming] == want {
			continue
		}
		base := pod.DeepCopy()
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[LabelStreaming] = want
		if err := r.Patch(ctx, pod, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to label pod %s: %w", pod.Name, err)
		}
	}
	return nil
}

// restartPrimaryForPasswordSync deletes the primary pod once when the
// operator's password is rejected (28P01) by a primary that is not in
// recovery: the recovery-aware postStart hook skips the password sync while
// a restored server is still recovering, so after a restore the password is
// only synced on the next start. At most one restart per operator password,
// recorded on the credentials Secret, so a password that keeps failing does
// not cause a restart loop.
func (r *ClusterReconciler) restartPrimaryForPasswordSync(ctx context.Context, cluster *postgresv1alpha1.Cluster,
	secret *corev1.Secret, primary *corev1.Pod) {
	fp := replicationPasswordFingerprint(cluster, "operator\x00"+string(secret.Data[SecretKeyPassword]))
	if secret.Annotations[AnnotationPasswordSyncRestart] == fp {
		return
	}
	log := logf.FromContext(ctx)
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[AnnotationPasswordSyncRestart] = fp
	if err := r.Update(ctx, secret); err != nil {
		log.Error(err, "Failed to record the password-sync restart")
		return
	}
	if err := r.Delete(ctx, primary, client.Preconditions{UID: &primary.UID}); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Failed to restart the primary to sync the operator password")
		return
	}
	log.Info("Restarted the primary: it rejects the operator password, which is synced on start")
	if r.Recorder != nil {
		r.Recorder.Eventf(cluster, primary, corev1.EventTypeWarning, "OperatorPasswordRejected", "RestartPrimary",
			"The primary rejects the operator password; restarting it once so the postStart hook syncs the password")
	}
}

// markInitialized records in the pg_hba ConfigMap (mounted into the
// bootstrap init container) that the Cluster holds data, once the operator
// reached a primary that is not in recovery. From then on the primary's
// bootstrap refuses to initialize an empty data directory (its volume was
// lost) instead of silently starting a new, empty database. The annotation
// AnnotationAllowPrimaryInit on the Cluster removes the marker.
func (r *ClusterReconciler) markInitialized(ctx context.Context, cluster *postgresv1alpha1.Cluster) error {
	if cluster.Annotations[AnnotationAllowPrimaryInit] == labelValueTrue {
		return nil
	}
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: hbaConfigMapName(cluster), Namespace: cluster.Namespace}, cm); err != nil {
		return client.IgnoreNotFound(err)
	}
	if _, ok := cm.Data[initializedMarkerKey]; ok || !metav1.IsControlledBy(cm, cluster) {
		return nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[initializedMarkerKey] = initializedMarkerValue
	return r.Update(ctx, cm)
}

// primaryVolumeMissing reports whether standby data PVCs exist while the
// primary's does not: starting the primary on a new, empty volume would
// initialize a new database next to standbys holding the real data.
func (r *ClusterReconciler) primaryVolumeMissing(ctx context.Context, cluster *postgresv1alpha1.Cluster) (bool, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		LabelAppName:     AppNamePostgresql,
		LabelAppInstance: cluster.Name,
	}); err != nil {
		return false, err
	}
	primary, standby := false, false
	for _, pvc := range pvcs.Items {
		if ordinal, ok := pvcOrdinal(cluster, pvc.Name); ok {
			primary = primary || ordinal == 0
			standby = standby || (ordinal > 0 && pvc.DeletionTimestamp == nil)
		}
	}
	return standby && !primary, nil
}

// ensureReplicationRole creates pgop_replicator (LOGIN REPLICATION) on the
// primary and converges its password to the credentials Secret. The password
// is sent as a SCRAM verifier and only when the role is missing or the
// Secret's fingerprint annotation does not match, so it is not re-sent on
// every reconcile. When the password changed, the standby pods are deleted:
// their bootstrap init container writes the password file the WAL receiver
// uses, so they pick up the new password on restart. The primary is not
// restarted.
func (r *ClusterReconciler) ensureReplicationRole(ctx context.Context, cluster *postgresv1alpha1.Cluster,
	secret *corev1.Secret, srv ReplicationServer, pods []corev1.Pod) error {
	password := string(secret.Data[SecretKeyReplicationPassword])
	if password == "" {
		return fmt.Errorf("credentials Secret %q has no %q", secret.Name, SecretKeyReplicationPassword)
	}
	exists, err := srv.RoleExists(ctx, ReplicationUsername)
	if err != nil {
		return err
	}
	fingerprint := replicationPasswordFingerprint(cluster, password)
	recorded := secret.Annotations[AnnotationReplicationPasswordFingerprint]
	if exists && recorded == fingerprint {
		return nil
	}
	if err := srv.CreateRole(ctx, ReplicationUsername, postgres.RoleOptions{
		Login:           true,
		Replication:     true,
		ConnectionLimit: -1,
		Password:        password,
	}); err != nil {
		return err
	}
	if exists && recorded != "" {
		if err := r.restartStandbys(ctx, cluster, pods); err != nil {
			return err
		}
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[AnnotationReplicationPasswordFingerprint] = fingerprint
	return r.Update(ctx, secret)
}

// restartStandbys deletes the standby pods so the StatefulSet recreates
// them (with a fresh replication password file).
func (r *ClusterReconciler) restartStandbys(ctx context.Context, cluster *postgresv1alpha1.Cluster, pods []corev1.Pod) error {
	for i := range pods {
		pod := &pods[i]
		if ordinal, _ := podOrdinal(cluster, pod.Name); ordinal == 0 || pod.DeletionTimestamp != nil {
			continue
		}
		if err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to restart standby %s: %w", pod.Name, err)
		}
		logf.FromContext(ctx).Info("Restarted a standby for the new replication password", "pod", pod.Name)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal, "ReplicationPasswordChanged", "RestartStandbys",
			"The replication password changed; restarted the standbys to load it")
	}
	return nil
}

// reconcileReplicationSlots manages the operator's slots on the primary:
//
//   - the slot of a standby (ordinal 1..want-1) is created once its pod
//     exists, not ahead of time, so a slot never holds back WAL for a standby
//     that is still waiting for its predecessors to start;
//   - an invalidated slot (wal_status "lost") that is not in use is dropped
//     and re-created, so re-cloning the standby works; its pod names are
//     returned (the standby itself must be re-cloned);
//   - the slots of scaled-away standbys are dropped once they are not in use
//     and their data PVC is gone or being deleted (a standby scaled back up
//     before its PVC was removed resumes from its slot). pvcs is the set of
//     ordinals whose data PVC is kept; drops are skipped unless allowDrops.
func (r *ClusterReconciler) reconcileReplicationSlots(ctx context.Context, cluster *postgresv1alpha1.Cluster,
	srv ReplicationServer, want int32, podExists, pvcs map[int]bool, allowDrops bool) ([]string, error) {
	log := logf.FromContext(ctx)
	slots, err := srv.ReplicationSlots(ctx)
	if err != nil {
		return nil, err
	}
	existing := map[string]bool{}
	var recreated []string
	for _, s := range slots {
		ordinal, ok := slotOrdinal(s.Name)
		if !ok || !s.Physical || ordinal == 0 {
			existing[s.Name] = true
			continue
		}
		switch {
		case ordinal >= int(want):
			if !allowDrops || s.Active || pvcs[ordinal] {
				existing[s.Name] = true
				continue
			}
			if err := srv.DropReplicationSlot(ctx, s.Name); err != nil {
				return nil, err
			}
			log.Info("Dropped the replication slot of a removed standby", "slot", s.Name)
		case s.WALStatus == postgres.WALStatusLost && !s.Active:
			if err := srv.DropReplicationSlot(ctx, s.Name); err != nil {
				return nil, err
			}
			pod := fmt.Sprintf("%s-%d", cluster.Name, ordinal)
			recreated = append(recreated, pod)
			log.Info("Dropped an invalidated replication slot", "slot", s.Name)
			if r.Recorder != nil {
				r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, "ReplicationSlotInvalidated", "RecreateSlot",
					"Replication slot %s was invalidated (max_slot_wal_keep_size exceeded) and is re-created; "+
						"standby %s must be re-cloned: delete PVC data-%s and pod %s", s.Name, pod, pod, pod)
			}
		default:
			existing[s.Name] = true
		}
	}
	for i := 1; i < int(want); i++ {
		if name := replicationSlotName(i); !existing[name] && podExists[i] {
			if err := srv.CreatePhysicalReplicationSlot(ctx, name); err != nil {
				return recreated, err
			}
		}
	}
	return recreated, nil
}

// cleanupStandbyVolumes deletes the data PVCs of standbys that were scaled
// away, so a standby that is added again later is cloned afresh instead of
// starting on stale data. The StatefulSet keeps PVCs on scale-down
// (whenScaled=Retain, which also protects the primary's volume from a manual
// "kubectl scale --replicas=0"), so the operator removes them itself: only for
// ordinals >= 1 and >= spec.replicas, only once the StatefulSet no longer
// wants the pod and the pod is gone. Returns the ordinals of the data PVCs
// that still exist.
func (r *ClusterReconciler) cleanupStandbyVolumes(ctx context.Context, cluster *postgresv1alpha1.Cluster,
	sts *appsv1.StatefulSet, pods []corev1.Pod) (map[int]bool, error) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcs, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		LabelAppName:     AppNamePostgresql,
		LabelAppInstance: cluster.Name,
	}); err != nil {
		return nil, err
	}
	podExists := map[int]bool{}
	for _, p := range pods {
		if ordinal, ok := podOrdinal(cluster, p.Name); ok {
			podExists[ordinal] = true
		}
	}
	stsReplicas := desiredReplicas(cluster)
	if sts != nil && sts.Spec.Replicas != nil {
		stsReplicas = max(stsReplicas, *sts.Spec.Replicas)
	}
	remaining := map[int]bool{}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		ordinal, ok := pvcOrdinal(cluster, pvc.Name)
		if !ok {
			continue
		}
		if pvc.DeletionTimestamp != nil && ordinal != 0 {
			continue // already being removed; a new pod would get a fresh PVC
		}
		if ordinal == 0 || ordinal < int(stsReplicas) || podExists[ordinal] {
			remaining[ordinal] = true
			continue
		}
		if err := r.Delete(ctx, pvc, client.Preconditions{UID: &pvc.UID}); err != nil && !apierrors.IsNotFound(err) {
			remaining[ordinal] = true
			return remaining, err
		}
		logf.FromContext(ctx).Info("Deleted the data volume of a removed standby", "pvc", pvc.Name)
		if r.Recorder != nil {
			r.Recorder.Eventf(cluster, pvc, corev1.EventTypeNormal, "StandbyVolumeDeleted", "ScaleDown",
				"Deleted the data volume %s of a removed standby", pvc.Name)
		}
	}
	return remaining, nil
}

// clustersForPod maps one of a Cluster's pods to the Cluster, so pod
// creation, readiness and deletion relabel pods and re-check replication.
func clustersForPod(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[LabelAppName] != AppNamePostgresql || labels[LabelAppManagedBy] != LabelValuePgop || labels[LabelAppInstance] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      labels[LabelAppInstance],
	}}}
}
