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
it up.

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

A physical restore runs `pgbackrest restore` against the repository configured
by the source `Backup`, optionally to a point in time.

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Restore
metadata:
  name: my-cluster-pitr
spec:
  type: physical
  backupRunRef:
    name: my-cluster-full-20260101T020000
  clusterRef:
    name: my-cluster
  targetTime: "2026-01-01T06:00:00Z"   # optional PITR target
```

!!! warning "Physical restores are not automated end to end"
    A physical restore overwrites PostgreSQL's data directory with
    `pgbackrest restore --delta`, so the server must be stopped while it runs.
    pgop does not do this for you, and two things stand in the way today:

    - **The Cluster cannot be stopped through pgop.** `spec.replicas` has a
      minimum of 1, and the operator reverts a manual
      `kubectl scale statefulset <cluster> --replicas=0` on its next reconcile.
      Deleting the StatefulSet with `--cascade=orphan` does not help either:
      the operator recreates it and it adopts the running pod again.
    - **The restore Job does not mount the Cluster's data volume.** It runs
      `pgbackrest restore` with only the pgBackRest configuration and a
      scratch directory mounted, so it cannot write into `data-<cluster>-0`.

    Until this is automated, a physical restore is a manual procedure:

    1. Stop the operator: `kubectl -n <operator-namespace> scale deployment
       <operator-deployment> --replicas=0` (this pauses reconciliation of
       **every** Cluster it manages).
    2. Scale the StatefulSet to 0: `kubectl scale statefulset <cluster>
       --replicas=0`, and wait until the pods are gone.
    3. Run `pgbackrest restore --delta` (with your `--type`/`--target`
       options) in a pod that mounts `data-<cluster>-0` at the Cluster's data
       path and the `<backup>-pgbackrest` ConfigMap at `/etc/pgbackrest`, as
       the postgres user (UID 999).
    4. Scale the operator back up; it restores the StatefulSet's replicas.

    For a point-in-time target, prefer `--target-action=promote`: see
    [Replication → Restores and the password-sync hook](replication.md#restores-and-the-password-sync-hook)
    for why a server left paused in recovery is a problem.

!!! warning "Clusters with standbys"
    A physical restore only rewrites the primary's volume (`data-<cluster>-0`);
    the standbys' copies no longer match it. Scale the Cluster to
    `replicas: 1` before the restore (this removes the standbys and their
    volumes), restore, then scale up again so that new standbys are cloned
    from the restored primary. See [Replication](replication.md#backups-and-restores).

## Spec Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `type` | string | **required** | `logical` or `physical` |
| `backupRunRef.name` | string | **required** | `BackupRun` to restore from (same namespace) |
| `clusterRef.name` | string | **required** | Target `Cluster` (same namespace) |
| `databaseRef.name` | string | required for `logical` | Target `Database` |
| `targetTime` | RFC3339 timestamp | - | Point-in-time target (physical only) |

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
