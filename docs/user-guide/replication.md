# Replication

A Cluster with `replicas` greater than 1 runs one **primary** and
`replicas - 1` **hot standbys** that stream the primary's WAL
(asynchronous physical streaming replication). The standbys serve read-only
queries.

!!! warning "No automated failover"
    pgop does **not** promote a standby when the primary fails. While the
    primary pod is down, writes are unavailable (the standbys keep serving
    reads, possibly slightly stale) until Kubernetes restarts the primary.
    Read replicas add read capacity and a warm copy of the data; they are not
    a high-availability setup.

    A manual, planned switchover is planned as the next step (issue #26,
    milestone 2) and is not available yet. If you need automated failover
    today, use an operator built for it, such as
    [CloudNativePG](https://cloudnative-pg.io/) (or Crunchy PGO / Zalando
    postgres-operator).

## Example

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: production-db
spec:
  replicas: 3          # 1 primary + 2 standbys (1-10)
  storage:
    size: 20Gi
  tls: {}              # optional; replication then runs over TLS
```

```console
$ kubectl get cluster production-db -o wide
NAME            READY   INSTANCES   READY INSTANCES   PRIMARY           ENDPOINT                                          AGE
production-db   true    3           3                 production-db-0   production-db.default.svc.cluster.local:5432   5m
```

Requires Kubernetes 1.27 or later (the documented minimum for pgop).

## Topology and Services

| Pod | Role | Label `pgop.ruck.io/role` |
|-----|------|---------------------------|
| `<cluster>-0` | primary (read-write) | `primary` |
| `<cluster>-1` … `<cluster>-N` | hot standby (read-only) | `replica` |

| Service | Routes to | Use for |
|---------|-----------|---------|
| `<cluster>` (`status.endpoint`) | the primary only | everything that writes; the default for all clients |
| `<cluster>-ro` (`status.readOnlyEndpoint`) | the standbys | read-only queries that can tolerate replication lag |

- The read-write Service selects the primary pod by its StatefulSet pod name
  (`statefulset.kubernetes.io/pod-name: <cluster>-0`), so it never routes to a
  standby, and keeps working while the operator is not running.
- The read-only Service selects `pgop.ruck.io/role: replica`. The operator
  sets the role labels (the StatefulSet pod template cannot carry per-pod
  labels); a standby pod that was recreated joins `<cluster>-ro` once the
  operator has labelled it.
- `<cluster>-ro` exists only while `replicas` is greater than 1. Writes
  through it fail (`cannot execute INSERT in a read-only transaction`).
- The operator, Role and Database reconciles, backups and restores all go
  through `<cluster>`, i.e. always to the primary.
- With `spec.tls`, all instances serve the same certificate. Operator-requested
  certificates (self-managed CA, `issuerRef`) also name `<cluster>-ro…`; with
  `secretName`, add `<cluster>-ro.<namespace>.svc.cluster.local` to your
  certificate if clients verify the standbys.

Role and Database credentials Secrets point at the primary. For read-only
traffic, use the same credentials with the `-ro` host.

## How it works

### Bootstrapping a standby

Every pod runs an init container `pgop-bootstrap` (the Cluster's image):

- a data directory that already exists is used as it is;
- the primary (`<cluster>-0`) with an empty volume is initialized by the image
  entrypoint (`initdb`) as usual, **unless standbys of the Cluster are still
  running**: then the primary's volume was lost, and starting an empty primary
  next to standbys that hold the data would lose it. The init container
  refuses to start (see [Primary volume lost](#primary-volume-lost));
- a standby with an empty volume clones the primary with
  `pg_basebackup -X stream` over its replication slot, retrying every few
  seconds until the primary allows it. The clone is written to a staging
  directory and only moved into place when complete, so an interrupted clone
  is redone instead of starting on partial data.

The standby then starts in standby mode (`standby.signal`) and streams from
the primary through the read-write Service:

```
primary_conninfo = 'host=<cluster>.<ns>.svc.cluster.local port=<port> user=pgop_replicator
                    passfile=/run/pgop/replication.pgpass application_name=<pod>
                    sslmode=verify-full sslrootcert=/etc/pgop/tls/ca.crt'   # sslmode=disable without spec.tls
