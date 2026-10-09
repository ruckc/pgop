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
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// Physical backups with pgBackRest.
//
// Architecture (see docs/user-guide/backups.md):
//
//   - A Cluster that is the clusterRef of a physical Backup runs the
//     Postgres+pgBackRest image. PostgreSQL archives WAL with
//     "pgbackrest archive-push" straight to the Backup's repository
//     (archive_mode/archive_command are operator-owned), and a "pgbackrest"
//     sidecar runs the pgBackRest TLS server and creates the stanza once the
//     primary is up.
//   - The backup CronJobs run pgBackRest as the repository host: they read
//     and write the repository directly and reach the data directory through
//     the TLS server of the primary (pg1-host-type=tls, port 8432 of the
//     Cluster's read-write Service), authenticated with client certificates
//     from a per-Cluster CA the operator manages (pgbackrest_tls.go).
//   - A physical Restore stops the Cluster, runs "pgbackrest restore" in a Job
//     that mounts the primary's data volume, and starts the Cluster again;
//     PostgreSQL then recovers from the archive (restore_command).
//
// The repository settings are passed to pgBackRest as PGBACKREST_*
// environment variables (no configuration file), so the PostgreSQL container,
// the sidecar and the Jobs share them and secrets stay in Secrets.
const (
	// pgbackrestVersion is the pgBackRest version of the images pgop builds
	// (images/pgbackrest/Dockerfile). The Jobs and the PostgreSQL pods must
	// run the same version.
	pgbackrestVersion = "2.59.3"

	// DefaultPgbackrestImage runs the backup and restore Jobs unless
	// spec.physical.image is set.
	DefaultPgbackrestImage = "ghcr.io/ruckc/pgop-pgbackrest:" + pgbackrestVersion
	// pgopPostgresImageRepository is the Postgres+pgBackRest image that
	// replaces the official postgres image while physical backups are
	// enabled; tagged "<major>-<pgbackrestVersion>".
	pgopPostgresImageRepository = "ghcr.io/ruckc/pgop-postgres"

	// pgbackrestStanza is the stanza of every Cluster: each physical Backup
	// has its own repository path.
	pgbackrestStanza = "main"
	// pgbackrestTLSPort is the port of the pgBackRest TLS server.
	pgbackrestTLSPort = 8432
	// pgbackrestPortName names the TLS server port on the pod and Service.
	pgbackrestPortName = "pgbackrest"
	// pgbackrestContainerName is the sidecar running the TLS server, and the
	// container of the backup and restore Jobs.
	pgbackrestContainerName = "pgbackrest"
	// pgbackrestClientCN is the CommonName of the client certificate the
	// Jobs present; the TLS server only accepts it (tls-server-auth).
	pgbackrestClientCN = "pgop-pgbackrest-client"

	// postgresSocketDir is the directory of PostgreSQL's Unix socket, shared
	// with the sidecar through an emptyDir while backups are enabled.
	postgresSocketDir       = "/var/run/postgresql"
	postgresSocketVolume    = "pg-socket"
	pgbackrestTLSVolumeName = "pgbackrest-tls"
	pgbackrestTLSMountPath  = "/etc/pgop/pgbackrest-tls"
	s3CAVolumeName          = "s3-ca"
	s3CAMountPath           = "/etc/pgop/s3-ca"
	s3CAFileName            = "ca.crt"
	pgbackrestTmpMountPath  = "/tmp"

	// pgbackrestBin is where both images install pgBackRest. pgBackRest
	// writes its own path into restore_command, so it must be the same in
	// the restore Job and the PostgreSQL image.
	pgbackrestBin = "/usr/bin/pgbackrest"
)

// isOfficialPostgresImage reports whether image is the official postgres
// image from Docker Hub (any tag or digest).
func isOfficialPostgresImage(image string) bool {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		image = image[:colon]
	}
	switch image {
	case "postgres", "library/postgres", "docker.io/postgres", "docker.io/library/postgres",
		"index.docker.io/library/postgres", "registry-1.docker.io/library/postgres":
		return true
	}
	return false
}

// supportedPgopPostgresMajors are the majors pgop builds a Postgres+pgBackRest
// image for (see .github/workflows/images.yml).
var supportedPgopPostgresMajors = []int{16, 17, 18}

