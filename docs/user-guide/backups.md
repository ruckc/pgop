# Backups

A `Backup` is a backup **policy**: the operator turns it into CronJobs that
write to object storage (S3 or S3-compatible). There are two types:

| Type | Tool | Scope | Restore |
|------|------|-------|---------|
| `logical` | `pg_dump` (schema + data dumps) | one `Database` | `pg_restore` into a database ([Restores](restores.md#logical-restore-pg_restore)) |
| `physical` | [pgBackRest](https://pgbackrest.org) (full + incremental, WAL archiving) | a whole `Cluster` | data directory, to a backup or a point in time ([Restores](restores.md#physical-restore-pgbackrest)) |

## Logical backups (pg_dump)

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Backup
metadata:
  name: myapp-backup
spec:
  type: logical
  databaseRef:
    name: myapp
  schedule: "0 2 * * *"        # schema and data dumps share one schedule
  backupRunTTL: "168h"
  destination:
    type: s3
    s3:
      bucket: pgop-backups
      prefix: myapp
      region: us-east-1
      endpoint: http://rustfs:9000     # optional, S3-compatible storage
      credentialsSecretRef:
        name: s3-credentials           # keys AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
```

The operator creates the CronJobs `<backup>-schema` and `<backup>-data`. Each
Job runs `pg_dump -Fc` (`--schema-only` or `--data-only`) against the primary
as the operator's superuser and uploads the dump with the AWS CLI to
`s3://<bucket>/<prefix>/schema/<time>.dump` and
`s3://<bucket>/<prefix>/data/<time>.dump` (`<time>` is `YYYYMMDDTHHMMSS`, UTC
in the Job's container). The `s3-upload` container logs the location
(`Uploaded to s3://...`). See
[Clusters → Backups and restores](clusters.md#backups-and-restores) for how
the Jobs connect over TLS.

Things to know about logical backups:

- Only `s3` destinations are implemented; `endpoint` may be `http://` (for
  example an in-cluster MinIO or RustFS).
- `retention` and `encryption` are not applied: dumps are never expired or
  encrypted by pgop. Use bucket lifecycle rules and server-side encryption.
- The Jobs do **not** record `BackupRun`s. To restore a dump, create a
  `BackupRun` for it by hand (see
  [Restores: logical](restores.md#logical-restore-pg_restore)).
- Take a dump now with
  `kubectl create job myapp-data-now --from=cronjob/myapp-backup-data`.

## Physical backups (pgBackRest)

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Backup
metadata:
  name: my-cluster-backup
spec:
  type: physical
  clusterRef:
    name: my-cluster
  physical:
    fullSchedule: "0 2 * * 0"          # default: Sundays 02:00
    incrementalSchedule: "0 2 * * 1-6" # default: the other days
    # image: ghcr.io/ruckc/pgop-pgbackrest:2.59.3   # default
    # archivePushQueueMax: 4Gi       # default: a quarter of the Cluster's storage size
    # postgresImageIncludesPgbackrest: true   # only for a custom Cluster image, see Images
  retention:
    disabled: false
    keepLast: 4                        # keep 4 full backups (and what depends on them)
  backupRunTTL: "720h"
  destination:
    type: s3
    s3:
      bucket: pgop-backups
      prefix: my-cluster                # repository path; default /<namespace>/<cluster>/<backup>
      region: us-east-1
      endpoint: https://minio.example.com   # optional; must be https://
      credentialsSecretRef:
        name: s3-credentials            # keys AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
      caSecretRef:                      # optional: CA of a private endpoint
        name: minio-ca
        key: ca.crt
  encryption:                           # optional: pgBackRest repository encryption
    enabled: true
    keySecretRef:
      name: backup-encryption
      key: passphrase
```

### What the operator sets up

**On the Cluster** (only while a physical `Backup` names it in `clusterRef`):

- The pod runs pgop's **Postgres+pgBackRest image**
  `ghcr.io/ruckc/pgop-postgres:<major>-<pgbackrest-version>` instead of the
  official `postgres:<major>` image (see [Images](#images)). If the Cluster's
  image cannot be swapped and is not declared to contain pgBackRest, the
  Backup is `Invalid` and **the Cluster is not changed** (no archiving is
  configured that could never succeed).
- **WAL archiving**: `archive_mode=on` and an `archive_command` that runs
  `pgbackrest --stanza=main archive-push %p` (and records dropped WAL, see
  below). Both are operator-owned (they cannot be set in `spec.parameters`).
  WAL goes straight from the pod to the repository.
- **A bound on queued WAL**: while archiving fails (wrong credentials, the
  repository unreachable, the stanza not created yet), WAL waits in `pg_wal`
  up to `physical.archivePushQueueMax` (pgBackRest `archive-push-queue-max`;
  default a quarter of `storage.size`, at least 64Mi). Beyond it pgBackRest
  **drops** WAL instead of filling the volume and stopping PostgreSQL. See
  [Dropped WAL](#dropped-wal). The Cluster's and the Backup's `WALArchiving`
  condition (checked every minute) turns `False` as soon as archiving fails,
  and stays `False` after a drop until the gap is closed.
- A **`pgbackrest` sidecar** runs the
  [pgBackRest TLS server](https://pgbackrest.org/user-guide.html#repo-host/setup-tls)
  on port 8432, exposed on the read-write Service `<cluster>`, and creates the
  stanza (`pgbackrest stanza-create`, idempotent) once the primary is up, so
  archiving starts working right away rather than at the first backup.
- The repository settings reach pgBackRest as `PGBACKREST_*` environment
  variables taken from the `Backup` (credentials and the encryption
  passphrase stay in their Secrets); there is no `pgbackrest.conf`.
- A Secret `<cluster>-pgbackrest-tls` with a CA, a server certificate and a
  client certificate (see [pgBackRest TLS](#pgbackrest-tls)).

Adding or removing the `Backup` (or changing its destination) restarts the
Cluster's pods once, to change the image and the archiving settings. Clusters
without a physical `Backup` keep their pod template: upgrading the operator
does not restart them.

When the Backup is deleted (or becomes `Invalid`), archiving and the sidecar
are removed, but **pgop's image is kept**: switching back to the official
image could change the C library (see [Images](#images)). The Cluster's
`PhysicalBackup` condition shows the state:

| `PhysicalBackup` | Meaning |
|------------------|---------|
| `True` (`Enabled`) | WAL is archived for the named Backup |
| `False` (`Invalid`) | a physical Backup names the Cluster but cannot be used (the message says why); WAL is **not** archived. A Warning event is recorded |
| `False` (`Disabled`) | no physical Backup, and pgop's image is kept; set `spec.image` to it to keep it explicitly, or to another image once you checked the collations |
| absent | the Cluster never had a physical Backup |

**The backups**: the CronJobs `<backup>-full` and `<backup>-incremental` run
pgBackRest as the *repository host*. Each Job reads and writes the repository
directly and reaches the primary's data directory through the TLS server
(`pg1-host-type=tls`), then:

1. `pgbackrest stanza-create` (a no-op once the stanza exists);
2. `pgbackrest backup --type=full|incr` (an incremental backup without a prior
   full backup is taken as a full one; `start-fast` forces an immediate
   checkpoint);
3. expires backups beyond the retention (pgBackRest does this after each
   backup).

### Status and BackupRuns

Every Job of the CronJobs, including one created by hand, is recorded as a
`BackupRun` named after the Job, owned by the `Backup`:

```console
$ kubectl get backupruns
NAME                             TYPE          PHASE       STARTED   COMPLETED
my-cluster-backup-full-29342160  full          Succeeded   10m       9m
```

`status.location` holds the backup:
`s3://<bucket>/<repository path>/backup/main/<pgBackRest label>`, for example
`s3://pgop-backups/my-cluster/backup/main/20260101-020000F`. A `BackupRun` is
deleted `backupRunTTL` after it completed (the backup itself stays in the
repository until pgBackRest expires it).

The `Backup` reports `status.lastFullBackupTime`,
`status.lastIncrementalBackupTime`, an `Available` condition (`False`,
reason `Invalid`, when the spec cannot be used: an `http://` endpoint, an
unsupported Cluster image, a second physical Backup of the Cluster; nothing
is scheduled then) and `WALArchiving`, mirrored from the Cluster:

| `WALArchiving` | Meaning |
|----------------|---------|
| `True` (`Archiving`) | the last WAL segment was archived after the last failure |
| `False` (`ArchiveFailing`) | the last attempt failed; the message names the segment, see the `postgresql` container log |
| `False` (`WALDropped`) | pgBackRest dropped WAL; latched until the gap is closed, see [Dropped WAL](#dropped-wal) |
| `Unknown` | nothing archived yet, or the primary cannot be queried |

### Dropped WAL

When the WAL waiting to be archived exceeds `archivePushQueueMax`, pgBackRest
tells PostgreSQL that the segment was archived and drops it, only logging a
warning. `pg_stat_archiver` then shows success, so pgop's `archive_command`
records each drop in `$PGDATA/pgop-wal-dropped` (`<UTC time> <segment>`), and
the operator reads it every minute:

- `status.lastWALDrop` on the Cluster records the newest dropped segment and
  when; a Warning event (`WALDropped`) is recorded;
- `WALArchiving` is `False` (`WALDropped`) even though archiving works again.

**What is lost:** point-in-time recovery to any time from the dropped segment
on, from any backup taken before the drop: recovery cannot replay WAL across
the gap. Backups taken before the drop can still be restored to their own
consistency point.

**What closes the gap:** a backup that **starts after** the drop and
succeeds, of any type: full, differential or incremental. An incremental or
differential backup depends on earlier backups' *files*, not on their WAL, and
restoring any backup only needs the WAL from its own start onward (which
pgBackRest checks is archived before the backup succeeds). Once a successful
`BackupRun` started after the drop exists, `lastWALDrop.closedBy` names it
and `WALArchiving` reflects archiving again. Take one right away
(`kubectl create job ... --from=cronjob/<backup>-full`) once the cause of the
outage is fixed.

The marker file is kept in the data directory (so it survives restarts and is
part of backups); a restored data directory brings the drops from before its
backup, which that backup already closes.

To take a backup now:

```sh
kubectl create job my-cluster-backup-full-now --from=cronjob/my-cluster-backup-full
```

### Retention

| `retention` | pgBackRest |
|-------------|------------|
| `disabled: true` (default) | nothing is expired (`repo1-retention-full` at its maximum) |
| `keepLast: N` | `repo1-retention-full=N` (full backups; incrementals and WAL of expired backups go with them) |
| `keepDays: N` | `repo1-retention-full-type=time`, `repo1-retention-full=N` |

Keep `disabled: true` with write-only credentials (e.g. object lock); expiry
needs delete permission.

### Repository path

Without `prefix`, the repository is `/<namespace>/<cluster>/<backup>` in the
bucket, so Backups of different Clusters (or namespaces) sharing a bucket
never share a repository: a pgBackRest stanza belongs to one PostgreSQL
system, and archiving into another system's stanza fails. Changing `prefix`
(or the Backup's name without a prefix) starts a new, empty repository; the
old one, with its backups, stays where it is and is not expired by pgop.

### pgBackRest TLS

The TLS server and the backup Jobs authenticate each other with certificates
from a CA the operator creates per Cluster, in the Secret
`<cluster>-pgbackrest-tls`:

| Key | Content | Mounted into |
|-----|---------|--------------|
| `ca.crt` | the CA | the pod and the Jobs |
| `ca.key` | the CA key | nowhere |
| `tls.crt`, `tls.key` | server certificate for `<cluster>.<namespace>.svc.cluster.local` (and shorter forms) | the pod |
| `client.crt`, `client.key` | client certificate, CN `pgop-pgbackrest-client` | the backup Jobs |

The server only accepts that client certificate (`tls-server-auth`), and the
Jobs verify the server certificate against the CA and the Service name. The
CA is valid for 10 years and replaced one year before it expires; the
certificates are valid for one year and renewed 60 days before they expire.
The sidecar restarts the TLS server when its mounted certificates change; a
backup that starts in the minute after a renewal, before the kubelet updated
the mounted Secret, can fail once and is retried by the Job.

This CA is independent of [`spec.tls`](clusters.md#tls): the PostgreSQL
server certificate may come from your own Secret or from cert-manager, whose
CA key pgop does not hold, and pgBackRest also needs a client certificate.
The Secret is deleted when the Cluster no longer has a physical `Backup`.

### S3 endpoints

pgBackRest always talks HTTPS to S3 and verifies the certificate:

- `endpoint` must be `https://host[:port]` (or `host[:port]`); `http://` is
  rejected (the `Backup` reports it, and the Cluster is not changed). For a
  plain-HTTP store, put a TLS proxy in front of it.
- Without `endpoint`, AWS S3 `s3.<region>.amazonaws.com` is used
  (virtual-hosted buckets); with `endpoint`, path-style URLs are used.
- For a private CA, set `caSecretRef` (a key in a Secret in the Backup's
  namespace holding the PEM CA bundle).
- Without `credentialsSecretRef`, pgBackRest uses instance/pod credentials
  (`repo1-s3-key-type=auto`).

### Images

pgop builds both images from one Dockerfile (`images/pgbackrest/Dockerfile`)
with one pinned pgBackRest release, because pgBackRest requires the same
version on both ends of its protocol:

| Image | Used by |
|-------|---------|
| `ghcr.io/ruckc/pgop-pgbackrest:2.59.3` | backup and restore Jobs (`spec.physical.image`) |
| `ghcr.io/ruckc/pgop-postgres:<major>-2.59.3` (16, 17, 18) | Clusters with physical backups |

They are published for `linux/amd64` and `linux/arm64` with provenance, an
SBOM and a build attestation. The base images are pinned by digest
(Dependabot bumps them, e.g. for a new PostgreSQL minor release) and the
images are rebuilt weekly for package updates. Each publish also writes an
immutable tag with the date, e.g. `ghcr.io/ruckc/pgop-postgres:18-2.59.3-20261012`.

The operator uses the moving tag `<major>-2.59.3` (pull policy
`IfNotPresent`): a node keeps the build it pulled first, so after a rebuild
pods on different nodes can run different builds (PostgreSQL minor releases
of the same major, which streaming replication supports) until their nodes
pull the new one. To pin a build, set `spec.image` to a dated tag or digest
of `ghcr.io/ruckc/pgop-postgres` and `physical.postgresImageIncludesPgbackrest: true`
(and `physical.image` to the matching `pgop-pgbackrest` tag).

The image swap only applies to the **official** `postgres` image
(`postgres:<tag>` or `docker.io/library/postgres:<tag>`) of a supported major
version whose tag is known to be **Debian trixie**, the base of pgop's image,
so the C library and its collations (which text indexes depend on) stay the
same:

| Tag | Swapped |
|-----|---------|
| `<major>-trixie`, `<major>.<minor>-trixie` | yes |
| `18`, `18.<minor>` | yes (every released PostgreSQL 18 image is trixie) |
| `16`, `17`, `16.<minor>`, `17.<minor>` | only with `physical.acceptImageSwap: true` |
| `-alpine*`, `-bookworm`, `-bullseye`, digest-only, `latest` | never: the Backup is `Invalid` |

The bare 16 and 17 tags (including minor tags such as `16.4` or `17.2`) were
Debian **bookworm** until August 2025: a Cluster initialized on one of them
before that has text indexes built with bookworm's glibc 2.36, and trixie has
glibc 2.41. Set `acceptImageSwap: true` only after checking that the Cluster
was initialized on a trixie image, or with a plan to `REINDEX` its text
indexes (and refresh the collation versions) after the swap. `-alpine` uses
musl and is never swapped. The tag's minor version is not kept: pgop's image
follows the latest minor release of the major.

pgop never swaps back on its own: when the Backup is deleted, pgop's image
stays (see above).

Any other image must contain pgBackRest **2.59.3** at `/usr/bin/pgbackrest`,
with the `postgres` user as UID 999, and the Backup must declare it with
`physical.postgresImageIncludesPgbackrest: true`, for example:

```dockerfile
FROM ghcr.io/ruckc/pgop-pgbackrest:2.59.3 AS pgbackrest
FROM my-registry/postgis:18
COPY --from=pgbackrest /usr/bin/pgbackrest /usr/bin/pgbackrest
# plus pgBackRest's runtime libraries, see images/pgbackrest/Dockerfile
```

If you override `spec.physical.image`, it must run the same pgBackRest
version as the Cluster's image.

### Limitations

- One physical `Backup` per Cluster (one repository); a second one is
  rejected (`Available=False`).
- S3 destinations only (no Azure/GCS for physical backups yet).
- Backups are always taken from the primary.
- WAL is pushed synchronously (no `archive-async`); during a repository
  outage WAL accumulates in `pg_wal` up to `archivePushQueueMax`, then is
  dropped (see above).
- Archiving is enabled when the pod starts with the Backup, before the stanza
  exists; the sidecar creates it within seconds of the primary accepting
  connections, and failures until then are bounded as above. (Enabling
  `archive_command` only after a health check would need an extra pod
  restart or reload orchestration.)
- `archive_timeout` is not set by pgop: on a quiet server the newest WAL
  reaches the repository only when a segment fills up. Set
  `spec.parameters.archive_timeout` (e.g. `"60s"`) to bound how much a
  point-in-time restore can lose.