```

### Replication user

Standbys connect as the dedicated role `pgop_replicator` (`LOGIN
REPLICATION`, not a superuser). Its password is the `replication-password` key
of the `<cluster>-credentials` Secret. The operator creates the role on the
primary and sets the password as a SCRAM-SHA-256 verifier (never plaintext),
again whenever the Secret's password changes (tracked with the
`pgop.ruck.io/replication-password-fingerprint` annotation). The role name is
reserved: a Role resource cannot use `roleName: pgop_replicator`.

### pg_hba.conf

A Cluster with standbys always runs with an operator-managed `pg_hba.conf`
(ConfigMap `<cluster>-hba`, loaded with `-c hba_file=…`). It is the same file
used for [`spec.tls.requireTLS`](clusters.md#tls), extended for replication:

```
local     all         all                       trust
local     replication all                       trust
hostssl   replication pgop_replicator 0.0.0.0/0 scram-sha-256
hostssl   replication pgop_replicator ::/0      scram-sha-256
host      all         pgop_replicator all       reject
hostssl   all         all             0.0.0.0/0 scram-sha-256
hostssl   all         all             ::/0      scram-sha-256
hostnossl all         all             all       reject
hostnossl replication all             all       reject
```

Without `requireTLS` (or without `spec.tls`) the `hostssl` lines are `host`
lines and there are no `hostnossl … reject` lines. `pgop_replicator` can only
open replication connections. Changes made by hand to `$PGDATA/pg_hba.conf`
have no effect while the managed file is in use.

### Replication slots

Each standby streams over its own physical replication slot
`pgop_replica_<n>`, created by the operator on the primary, so the primary
keeps the WAL a standby still needs. To keep a standby that is down for a long
time from filling the primary's disk, `max_slot_wal_keep_size` defaults to a
quarter of `spec.storage.size` (at least 64MB). Set it in `spec.parameters`
to override it (`-1` = unlimited). When a slot exceeds it, PostgreSQL
invalidates the slot and that standby must be re-cloned (see
[Re-cloning a standby](#re-cloning-a-standby)).

`wal_level`, `max_wal_senders`, `max_replication_slots`, `hot_standby`,
`primary_conninfo` and `primary_slot_name` are reserved and cannot be set in
`spec.parameters`. The PostgreSQL defaults (`replica`, 10, 10, `on`) are
enough for up to 10 instances. All other parameters, and TLS settings, apply
to every instance.

### Synchronous replication

Replication is asynchronous: a commit returns before the standbys have the
WAL, so a standby can lag behind, and a transaction committed on the primary
just before the primary's volume is lost is not on any standby. Synchronous
replication (`synchronous_standby_names`) is not managed by pgop yet and is
planned for a later milestone.

## Read-your-writes

A standby can lag behind the primary. An application that must read its own
writes from `<cluster>-ro` can compare WAL positions:

```sql
-- on the primary, after the write:
SELECT pg_current_wal_lsn();            -- e.g. 0/3000148
-- on the standby, before the read; wait/retry until true:
SELECT pg_last_wal_replay_lsn() >= '0/3000148';
```

Otherwise, send reads that must see the latest writes to `<cluster>`.

## Scaling

- **Up:** raise `spec.replicas`. The first time a Cluster goes from 1 to more
  instances, its pod template gains the replication settings (init container,
  managed `pg_hba.conf`, `primary_conninfo`), so **the primary restarts
  once**. The operator waits until the primary runs the new template before it
  adds the first standby. Later scale-ups do not restart the primary.
- **Down:** lower `spec.replicas`. The StatefulSet removes the highest pods
  first. Once a removed standby's pod is gone, the operator deletes its data
  PVC and its replication slot. The primary's PVC (`data-<cluster>-0`) is
  never deleted by scaling.
- Scaling back to 1 keeps the replication settings in the pod template, so it
  does not restart the primary; `<cluster>-ro` is deleted.
- Clusters that never had more than one instance keep exactly the pod
  template they had before replication existed: upgrading the operator does
  not restart them. (Their read-write Service selector is narrowed to the
  primary pod in place, which does not change routing.)

## Updates and restarts

Changes that alter the pod template (image, resources, TLS settings,
restart-only parameters) roll the StatefulSet from the highest ordinal down:
standbys restart first, the primary last. Restarting the primary is a short
read-write outage; the standbys keep serving reads and reconnect afterwards.

## Monitoring

- `status.readyInstances`, `status.currentPrimary`, `status.readOnlyEndpoint`.
- The `ReplicationHealthy` condition is `True` (reason `Streaming`) when every
  standby streams. Otherwise it is `False` with reason `StandbyNotStreaming`
  (naming the standbys that are not streaming: still cloning, catching up or
  disconnected), `WaitingForPrimary` or `ReplicationError`. It never affects
  `Available`, which is `True` once every instance is ready.
- On the primary:

  ```sql
  SELECT application_name, state, sent_lsn, replay_lsn, replay_lag FROM pg_stat_replication;
  SELECT slot_name, active, wal_status, restart_lsn FROM pg_replication_slots;
  ```

## Failure scenarios

### Standby down

The primary keeps the standby's WAL in its slot (up to
`max_slot_wal_keep_size`) and the standby catches up when it returns. If its
slot was invalidated (`wal_status = 'lost'`), re-clone it.

### Re-cloning a standby

Delete the standby's PVC and pod; the StatefulSet recreates both and the init
container clones the primary again:

```sh
kubectl delete pvc data-<cluster>-<n> --wait=false
kubectl delete pod <cluster>-<n>
```

### Primary down

Writes fail until Kubernetes restarts the primary pod (on its node, or
elsewhere if the volume can move). Standbys keep serving reads. There is no
promotion.

### Primary volume lost

If `data-<cluster>-0` is lost while standbys are still running, the primary's
init container refuses to run `initdb` (the pod stays in `Init` with a message
in the `pgop-bootstrap` logs), because an empty new primary would discard the
data the standbys hold. Recover the primary's volume from a backup, or, to
start over, scale the Cluster to 1 instance (removing the standbys) first.

## Backups and restores

Backups always run against the primary (`<cluster>` Service). A
[physical restore](restores.md) replaces the primary's data, which the
standbys no longer match: scale the Cluster to 1 instance before the restore
(this removes the standbys and their volumes), restore, then scale up again so
the standbys are cloned from the restored primary.

## Limitations

- No automated failover, and no switchover yet (see above).
- Asynchronous replication only.
- `wal_level` is reserved and stays `replica`, so logical decoding / logical
  replication slots are not available.
- Pods are not spread across nodes by pgop (no anti-affinity yet); on a
  single node, the standbys add read capacity but no protection against the
  node failing.
- Individual standbys cannot be addressed through a Service; `<cluster>-ro`
  load-balances per connection.
