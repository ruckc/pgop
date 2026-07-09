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

!!! warning "Physical restore rewrites the data directory"
    A physical restore overwrites PostgreSQL's data directory with
    `pgbackrest restore --delta`. The target Cluster **must be stopped** (scale
    its StatefulSet to `0` replicas) with its data volume available before the
    restore Job runs, and started again afterward. The operator issues the
    restore command but does not automate the stop/start dance — do that as part
    of your recovery runbook.

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