// postgresMajor returns the Cluster's PostgreSQL major version, or 0.
func postgresMajor(cluster *postgresv1alpha1.Cluster) int {
	if cluster.Spec.PostgresMajorVersion != nil {
		return int(*cluster.Spec.PostgresMajorVersion)
	}
	return parsePostgresMajor(cmp.Or(cluster.Spec.Image, DefaultPostgresImage))
}

// postgresImageForBackups returns the image of a Cluster with physical
// backups: the official postgres image (the default) is replaced by pgop's
// Postgres+pgBackRest image of the same major version; any other image is
// used as it is and must contain pgBackRest pgbackrestVersion at
// pgbackrestBin.
func postgresImageForBackups(cluster *postgresv1alpha1.Cluster) string {
	image := cmp.Or(cluster.Spec.Image, DefaultPostgresImage)
	if !isOfficialPostgresImage(image) {
		return image
	}
	major := postgresMajor(cluster)
	if !slices.Contains(supportedPgopPostgresMajors, major) {
		return image
	}
	return PgopPostgresImage(major)
}

// PgopPostgresImage is pgop's Postgres+pgBackRest image for a PostgreSQL
// major version.
func PgopPostgresImage(major int) string {
	return fmt.Sprintf("%s:%d-%s", pgopPostgresImageRepository, major, pgbackrestVersion)
}

// pgbackrestImage returns the image of a Backup's pgBackRest Jobs.
func pgbackrestImage(backup *postgresv1alpha1.Backup) string {
	if backup.Spec.Physical != nil && backup.Spec.Physical.Image != "" {
		return backup.Spec.Physical.Image
	}
	return DefaultPgbackrestImage
}

// physicalBackupFor returns the physical Backup whose clusterRef is the
// Cluster, or nil. With several, the oldest one (then by name) is used: a
// Cluster archives its WAL into one repository.
func physicalBackupFor(ctx context.Context, c client.Reader, cluster *postgresv1alpha1.Cluster) (*postgresv1alpha1.Backup, error) {
	backups := &postgresv1alpha1.BackupList{}
	if err := c.List(ctx, backups, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}
	var found []*postgresv1alpha1.Backup
	for i := range backups.Items {
		b := &backups.Items[i]
		if b.Spec.Type == postgresv1alpha1.BackupTypePhysical && b.Spec.ClusterRef != nil &&
			b.Spec.ClusterRef.Name == cluster.Name && b.DeletionTimestamp == nil {
			found = append(found, b)
		}
	}
	if len(found) == 0 {
		return nil, nil
	}
	slices.SortFunc(found, func(a, b *postgresv1alpha1.Backup) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return found[0], nil
}

// s3Endpoint is the host and port pgBackRest connects to.
type s3Endpoint struct {
	Host string
	Port int
}

// parseS3Endpoint turns spec.destination.s3.endpoint (as given to the AWS
// CLI: an https:// URL, or a bare host[:port]) into pgBackRest's host and
// port. pgBackRest only speaks HTTPS to S3, so http:// is rejected. Without an
// endpoint, AWS S3 of the region is used.
func parseS3Endpoint(s3 *postgresv1alpha1.S3Destination) (s3Endpoint, error) {
	if s3.Endpoint == "" {
		return s3Endpoint{Host: fmt.Sprintf("s3.%s.amazonaws.com", s3.Region), Port: 443}, nil
	}
	raw := s3.Endpoint
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return s3Endpoint{}, fmt.Errorf("invalid S3 endpoint %q", s3.Endpoint)
	}
	if u.Scheme != "https" {
		return s3Endpoint{}, fmt.Errorf("S3 endpoint %q: physical backups (pgBackRest) require an https:// endpoint", s3.Endpoint)
	}
	if u.Path != "" && u.Path != "/" {
		return s3Endpoint{}, fmt.Errorf("S3 endpoint %q must not have a path", s3.Endpoint)
	}
	port := 443
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil || port < 1 || port > 65535 {
			return s3Endpoint{}, fmt.Errorf("invalid port in S3 endpoint %q", s3.Endpoint)
		}
	}
	return s3Endpoint{Host: u.Hostname(), Port: port}, nil
}

