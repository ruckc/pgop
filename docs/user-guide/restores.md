# Restores

A `Restore` resource triggers a **one-shot** restore of a previous
[`BackupRun`](../reference/api.md) into a target [`Cluster`](clusters.md)
(and, for logical restores, a target [`Database`](databases.md)).

## Overview

The Restore controller:

1. Resolves the referenced `BackupRun` and its parent `Backup` (for the
   destination bucket and credentials).
2. Creates a Kubernetes **Job** that performs the restore.
3. Tracks the Job and reflects its state in `status.phase`
   (`Pending` → `Running` → `Succeeded`/`Failed`).

The Job is owner-referenced by the `Restore`, so deleting the `Restore` cleans
it up. A physical restore also stops and restarts the Cluster (see below).

## Logical restore (pg_restore)

A logical restore downloads the `pg_dump` artifact recorded on
`BackupRun.status.location` and runs `pg_restore` into the target database.

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Restore
metadata:
  name: myapp-restore
  namespace: default
spec:
  type: logical
  backupRunRef:
    name: myapp-backup-data-20260101T020000
  clusterRef:
    name: my-cluster        # target cluster (may differ from the source)
  databaseRef:
    name: myapp             # target database (required for logical)
```

The restore runs `pg_restore --no-owner --clean --if-exists`, so it recreates
objects into an existing database and tolerates a differing role set on the
target cluster.

When the target Cluster has TLS enabled, `pg_restore` connects with the
`sslmode` and `ca.crt` from the target Cluster's credentials Secret
(`verify-full` once `TLSReady` is `True`). See
[Clusters → Backups and restores](clusters.md#backups-and-restores).

## Physical restore (pgBackRest)

A physical restore replaces the data directory of the Cluster a
[physical Backup](backups.md#physical-backups-pgbackrest) was taken from, with
`pgbackrest restore` from that Backup's repository.

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Restore
metadata:
  name: my-cluster-pitr
spec:
  type: physical
  backupRunRef:
    name: my-cluster-backup-full-29342160   # selects the Backup (repository)
  clusterRef:
    name: my-cluster                        # must be the Backup's clusterRef
  targetTime: "2026-01-01T06:00:00Z"        # optional point-in-time target
```

What is restored:

| `targetTime` | BackupRun `status.location` | Result |
|--------------|-----------------------------|--------|
| set | any | point-in-time recovery to `targetTime`, from the newest backup taken before it (`--type=time`) |
| unset | a backup (recorded by pgop) | that backup, recovered just to consistency (`--set=<label> --type=immediate`) |
| unset | empty | the newest backup, replaying all archived WAL (latest state) |

In every case the server is promoted once the target is reached
(`--target-action=promote`) and continues on a new timeline, archiving into the
same repository.

### What happens

The Restore controller runs the whole procedure; the operator keeps running
and other Clusters are not affected:

1. **Stop the Cluster.** It sets the annotation
   `pgop.ruck.io/restore-in-progress: <restore>` on the Cluster; the Cluster
   controller scales the StatefulSet to 0 and reports
   `Available=False` (`PausedForRestore`).
2. **Restore.** Once no PostgreSQL pod runs, it deletes the standbys' data
   volumes (`data-<cluster>-1…`; they no longer match the restored primary and
   are cloned again) and runs the Job `<restore>-restore`: the pgBackRest
   image, as UID 999, with the primary's volume `data-<cluster>-0` mounted. It
   runs `pgbackrest restore --delta` (only changed files are rewritten), which
   also writes `recovery.signal` and the recovery settings
   (`restore_command = pgbackrest archive-get …`).
3. **Start the Cluster.** When the Job succeeds the annotation is removed, the
   StatefulSet scales back up and PostgreSQL recovers from the WAL archive,
   then is promoted. The Restore is `Succeeded` at this point; the Cluster
   becomes `Ready` once recovery is complete. Standbys are cloned from the
   restored primary.

`status.conditions` shows the current step (`StoppingCluster`, `Restoring`).
Only one Restore at a time can hold a Cluster; a second one waits
(`WaitingForRestore`).

!!! warning "When the restore Job fails"
    The data directory may be partly restored, so the Cluster **stays
    stopped** and the Restore is `Failed` (the Job's pod and log are kept).
    Fix the cause and create a new Restore, or delete the failed Restore to
    start the Cluster again on whatever is in its volume. Deleting a Restore
    at any time starts the Cluster again.

### Disaster recovery

The Cluster and its data volume must exist (the Job restores into
`data-<cluster>-0`). To recover a lost Cluster into a new namespace or
Kubernetes cluster:

1. Create the Cluster with the same name, and the physical Backup with the
   same `destination` (same bucket and `prefix`, credentials and encryption
   passphrase). The new, empty server cannot archive into the existing
   repository (its system identifier differs), which is expected until the
   restore.
2. Create a `BackupRun` that points at the Backup (`status.location` may stay
   empty to restore the latest state):

    ```yaml
    apiVersion: pgop.ruck.io/v1alpha1
    kind: BackupRun
    metadata:
      name: dr-latest
    spec:
      backupRef:
        name: my-cluster-backup
      type: full
    ```

3. Create the Restore with that `backupRunRef` (and optionally `targetTime`).

### Limitations

- Restoring into a different Cluster than the one the Backup was taken from
  is not supported (use a logical backup to copy data between Clusters).
- The restore and the WAL replay need the repository to be reachable from
  both the Job and the Cluster pod.

## Spec Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `type` | string | **required** | `logical` or `physical` |
| `backupRunRef.name` | string | **required** | `BackupRun` to restore from (same namespace) |
| `clusterRef.name` | string | **required** | Target `Cluster` (same namespace) |
| `databaseRef.name` | string | required for `logical` | Target `Database` |
| `targetTime` | RFC3339 timestamp | - | Point-in-time target (physical only; second precision) |

## Status

| Field | Description |
|-------|-------------|
| `phase` | `Pending`, `Running`, `Succeeded`, or `Failed` |
| `startTime` | When the restore Job started |
| `completionTime` | When the restore Job finished |
| `jobName` | Name of the Job executing the restore (`<restore-name>-restore`) |
| `conditions` | Detailed status conditions |

## Notes

- `backupRunRef`, `clusterRef`, and `databaseRef` are resolved in the
  **same namespace** as the `Restore`.
- To re-run a restore, delete and recreate the `Restore` (or create a new one
  with a different name); a `Restore` is a one-shot record, not a controller
  loop.
