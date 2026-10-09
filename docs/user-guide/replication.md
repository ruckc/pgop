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
- The read-only Service selects `pgop.ruck.io/role: replica` **and**
  `pgop.ruck.io/streaming: "true"`. The operator sets both labels (the
  StatefulSet pod template cannot carry per-pod labels) from
  `pg_stat_replication` on the primary: a standby joins `<cluster>-ro` once it
  streams, and leaves it when it stops streaming (it is cloning or catching
  up, its slot was invalidated, it holds data of another primary, or it cannot
  authenticate), so it does not serve stale data indefinitely. While the
  primary is down the labels are left as they are, so the standbys keep
  serving (possibly stale) reads. Labels are updated within about 30 seconds,
  and only while the operator runs.
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

The slot of a standby is created when its pod is created, not ahead of time,
so a slot never holds back WAL for a standby that is still waiting for its
predecessors to start. While a standby clones, its WAL stream uses the slot,
so the slot keeps up during a normal clone.

If a slot is invalidated anyway (`wal_status = 'lost'`, the standby was down
or behind by more than `max_slot_wal_keep_size`), the operator drops and
re-creates it once it is not in use, records a `ReplicationSlotInvalidated`
Warning event, names the standby in the `ReplicationHealthy` message and takes
it out of `<cluster>-ro`. The standby itself cannot catch up any more and must
be [re-cloned](#re-cloning-a-standby).

`wal_level`, `max_wal_senders`, `max_replication_slots`, `hot_standby`,
`primary_conninfo` and `primary_slot_name` are reserved and cannot be set in
`spec.parameters`. `wal_level` stays `replica` and `hot_standby` `on`;
`max_wal_senders` and `max_replication_slots` are set to 32 (fixed, so
scaling never restarts the pods; enough for two concurrent clones per standby
at 10 instances). All other parameters, and TLS settings, apply to every
instance.

The default `max_slot_wal_keep_size` is a server option in the pod template:
changing `storage.size` (without setting `max_slot_wal_keep_size` in
`spec.parameters`) restarts the pods once.

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

- `status.ready` and the `Available` condition follow the **primary only**: a
  standby that is cloning, down or stuck never makes the Cluster unavailable,
  never turns `TLSReady` off or downgrades the `sslmode` published to clients
  from `verify-full`, and never blocks Role and Database reconciles.
- `status.readyInstances`, `status.currentPrimary`, `status.readOnlyEndpoint`.
- The `ReplicationHealthy` condition is `True` (reason `Streaming`) when every
  standby streams. Otherwise it is `False` with reason `StandbyNotStreaming`
  (naming the standbys that are not streaming: still cloning, catching up or
  disconnected), `WaitingForPrimary` or `ReplicationError`. It never affects
  `Available`.
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

Once the operator has reached the primary of a Cluster with standbys, it
records in the ConfigMap `<cluster>-hba` (key `pgop-initialized`, mounted into
the init container) that the Cluster holds data. The same happens when
standby volumes exist but the primary's does not (for example a Cluster
recreated after only `data-<cluster>-0` was deleted). From then on, if
`data-<cluster>-0` is lost or replaced by an empty volume, the primary's init
container **refuses to run `initdb`**: the pod stays in `Init` with a message
in the `pgop-bootstrap` logs, instead of starting a new, empty database next
to standbys (or retained volumes) holding the real data. Independently, it
also refuses while standbys answer on `<cluster>-ro`.

Recover the primary's volume from a backup. To deliberately start over with an
empty database, set the annotation `pgop.ruck.io/allow-primary-init: "true"`
on the Cluster (the operator removes the marker), let the primary initialize,
and **remove the annotation again**: while it is set, the guard stays off
(the marker is never written). Standbys that hold data of the old primary
then cannot stream (they stay out of `<cluster>-ro`) and must be re-cloned.

### Changing the replication password

Editing `replication-password` in `<cluster>-credentials` sets the new
password on the primary and then restarts the standby pods **one at a time**
(highest ordinal first; the next one only once the previous one streams again,
or after 10 minutes), so `<cluster>-ro` keeps serving from the others. The
primary is not restarted. The rollout is recorded in the
`pgop.ruck.io/replication-password-rollout` annotation of the Secret.

### Manual scaling of the StatefulSet

The operator owns `spec.replicas` of the StatefulSet: a manual
`kubectl scale statefulset` is reverted on the next reconcile (also from 0).

### Restores and the password-sync hook

Every pod runs a postStart hook that sets the operator's password in the
database to the one in `<cluster>-credentials`. On Clusters with standbys the
hook skips instances in recovery (standbys, and a server recovering a
physical backup), where `ALTER ROLE` would fail. Known limitations:

- **Single-instance Clusters** (that never had standbys) keep the original
  hook, which does not skip recovery, so their pod template stays unchanged
  on operator upgrade. A [physical restore](restores.md) to a point in time
  leaves the server paused in recovery (pgBackRest `--target-action=pause`),
  the hook fails and the container is restarted in a loop. Workaround: finish
  recovery before starting the pod normally (restore with
  `--target-action=promote`, or remove `recovery.signal` / run
  `SELECT pg_wal_replay_resume()` in a debug pod), or temporarily scale the
  Cluster to 2 instances so the recovery-aware hook is used. A fix is tracked
  separately.
- **Clusters with standbys:** after a restore, the hook skipped the password
  sync while the server was recovering, so the primary may reject the
  operator password (`28P01`) once recovery ends. The operator cannot tell
  from a failed login whether the server is still recovering, so it restarts
  the primary pod and retries with a growing delay (after 5, 10, 20 and 40
  minutes; at most 5 restarts per password value, recorded in the
  `pgop.ruck.io/password-sync-restart` annotation of the credentials Secret,
  which is cleared once the login works). A restart while the server is still
  paused in recovery does not sync the password; resume or promote it, and a
  later attempt (or deleting the primary pod by hand) completes the sync.

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