// validatePhysicalBackup checks the parts of a physical Backup that
// pgBackRest needs.
func validatePhysicalBackup(backup *postgresv1alpha1.Backup) error {
	if backup.Spec.ClusterRef == nil {
		return errors.New("clusterRef is required for physical backups")
	}
	dest := backup.Spec.Destination
	if dest.Type != postgresv1alpha1.DestinationTypeS3 || dest.S3 == nil {
		return fmt.Errorf("physical backups support destination type s3 only (got %q)", dest.Type)
	}
	if _, err := parseS3Endpoint(dest.S3); err != nil {
		return err
	}
	if e := backup.Spec.Encryption; e != nil && e.Enabled && e.KeySecretRef == nil {
		return errors.New("encryption.keySecretRef is required when encryption is enabled")
	}
	return nil
}

// repoPath is the repository path of a Backup within its bucket.
func repoPath(backup *postgresv1alpha1.Backup) string {
	prefix := strings.Trim(backup.Spec.Destination.S3.Prefix, "/")
	if prefix == "" {
		prefix = backup.Name
	}
	return "/" + prefix
}

func envVar(name, value string) corev1.EnvVar { return corev1.EnvVar{Name: name, Value: value} }

// pgbackrestRepoEnv returns the repository settings of a (validated)
// physical Backup: the S3 location and credentials, the endpoint CA and the
// encryption passphrase. They are identical in the PostgreSQL pod and the
// Jobs.
func pgbackrestRepoEnv(backup *postgresv1alpha1.Backup) []corev1.EnvVar {
	s3 := backup.Spec.Destination.S3
	ep, _ := parseS3Endpoint(s3)
	env := []corev1.EnvVar{
		envVar("PGBACKREST_STANZA", pgbackrestStanza),
		envVar("PGBACKREST_REPO1_TYPE", "s3"),
		envVar("PGBACKREST_REPO1_PATH", repoPath(backup)),
		envVar("PGBACKREST_REPO1_S3_BUCKET", s3.Bucket),
		envVar("PGBACKREST_REPO1_S3_REGION", s3.Region),
		envVar("PGBACKREST_REPO1_S3_ENDPOINT", ep.Host),
		envVar("PGBACKREST_REPO1_STORAGE_PORT", strconv.Itoa(ep.Port)),
	}
	if s3.Endpoint != "" {
		// S3-compatible storage rarely serves virtual-hosted buckets.
		env = append(env, envVar("PGBACKREST_REPO1_S3_URI_STYLE", "path"))
	}
	if s3.CredentialsSecretRef != nil {
		env = append(env,
			secretEnv("PGBACKREST_REPO1_S3_KEY", s3.CredentialsSecretRef.Name, envAWSAccessKeyID),
			secretEnv("PGBACKREST_REPO1_S3_KEY_SECRET", s3.CredentialsSecretRef.Name, envAWSSecretAccessKey))
	} else {
		// Instance profile / pod identity credentials.
		env = append(env, envVar("PGBACKREST_REPO1_S3_KEY_TYPE", "auto"))
	}
	if s3.CASecretRef != nil {
		env = append(env, envVar("PGBACKREST_REPO1_STORAGE_CA_FILE", s3CAMountPath+"/"+s3CAFileName))
	}
	if e := backup.Spec.Encryption; e != nil && e.Enabled && e.KeySecretRef != nil {
		env = append(env,
			envVar("PGBACKREST_REPO1_CIPHER_TYPE", "aes-256-cbc"),
			secretEnv("PGBACKREST_REPO1_CIPHER_PASS", e.KeySecretRef.Name, e.KeySecretRef.Key))
	}
	return env
}

// pgbackrestPGEnv are the settings that locate the PostgreSQL instance
// (data directory, socket, superuser) and keep pgBackRest from writing log
// files. pgBackRest connects through the Unix socket, where the operator
// user authenticates with trust (as the probes do).
func pgbackrestPGEnv(cluster *postgresv1alpha1.Cluster, layout postgresLayout) []corev1.EnvVar {
	return []corev1.EnvVar{
		envVar("PGBACKREST_PG1_PATH", layout.PGDATA),
		envVar("PGBACKREST_PG1_PORT", strconv.Itoa(int(clusterPort(cluster)))),
		envVar("PGBACKREST_PG1_SOCKET_PATH", postgresSocketDir),
		envVar("PGBACKREST_PG1_USER", DefaultOperatorUsername),
		envVar("PGBACKREST_LOG_LEVEL_CONSOLE", "info"),
		envVar("PGBACKREST_LOG_LEVEL_FILE", "off"),
	}
}

