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

!!! danger "A logical restore runs the dump as a superuser"
    `pg_restore` runs the dump's SQL as the target Cluster's operator
    superuser (`pgop_operator`), and `status.location` may name any object
    the Backup's S3 credentials can read. A crafted dump can contain any SQL.
    So whoever can create `Restore`s and patch `backupruns/status` in the
    namespace (or write to the backup bucket) can run SQL as a superuser:
    that is superuser-equivalent. Only restore dumps you trust, grant
    `create` on `restores.pgop.ruck.io` and `patch`/`update` on
    `backupruns/status` only to Cluster administrators, and keep write
    access to the bucket narrow.

Logical backup Jobs do not record `BackupRun`s, so record the dump you want
to restore first. Find it in the bucket (the backup Jobs, and with them the
`s3-upload` container's `Uploaded to s3://...` log line, are deleted five
minutes after they finish):

```sh
aws s3 ls s3://pgop-backups/myapp/data/
```

Create a `BackupRun` whose `backupRef` names the logical `Backup` (it
provides the bucket, endpoint and credentials), then set its
`status.location` to the dump:

```sh
kubectl apply -f - <<EOF
apiVersion: pgop.ruck.io/v1alpha1
kind: BackupRun
metadata:
  name: myapp-data-20260101
spec:
  backupRef:
    name: myapp-backup
  type: data
EOF
kubectl patch backuprun myapp-data-20260101 --subresource=status --type=merge \
  -p '{"status":{"location":"s3://pgop-backups/myapp/data/20260101T020000.dump"}}'
```

Then, **only after the location is set**, create the Restore. A Restore whose
BackupRun has no location yet fails immediately and is not retried (the
Restore does not watch BackupRuns, and its spec is immutable): delete it and
create it again.

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Restore
metadata:
  name: myapp-restore
  namespace: default
spec:
  type: logical
  backupRunRef:
    name: myapp-data-20260101
  clusterRef:
    name: my-cluster        # target cluster (may differ from the source)
  databaseRef:
    name: myapp             # target database (required for logical)
```

The Job runs `pg_restore --no-owner --clean --if-exists` as `pgop_operator`.
What that means:

- **Schema dump**: `--clean` first **drops** the tables and other objects
  the dump contains, **with their data**, then re-creates them empty. The
  re-created objects are owned by `pgop_operator`, not by the database
  owner. Hand them back to the
  owner afterwards (`ALTER TABLE ... OWNER TO <owner>` and so on). Do not
  `REASSIGN OWNED BY pgop_operator` blindly: the operator also owns
  extension objects and schemas pgop created for extensions. Until then the
  owner cannot alter the objects, and
  [object grants](databases.md#which-objects-pgop-grants-on) apply the
  stricter rules for operator-owned objects (no views, no `TRIGGER`).
- **Data dump**: `--clean` does not remove rows; the dump's rows are
  **appended** to the existing tables. Restore into empty tables (`TRUNCATE`
  them first, or restore the schema dump first). Existing rows make primary
  key and unique constraints fail, and foreign keys can fail on the order
  the tables are loaded in. Objects keep their owners.
- **Roles**: only ownership is skipped (`--no-owner`); the dump's `GRANT`s
  are restored, and a `GRANT` to a role the target Cluster does not have
  fails.
- **Errors**: `pg_restore` continues past failing statements but exits with
  an error, so the Job fails although most of the dump was restored. The Job
  is retried (`backoffLimit: 2`; a retry of a data dump appends the rows
  again) and the Restore ends `Failed`. Read
  `kubectl logs job/<restore>-restore` to see which statements failed and
  whether the result is usable.
- pgop applies the grants the target Database declares again at its next
  reconcile (annotate the Database to trigger one).

Unlike a physical restore, a logical restore needs no confirmation on the
Cluster.

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
    name: my-cluster-backup-full-29342160   # a Succeeded BackupRun of a physical Backup
  clusterRef:
    name: my-cluster                        # must be the Backup's clusterRef
  targetTime: "2026-01-01T06:00:00Z"        # optional point-in-time target
```

The `BackupRun` must be `Succeeded` and have a recorded backup
(`status.location`, set by pgop for every backup Job); otherwise the Restore
fails without touching the Cluster. What is restored:

| `targetTime` | Result |
|--------------|--------|
| unset | the BackupRun's backup, recovered just to consistency (`--set=<label> --type=immediate`) |
| set | point-in-time recovery to `targetTime`, from the newest backup taken before it (`--type=time`); the BackupRun only selects the repository |

In both cases the server is promoted once the target is reached
(`--target-action=promote`) and continues on a new timeline, archiving into the
same repository.

### Confirmation

A physical restore stops the Cluster and replaces its data, so creating a
`Restore` is not enough: the **Cluster** must confirm it (which needs
permission to change the Cluster, not just to create Restores):

```sh
kubectl annotate cluster my-cluster pgop.ruck.io/allow-restore=my-cluster-pitr
```

Until then the Restore is `Pending` with reason `AwaitingConfirmation`, and
nothing happens. The value is the Restore name, or `<name>/<uid>` to confirm
exactly one Restore object. The operator removes the annotation when the
Restore finishes (succeeded, failed or deleted), so a confirmation is used
once.

The Cluster records the last finished Restore in `status.lastRestore` (name,
UID, a fingerprint of its spec, result and time). **Once a Cluster had a
physical restore, only the UID form confirms a new one**
(`pgop.ruck.io/allow-restore=<name>/$(kubectl get restore <name> -o jsonpath='{.metadata.uid}')`):
a name-only approval could be re-applied by tooling and re-run the same
Restore manifest.

