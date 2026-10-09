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
Job runs `pg_dump -Fc` against the primary and uploads the dump with the AWS
CLI. See [Clusters → Backups and restores](clusters.md#backups-and-restores)
for how the Jobs connect over TLS.

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
  retention:
    disabled: false
    keepLast: 4                        # keep 4 full backups (and what depends on them)
  backupRunTTL: "720h"
  destination:
    type: s3
    s3:
      bucket: pgop-backups
      prefix: my-cluster                # repository path; defaults to the Backup name
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
  official `postgres:<major>` image (see [Images](#images)).
- **WAL archiving**: `archive_mode=on` and
  `archive_command=pgbackrest --stanza=main archive-push %p`. Both are
  operator-owned (they cannot be set in `spec.parameters`). WAL goes straight
  from the pod to the repository.
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
`s3://<bucket>/<prefix>/backup/main/<pgBackRest label>`, for example
`s3://pgop-backups/my-cluster/backup/main/20260101-020000F`. A `BackupRun` is
deleted `backupRunTTL` after it completed (the backup itself stays in the
repository until pgBackRest expires it).

The `Backup` reports `status.lastFullBackupTime`,
`status.lastIncrementalBackupTime` and an `Available` condition (`False` with
the reason when the spec cannot be used, e.g. an `http://` endpoint).

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

They are rebuilt weekly so base-image fixes are picked up, and published for
`linux/amd64` and `linux/arm64` with provenance and an SBOM.

The image swap only applies to the **official** `postgres` image
(`postgres:<tag>`, `docker.io/library/postgres:<tag>`) of a supported major
version; the tag's minor version is not kept (pgop's image follows the latest
minor release of the major). Any other `spec.image` is used as it is and
must contain pgBackRest **2.59.3** at `/usr/bin/pgbackrest`, with the
`postgres` user as UID 999, for example:

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
- WAL is pushed synchronously (no `archive-async`); a repository outage makes
  WAL accumulate in `pg_wal` until it is reachable again.
- `archive_timeout` is not set by pgop: on a quiet server the newest WAL
  reaches the repository only when a segment fills up. Set
  `spec.parameters.archive_timeout` (e.g. `"60s"`) to bound how much a
  point-in-time restore can lose.