// s3CAVolume mounts the S3 endpoint CA (nil without caSecretRef).
func s3CAVolume(backup *postgresv1alpha1.Backup) ([]corev1.Volume, []corev1.VolumeMount) {
	ref := backup.Spec.Destination.S3.CASecretRef
	if ref == nil {
		return nil, nil
	}
	return []corev1.Volume{{
			Name: s3CAVolumeName,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  ref.Name,
				Items:       []corev1.KeyToPath{{Key: ref.Key, Path: s3CAFileName}},
				DefaultMode: new(int32(0o444)),
			}},
		}},
		[]corev1.VolumeMount{{Name: s3CAVolumeName, MountPath: s3CAMountPath, ReadOnly: true}}
}

// archiveCommand is the operator-owned archive_command. The repository
// settings come from the environment of the postgresql container.
func archiveCommand() string {
	return pgbackrestBin + " --stanza=" + pgbackrestStanza + " archive-push %p"
}

// pgbackrestServerArgs are the server options for WAL archiving.
func pgbackrestServerArgs() []string {
	return []string{"-c", "archive_mode=on", "-c", "archive_command=" + archiveCommand()}
}

// pgbackrestSidecarScript runs in the pgbackrest sidecar: it runs the
// pgBackRest TLS server (restarted when it exits or its certificates are
// renewed) and, once the instance is a primary that accepts connections,
// creates the stanza (idempotent) so WAL archiving starts working.
const pgbackrestSidecarScript = `set -u
tls="` + pgbackrestTLSMountPath + `"
pid=""
certs() { cat "$tls/tls.crt" "$tls/tls.key" "$tls/ca.crt" 2>/dev/null | cksum; }
start() {
  loaded="$(certs)"
  ` + pgbackrestBin + ` server &
  pid=$!
  echo "pgop: pgBackRest TLS server started"
}
stop() {
  [ -n "$pid" ] && kill "$pid" 2>/dev/null && wait "$pid" 2>/dev/null
  exit 0
}
trap stop TERM INT
start
stanza=""
while :; do
  sleep 10 &
  wait $!
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "pgop: pgBackRest TLS server exited; restarting it"
    start
  elif [ "$(certs)" != "$loaded" ]; then
    echo "pgop: TLS certificates changed; restarting the pgBackRest TLS server"
    kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
    start
  fi
  if [ -z "$stanza" ] && pg_isready -q -h "$PGBACKREST_PG1_SOCKET_PATH" -p "$PGBACKREST_PG1_PORT" -U "$PGBACKREST_PG1_USER" -d postgres; then
    recovery="$(psql -h "$PGBACKREST_PG1_SOCKET_PATH" -p "$PGBACKREST_PG1_PORT" -U "$PGBACKREST_PG1_USER" -d postgres -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null)"
    if [ "$recovery" = f ]; then
      if ` + pgbackrestBin + ` stanza-create; then
        stanza=done
      else
        echo "pgop: stanza-create failed; retrying"
      fi
    fi
  fi
done
`