The Restore `spec` is immutable (the API server rejects changes), so what was
confirmed is what runs.

!!! danger "Never commit `pgop.ruck.io/allow-restore` to git"
    The annotation is a one-time, interactive approval. Kept in a Cluster
    manifest, a GitOps tool would re-add it after pgop removed it, which turns
    it into a standing approval. Apply it with `kubectl annotate`, and exclude
    it from drift detection if your tool reports it.

### What happens

The Restore controller runs the whole procedure; the operator keeps running
and other Clusters are not affected:

1. **Stop the Cluster.** It sets the annotation
   `pgop.ruck.io/restore-in-progress: <restore>` on the Cluster; the Cluster
   controller scales the StatefulSet to 0 and reports
   `Available=False` (`PausedForRestore`).
2. **Restore.** Once no PostgreSQL pod runs, it records the Job name in the
   Restore status (from then on the restore counts as started: deleting the
   Restore can no longer start the Cluster) and runs the Job
   `<restore>-restore`: the pgBackRest image, as UID 999, with the primary's
   volume `data-<cluster>-0` mounted. It runs `pgbackrest restore --delta`
   (only changed files are rewritten), which also writes `recovery.signal`
   and the recovery settings (`restore_command = pgbackrest archive-get …`).
3. **Start the Cluster.** When the Job succeeds, the standbys' data volumes
   (`data-<cluster>-1…`, which no longer match the restored primary) are
   deleted, the annotation is removed, the StatefulSet scales back up and
   PostgreSQL recovers from the WAL archive, then is promoted. The Restore is
   `Succeeded` at this point; the Cluster becomes `Ready` once recovery is
   complete. Standbys are cloned from the restored primary.

`status.conditions` shows the current step (`AwaitingConfirmation`,
`StoppingCluster`, `Restoring`). Only one Restore at a time can hold a
Cluster; a second one waits (`WaitingForRestore`), including for a Restore
that is being deleted until its Job has stopped.

### Failed or interrupted restores

If the restore Job **fails**, or the Restore is **deleted after its Job
started**, the data directory may be partly restored:

- a deleted Restore first deletes its Job (foreground) and waits until none
  of its pods exists, so pgBackRest and PostgreSQL never run on the volume at
  the same time;
- the Cluster **stays stopped**: the annotation
  `pgop.ruck.io/restore-interrupted: <restore>` replaces `restore-in-progress`,
  and the Cluster reports `RestoreInterrupted=True` (and `Available=False`,
  reason `RestoreInterrupted`) with a Warning event;
- the standbys keep their volumes (they are only deleted after a successful
  restore);
- a failed Job's pod and log are kept (`kubectl logs job/<restore>-restore`).

The way out is to **restore again**: fix the cause, create a new Restore and
confirm it. `pgbackrest restore --delta` brings the partly restored directory
to the backup's state, and its success clears `restore-interrupted`.

Removing `pgop.ruck.io/restore-interrupted` by hand starts PostgreSQL on the
data directory as it is. Only do this if you know the restore did not change
it (e.g. it failed before writing anything). pgBackRest deletes
`global/pg_control` before changing any file and writes it last (verified in
pgBackRest 2.59.3, `restore/clean.c` and `restore.c`), so PostgreSQL refuses to
start on an incomplete restore instead of running on mixed data; but a restore
that was interrupted early may have left the previous data without its
`pg_control`, which then needs a new restore as well.

A Restore deleted before it recorded its Job name (still `Pending` or
stopping the Cluster), with no Job of it existing, leaves the data untouched,
and the Cluster simply starts again. From the moment the Job name is recorded
(condition reason `CreatingJob`), any interruption, including a Restore whose
target (BackupRun, Backup) disappears while its Job is created, counts as
interrupted: whether the Job ran cannot be told for sure, so the Cluster stays
stopped. pgop also never starts the Cluster while a Job of the Restore
exists.

### Disaster recovery

The Cluster and its data volume must exist (the Job restores into
`data-<cluster>-0`). To recover a lost Cluster into a new namespace or
Kubernetes cluster:

1. Create the Cluster with the same name, and the physical Backup with the
   same `destination`, credentials and encryption passphrase, and a `prefix`
   equal to the old repository path (the default path contains the
   namespace: `/<old-namespace>/<cluster>/<backup>`). The new, empty server
   cannot archive into the existing repository (its system identifier
   differs); `WALArchiving` is `False` until the restore, and the queued WAL
   is bounded by `archivePushQueueMax`.
2. Find the backup to restore: the labels are the directory names under
   `s3://<bucket>/<repository path>/backup/main/` (e.g. `20260101-020000F`,
   or `20260101-020000F_20260102-020000I` for an incremental one).
3. Record it in a `BackupRun` and mark it `Succeeded` (the status
   subresource):

    ```sh
    kubectl apply -f - <<EOF
    apiVersion: pgop.ruck.io/v1alpha1
    kind: BackupRun
    metadata:
      name: dr-20260101
    spec:
      backupRef:
        name: my-cluster-backup
      type: full
    EOF
    kubectl patch backuprun dr-20260101 --subresource=status --type=merge -p \
      '{"status":{"phase":"Succeeded","location":"s3://<bucket>/<repository path>/backup/main/20260101-020000F"}}'
    ```

4. Create the Restore with that `backupRunRef` (with `targetTime` to recover
   past the backup, up to the newest archived WAL) and confirm it on the
   Cluster.

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
- Ready-to-edit manifests: `examples/restores/` in the repository.