// applyPgbackrestTemplate adds physical backups to the pod template of a
// Cluster with a (validated) physical Backup: the Postgres+pgBackRest image,
// the repository settings, WAL archiving, the Unix socket shared with the
// pgbackrest sidecar (the pgBackRest TLS server), and the recovery-aware
// password-sync hook (a restored instance starts in recovery). Returns the
// sidecar and the pod volumes.
func applyPgbackrestTemplate(cluster *postgresv1alpha1.Cluster, backup *postgresv1alpha1.Backup, layout postgresLayout,
	container *corev1.Container, volumes []corev1.Volume) (corev1.Container, []corev1.Volume) {
	container.Image = postgresImageForBackups(cluster)
	env := append(pgbackrestRepoEnv(backup), pgbackrestPGEnv(cluster, layout)...)
	container.Env = append(container.Env, env...)
	caVolumes, caMounts := s3CAVolume(backup)
	socketMount := corev1.VolumeMount{Name: postgresSocketVolume, MountPath: postgresSocketDir}
	container.VolumeMounts = append(container.VolumeMounts, socketMount)
	container.VolumeMounts = append(container.VolumeMounts, caMounts...)
	container.Lifecycle = replicationPasswordSyncLifecycle()
	args := container.Args
	if len(args) == 0 {
		args = []string{postgresBinary}
	}
	container.Args = append(args, pgbackrestServerArgs()...)

	serverEnv := []corev1.EnvVar{
		envVar("PGBACKREST_TLS_SERVER_ADDRESS", "*"),
		envVar("PGBACKREST_TLS_SERVER_PORT", strconv.Itoa(pgbackrestTLSPort)),
		envVar("PGBACKREST_TLS_SERVER_CERT_FILE", pgbackrestTLSMountPath+"/"+TLSSecretKeyCert),
		envVar("PGBACKREST_TLS_SERVER_KEY_FILE", pgbackrestTLSMountPath+"/"+TLSSecretKeyKey),
		envVar("PGBACKREST_TLS_SERVER_CA_FILE", pgbackrestTLSMountPath+"/"+TLSSecretKeyCA),
		envVar("PGBACKREST_TLS_SERVER_AUTH", pgbackrestClientCN+"="+pgbackrestStanza),
	}
	sidecar := corev1.Container{
		Name:    pgbackrestContainerName,
		Image:   container.Image,
		Command: []string{shBin, "-c", pgbackrestSidecarScript},
		Ports: []corev1.ContainerPort{{
			Name:          pgbackrestPortName,
			ContainerPort: pgbackrestTLSPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env: append(slices.Clone(env), serverEnv...),
		VolumeMounts: append([]corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: layout.MountPath},
			socketMount,
			{Name: pgbackrestTLSVolumeName, MountPath: pgbackrestTLSMountPath, ReadOnly: true},
		}, caMounts...),
		SecurityContext: container.SecurityContext,
	}

	volumes = append(volumes,
		corev1.Volume{Name: postgresSocketVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{
			Name: pgbackrestTLSVolumeName,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: pgbackrestTLSSecretName(cluster),
				Items: []corev1.KeyToPath{
					{Key: TLSSecretKeyCert, Path: TLSSecretKeyCert},
					{Key: TLSSecretKeyKey, Path: TLSSecretKeyKey},
					{Key: TLSSecretKeyCA, Path: TLSSecretKeyCA},
				},
				// pgBackRest accepts a root-owned key with at most 0640,
				// readable by the postgres user through the pod fsGroup.
				DefaultMode: new(tlsFileMode),
			}},
		})
	volumes = append(volumes, caVolumes...)
	return sidecar, volumes
}

// pgbackrestServicePort is the read-write Service port of the TLS server.
func pgbackrestServicePort() corev1.ServicePort {
	return corev1.ServicePort{
		Name:       pgbackrestPortName,
		Port:       pgbackrestTLSPort,
		TargetPort: intstr.FromInt32(pgbackrestTLSPort),
		Protocol:   corev1.ProtocolTCP,
	}
}

// pgbackrestJobEnv are the settings of a Job that runs pgBackRest as the
// repository host: the repository, and the primary's TLS server.
func pgbackrestJobEnv(backup *postgresv1alpha1.Backup, cluster *postgresv1alpha1.Cluster, layout postgresLayout) []corev1.EnvVar {
	env := pgbackrestRepoEnv(backup)
	env = append(env, pgbackrestPGEnv(cluster, layout)...)
	return append(env,
		envVar("PGBACKREST_PG1_HOST", clusterHost(cluster)),
		envVar("PGBACKREST_PG1_HOST_TYPE", "tls"),
		envVar("PGBACKREST_PG1_HOST_PORT", strconv.Itoa(pgbackrestTLSPort)),
		envVar("PGBACKREST_PG1_HOST_CA_FILE", pgbackrestTLSMountPath+"/"+TLSSecretKeyCA),
		envVar("PGBACKREST_PG1_HOST_CERT_FILE", pgbackrestTLSMountPath+"/"+TLSSecretKeyCert),
		envVar("PGBACKREST_PG1_HOST_KEY_FILE", pgbackrestTLSMountPath+"/"+TLSSecretKeyKey),
		envVar("PGBACKREST_START_FAST", "y"),
	)
}

// pgbackrestRetentionEnv maps spec.retention onto pgBackRest's retention of
// full backups (expired after each backup). Nothing is expired while
// retention is disabled (the default): pgBackRest's maximum retention keeps
// every backup (and avoids its "may run out of space" warning).
func pgbackrestRetentionEnv(backup *postgresv1alpha1.Backup) []corev1.EnvVar {
	r := backup.Spec.Retention
	keepAll := []corev1.EnvVar{envVar("PGBACKREST_REPO1_RETENTION_FULL", pgbackrestMaxRetention)}
	if r.Disabled == nil || *r.Disabled {
		return keepAll
	}
	switch {
	case r.KeepLast != nil && *r.KeepLast > 0:
		return []corev1.EnvVar{envVar("PGBACKREST_REPO1_RETENTION_FULL", strconv.Itoa(int(*r.KeepLast)))}
	case r.KeepDays != nil && *r.KeepDays > 0:
		return []corev1.EnvVar{
			envVar("PGBACKREST_REPO1_RETENTION_FULL_TYPE", "time"),
			envVar("PGBACKREST_REPO1_RETENTION_FULL", strconv.Itoa(int(*r.KeepDays))),
		}
	}
	return keepAll
}

// pgbackrestMaxRetention is the largest repo1-retention-full pgBackRest
// accepts.
const pgbackrestMaxRetention = "9999999"

// pgbackrestJobVolumes are the volumes of a Job that talks to the TLS
// server: the client certificate and CA, /tmp (lock files; the root
// filesystem is read-only) and the S3 endpoint CA.
func pgbackrestJobVolumes(backup *postgresv1alpha1.Backup, cluster *postgresv1alpha1.Cluster) ([]corev1.Volume, []corev1.VolumeMount) {
	caVolumes, caMounts := s3CAVolume(backup)
	volumes := make([]corev1.Volume, 0, 2+len(caVolumes))
	volumes = append(volumes,
		corev1.Volume{
			Name: pgbackrestTLSVolumeName,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: pgbackrestTLSSecretName(cluster),
				Items: []corev1.KeyToPath{
					{Key: pgbackrestTLSKeyClientCert, Path: TLSSecretKeyCert},
					{Key: pgbackrestTLSKeyClientKey, Path: TLSSecretKeyKey},
					{Key: TLSSecretKeyCA, Path: TLSSecretKeyCA},
				},
				DefaultMode: new(tlsFileMode),
			}},
		},
		corev1.Volume{Name: volPgbackrestTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	mounts := make([]corev1.VolumeMount, 0, 2+len(caMounts))
	mounts = append(mounts,
		corev1.VolumeMount{Name: pgbackrestTLSVolumeName, MountPath: pgbackrestTLSMountPath, ReadOnly: true},
		corev1.VolumeMount{Name: volPgbackrestTmp, MountPath: pgbackrestTmpMountPath})
	return append(volumes, caVolumes...), append(mounts, caMounts...)
}

// pgbackrestLabelPattern matches a pgBackRest backup label (full,
// differential or incremental).
var pgbackrestLabelPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}F(_[0-9]{8}-[0-9]{6}[DI])?$`)

// pgbackrestPodSecurityContext runs pgBackRest Jobs as the postgres user
// (uid/gid 999), so a restore writes the data directory with the ownership
// PostgreSQL expects and the TLS key is readable through the fsGroup.
func pgbackrestPodSecurityContext() *corev1.PodSecurityContext {
	return postgresPodSecurityContext()
}

// pausedForRestore reports whether a physical Restore stopped the Cluster.
func pausedForRestore(cluster *postgresv1alpha1.Cluster) bool {
	return cluster.Annotations[AnnotationRestoreInProgress] != ""
}

// clustersForBackup maps a physical Backup to the Cluster it names.
func clustersForBackup(_ context.Context, obj client.Object) []reconcile.Request {
	b, ok := obj.(*postgresv1alpha1.Backup)
	if !ok || b.Spec.Type != postgresv1alpha1.BackupTypePhysical || b.Spec.ClusterRef == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: b.Namespace, Name: b.Spec.ClusterRef.Name}}}
}
