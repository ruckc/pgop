# Clusters

A Cluster resource represents a PostgreSQL primary, optionally with streaming
read replicas, managed by the operator.

## Overview

The Cluster controller:

1. Creates a Kubernetes Secret with auto-generated credentials
2. Deploys a StatefulSet running PostgreSQL (pod `<cluster>-0` is the primary)
3. Creates the Service `<cluster>` for read-write connections, which only ever
   routes to the primary, and, with `replicas` > 1, the Service
   `<cluster>-ro` for read-only connections to the standbys
4. Manages persistent storage for data

See [Replication](replication.md) for read replicas.

## Example

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: production-db
  namespace: databases
spec:
  image: postgres:18
  replicas: 1
  port: 5432
  storage:
    size: 100Gi
    storageClassName: fast-ssd
  resources:
    requests:
      memory: "1Gi"
      cpu: "500m"
    limits:
      memory: "4Gi"
      cpu: "2"
```

## Spec Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `image` | string | `postgres:18` | PostgreSQL container image |
| `replicas` | int | `1` | Number of instances, 1-10: one primary plus asynchronous streaming standbys. See [Replication](replication.md) |
| `port` | int | `5432` | PostgreSQL listen port |
| `storage.size` | string | `1Gi` | PVC size (e.g., "10Gi") |
| `storage.storageClassName` | string | - | Storage class name |
| `storage.retainPolicy` | string | `Retain` | `Retain` keeps the PVC when the Cluster is deleted; `Delete` removes it. See [Storage retention](#storage-retention) |
| `resources` | ResourceRequirements | - | CPU/memory requests/limits |
| `tls` | object | - | Setting `tls` (even `tls: {}`) enables server TLS. See [TLS](#tls) |
| `tls.secretName` | string | - | Your own Secret with `tls.crt`, `tls.key`, `ca.crt`. Mutually exclusive with `issuerRef` |
| `tls.issuerRef.name` | string | - | cert-manager issuer; the operator creates the `Certificate`. Mutually exclusive with `secretName` |
| `tls.issuerRef.kind` | string | `Issuer` | `Issuer`, `ClusterIssuer` or an external issuer kind |
| `tls.issuerRef.group` | string | `cert-manager.io` | Issuer API group (for external issuers) |
| `tls.requireTLS` | bool | `true` | Reject non-TLS TCP connections |
| `tls.minProtocolVersion` | string | `TLSv1.2` | `TLSv1.2` or `TLSv1.3` |
| `parameters` | map[string]string | - | PostgreSQL configuration parameters. See [Parameters](#parameters) |
| `rolePolicy.allowedAttributes` | []string | `[]` | Privileged attributes Roles may request: `createRole`, `replication`, `bypassRLS`. See [Role policy](#role-policy) |
| `rolePolicy.allowedPredefinedRoles` | []string | `[]` | Predefined `pg_*` roles Roles may be members of. See [Role policy](#role-policy) |
| `rolePolicy.allowedExistingRoles` | []string | `[]` | Roles not managed by a Role of this Cluster that Roles may be members of and Databases may grant privileges to. See [Role policy](#role-policy) |
| `rolePolicy.adoptableRoles` | []string | `[]` | Existing roles a Role may take over. See [Role policy](#role-policy) |
| `rolePolicy.adoptableDatabases` | []string | `[]` | Existing databases a Database may take over. See [Role policy](#role-policy) |
| `rolePolicy.allowedExtensions` | []string | `[]` | Untrusted extensions Databases may install (trusted ones are always allowed). See [Role policy](#role-policy) |

## Status

| Field | Description |
|-------|-------------|
| `ready` | Whether the cluster is ready to accept connections: the primary pod is ready (standbys never affect it) |
| `endpoint` | Read-write Service endpoint (hostname:port); always the primary |
| `readOnlyEndpoint` | Read-only Service endpoint `<cluster>-ro` (hostname:port); only with `replicas` > 1 |
| `readyInstances` | Number of ready PostgreSQL pods |
| `currentPrimary` | Pod running the primary (`<cluster>-0`) |
| `secretName` | Name of the credentials secret |
| `tlsSecretHash` | Hash of the certificate the server was last confirmed to present (TLS only) |
| `parametersHash` | Hash of the generated configuration file the server was last asked to reload (`parameters` only) |
| `pendingRestart` | Parameters the server reports as needing a restart (`pg_settings.pending_restart`) |
| `conditions` | Detailed status conditions (`Available`, `ExistingVolume`, `TLSReady`, `ParametersApplied`, `ReplicationHealthy`) |

## Parameters

`spec.parameters` sets PostgreSQL server configuration parameters (GUCs):

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: production-db
spec:
  parameters:
    shared_buffers: 1GB
    work_mem: 16MB
    max_connections: "200"
    shared_preload_libraries: pg_stat_statements
    pg_stat_statements.track: all
```

Values are strings; quote numbers in YAML (`"200"`). Keys are PostgreSQL
parameter names (including `extension.setting` names), matched
case-insensitively like PostgreSQL does.

### How parameters are applied

- The operator renders the parameters into `postgresql.conf` in the ConfigMap
  `<cluster>-config`, owned by the Cluster, and starts the server with
  `-c config_file=/etc/pgop/config/postgresql.conf`. That file first includes
  the data directory's own `postgresql.conf` (`include_if_exists`), so the
  image and `initdb` defaults still apply and `spec.parameters` overrides them.
- Changing a parameter only changes the ConfigMap, not the pod template. The
  kubelet updates the mounted file within about a minute; the operator waits
  for the server to see the new file (`pg_file_settings`) and then runs
  `pg_reload_conf()`.
- If a changed parameter only takes effect on a restart
  (`pg_settings.pending_restart`, for example `shared_buffers`,
  `max_connections` or `shared_preload_libraries`), the parameters are listed
  in `status.pendingRestart` and the operator restarts the pod once, through
  the pod template annotation `pgop.ruck.io/parameters-restart`. With a single
  instance this means a short outage; with standbys the StatefulSet restarts
  the standbys first and the primary last (still a short read-write outage).
- All instances (primary and standbys) mount the same generated file; the
  operator reloads every ready instance.
- The `ParametersApplied` condition reports progress:

  | Reason | Meaning |
  |--------|---------|
  | `Applied` (`True`) | Every parameter is in effect. |
  | `WaitingForServer` | The pod is not ready, a rollout is in progress, or the operator cannot connect. |
  | `WaitingForSync` | The server does not see the current file yet (kubelet sync delay), or has not restarted onto it. |
  | `Reloading` | `pg_reload_conf()` was run; the result is being checked. |
  | `PendingRestart` | A parameter needs a restart; the pod is being restarted. |
  | `InvalidParameter` | The server rejects a name or value (the message says which). Nothing is reloaded or restarted until it is fixed. |
  | `OverriddenByAlterSystem` | `ALTER SYSTEM` overrides a parameter (see below). |

  The condition never affects `Available`/`ready`: a cluster stays usable
  while parameters are being applied or are invalid.

Leaving `parameters` unset (or empty) keeps the image's configuration and does
not change the pod at all, so upgrading the operator does not restart existing
Clusters. Adding the first parameter, or removing the last one, changes the
pod template and restarts the pod once.

### Reserved parameters

Parameters the operator manages are rejected when the Cluster is created or
updated:

| Parameters | Why |
|------------|-----|
| `listen_addresses`, `port`, `unix_socket_directories` | Service, probes and the operator's own connections depend on them (use `spec.port`) |
| `config_file`, `data_directory`, `hba_file`, `ident_file`, `external_pid_file` | File locations set up by the operator and the image |
| `include`, `include_dir`, `include_if_exists` | Would read arbitrary files |
| `ssl`, `ssl_cert_file`, `ssl_key_file`, `ssl_min_protocol_version` | Controlled by [`spec.tls`](#tls) |
| `archive_mode`, `archive_command`, `archive_library`, `restore_command` | Operator-managed WAL archiving ([physical backups](backups.md#physical-backups-pgbackrest)) |
| `wal_level`, `max_wal_senders`, `max_replication_slots`, `hot_standby` | Streaming [replication](replication.md) depends on them: `wal_level` stays `replica`, `hot_standby` stays `on`, and Clusters that have had standbys run with `max_wal_senders` and `max_replication_slots` set to `32` |
| `primary_conninfo`, `primary_slot_name` | Set by the operator for standbys |

`max_slot_wal_keep_size` can be set; with standbys it otherwise defaults to a
quarter of `storage.size` (see [Replication](replication.md#replication-slots)).

Other TLS settings such as `ssl_ciphers` or `ssl_max_protocol_version` can be
set. Values containing line breaks are reported as `InvalidParameter`.

### ALTER SYSTEM

`ALTER SYSTEM` writes `postgresql.auto.conf` in the data directory, which
PostgreSQL reads after the main configuration file, so it overrides
`spec.parameters`. The operator checks for this every few minutes and sets
`ParametersApplied=False` with reason `OverriddenByAlterSystem` (and a Warning
event) naming the affected parameters. Run `ALTER SYSTEM RESET <name>` and
`SELECT pg_reload_conf()` to hand the parameter back to `spec.parameters`.
Per-database and per-role settings (`ALTER DATABASE ... SET`,
`ALTER ROLE ... SET`) are not tracked.

### Extensions and libraries

The operator does not install libraries. `shared_preload_libraries` must name
libraries present in the image: `pg_stat_statements` and the other contrib
modules ship with the official image, others (for example `pgaudit`) need a
custom image. Until the restart that loads a library completes, a Database
whose `CREATE EXTENSION` needs it keeps retrying.

### Recovering from a bad value

A value the server cannot start with (for example a misspelled
`shared_preload_libraries` entry) makes the pod crash-loop after the restart.
Fix the value in `spec.parameters`: only the ConfigMap changes, and the next
container restart reads the corrected file, so no manual pod deletion is
needed. (If you instead remove *all* parameters, the pod template changes and
a StatefulSet stuck on a crash-looping pod may need `kubectl delete pod` to
roll forward.)

## Role Policy

pgop creates roles, grants memberships and installs extensions as a superuser
on behalf of whoever can create Role and Database resources. `spec.rolePolicy`
decides how far that goes. It lives on the Cluster so that only someone with
RBAC to **edit the Cluster** can widen it; RBAC to create Roles or Databases
alone does not make anyone superuser-equivalent.

```yaml
spec:
  rolePolicy:
    allowedAttributes: [bypassRLS]          # createRole, replication, bypassRLS
    allowedPredefinedRoles: [pg_monitor]    # see the list below
    allowedExistingRoles: [analytics_ro]    # roles created outside pgop
    adoptableRoles: [legacy_app]            # existing roles a Role may take over
    adoptableDatabases: [legacy_db]         # existing databases a Database may take over
    allowedExtensions: [postgis, file_fdw]  # untrusted extensions
```

Without `rolePolicy` (the default):

- Roles are never superusers (there is no such field), and get no
  `CREATEROLE`, `REPLICATION` or `BYPASSRLS`. A Role requesting one reports
  `Available=False` with reason `RolePolicyViolation` and gets none of them;
  an existing role is altered down.
- Roles cannot be members of any predefined `pg_*` role, of a superuser role,
  or of a role with an attribute the policy does not allow (reason
  `MembershipNotAllowed`; memberships pgop granted earlier are revoked). See
  [Roles: membership policy](roles.md#membership-policy).
- Roles cannot be members of roles that no Role of this Cluster manages (a
  DBA's or a bootstrap Job's roles), unless `allowedExistingRoles` lists them.
  The same list decides which such roles Databases may grant privileges to
  ([`grants`, `schemas[].grants`](databases.md#grantee-policy); reason
  `GranteeNotAllowed`). Roles managed by Roles of the same Cluster can always
  be joined and granted to: the
  Cluster's namespace is one trust domain.
- Roles and Databases only manage roles and databases they created (or
  recorded in their status). Taking over an existing one needs
  `adoptableRoles` / `adoptableDatabases`: **to take over an existing role or
  database, a Cluster editor lists it; Role and Database writers cannot.**
  This is also how a Role or Database re-created without its status (for
  example from Git after a restore) regains its object. Only list objects you
  are willing to hand to the namespace's writers: pgop refuses superuser and
  forbidden-member roles, but cannot see everything an object's previous
  owner may have prepared (functions, grants, ownerships).
- Databases can only install extensions the server marks as trusted (reason
  `ExtensionNotAllowed` otherwise). See
  [Databases: extension policy](databases.md#extension-policy).

`allowedPredefinedRoles` accepts these roles (others are rejected by the API
server):

| Role | Gives | Notes |
|------|-------|-------|
| `pg_monitor`, `pg_read_all_settings`, `pg_read_all_stats`, `pg_stat_scan_tables` | Read server settings and statistics views | All databases |
| `pg_signal_backend` | Cancel or terminate other non-superuser sessions | Other Roles' sessions included |
| `pg_signal_autovacuum_worker` (PG 18) | Signal autovacuum workers | |
| `pg_checkpoint` (PG 15), `pg_use_reserved_connections` (PG 16) | Operational | |
| `pg_maintain` (PG 17) | `VACUUM`, `ANALYZE`, `REINDEX`, `REFRESH`, `CLUSTER`, `LOCK TABLE` on every table | All databases; can block other workloads |
| `pg_create_subscription` (PG 16) | Create logical replication subscriptions | Outbound connections from the server |
| `pg_read_all_data`, `pg_write_all_data` | Read / write every table, bypassing privileges | **All databases of the Cluster**, other teams' included |

Allowing `pg_monitor` also allows the roles PostgreSQL makes it a member of
(`pg_read_all_settings`, `pg_read_all_stats`, `pg_stat_scan_tables`).

The Cluster also owns the Secret `<cluster>-marker-key`: the random key that
signs the ownership markers pgop stores on the roles and databases it creates
(see [Roles: ownership](roles.md#ownership-of-the-postgresql-role)). It is
never mounted into a pod and cannot be used as a `passwordSecretRef`. If it is
deleted, a new key is generated and the roles and databases recorded in the
resources' status are re-marked; the markers never authorize a take-over, so
nothing else depends on the key.

`pg_execute_server_program`, `pg_read_server_files` and
`pg_write_server_files` give shell or file access on the server and can never
be allowed.

Allowing `createRole` on PostgreSQL 15 or older is close to allowing
superuser: there, `CREATEROLE` can grant membership in any non-superuser role,
including `pg_execute_server_program`. PostgreSQL 16 and later limit it to
roles the role created itself.

Changing `rolePolicy` re-reconciles the Cluster's Roles and Databases: removing
an entry takes the attribute away, revokes the membership pgop granted, or
stops installing the extension (extensions already installed are not
dropped).

!!! note "Upgrade / breaking change"
    Earlier versions applied whatever a Role or Database requested, including
    `superuser: true`. Roles that relied on `createRole`, `replication`,
    `bypassRLS` or `pg_*` memberships, and Databases using untrusted
    extensions, need the matching `rolePolicy` entries; see
    [Roles: upgrade](roles.md#upgrade-breaking-changes).

## Storage Retention

Each instance stores its data in a PersistentVolumeClaim named
`data-<cluster-name>-<n>` (`-0` for the primary), created by the StatefulSet.
What happens to these PVCs when the Cluster is deleted is controlled by
`spec.storage.retainPolicy`:

| Value | Behaviour on Cluster deletion |
|-------|-------------------------------|
| `Retain` (default) | The PVC and its data are **kept**. |
| `Delete` | The PVC is deleted together with the Cluster. Whether the underlying PersistentVolume and its data are destroyed depends on the StorageClass `reclaimPolicy`. |

```yaml
spec:
  storage:
    size: 10Gi
    retainPolicy: Delete
```

!!! warning "Deleting a Cluster keeps its data by default"
    With the default `Retain`, `kubectl delete cluster <name>` and
    `helm uninstall` of a chart that created the Cluster leave the PVC behind.
    A Cluster recreated later **with the same name in the same namespace**
    starts PostgreSQL on that old data directory (old databases, roles and
    data). Delete the PVC by hand (`kubectl delete pvc data-<name>-0`) if you
    want a clean start, or set `retainPolicy: Delete`.

Notes:

- `retainPolicy` maps onto the StatefulSet
  `persistentVolumeClaimRetentionPolicy.whenDeleted`. `whenScaled` is always
  `Retain`, so scaling the StatefulSet never destroys the primary's data (even
  a manual `kubectl scale --replicas=0`).
- Lowering `spec.replicas` removes standbys: once a removed standby's pod is
  gone, the operator deletes its PVC (never `data-<cluster>-0`) and its
  replication slot, so a standby added later is cloned afresh.
- **Requires Kubernetes 1.27+** (the field is beta and enabled by default since
  1.27, GA since 1.32). On older clusters `Delete` is silently ignored and PVCs
  are always retained.
- The field is mutable. Changing it updates the existing StatefulSet in place;
  it only takes effect when the Cluster is later deleted.
- `kubectl delete cluster <name> --cascade=orphan` leaves the StatefulSet and
  PVC behind regardless of `retainPolicy`.

### Starting on a pre-existing volume

When the operator creates a Cluster's StatefulSet and finds that
`data-<name>-0` already exists, it sets the `ExistingVolume` condition to
`True` (reason `PreExistingPVC`) and records a `PreExistingPVC` Warning event on
the Cluster. Otherwise the condition is `False` (reason `NewVolume`). The
condition is evaluated only once, at StatefulSet creation.

This is informational: the regenerated operator password in
`<name>-credentials` is synced into the existing database automatically on pod
start, so the operator can still connect. Application roles and databases from
the old data directory remain as they were.

```sh
kubectl get cluster <name> -o jsonpath='{.status.conditions[?(@.type=="ExistingVolume")]}'
kubectl get events --field-selector reason=PreExistingPVC
```

## Credentials Secret

The operator creates `<cluster-name>-credentials` containing:

```yaml
data:
  username: pgop_operator           # Superuser username
  password: <generated>             # Superuser password
  host: <cluster-name>.<ns>.svc     # Service hostname
  port: "5432"                      # PostgreSQL port
  database: postgres                # Default database
  sslmode: disable                  # verify-full while TLS is active (see TLS)
  uri: postgresql://pgop_operator:<password>@<host>:5432/postgres?sslmode=disable
  ca.crt: <PEM>                     # only while TLS is active
  replication-password: <generated> # password of pgop_replicator (standbys)
```

`replication-password` is the password standbys use to stream from the
primary as `pgop_replicator`. It is generated for every Cluster (also added to
Secrets created by older operator versions) and only used once the Cluster
has more than one instance.

## Using Credentials in Applications

Prefer a per-app [Database](databases.md#connection-secret) Secret
(`<database>-<owner>-credentials`), which includes the `database` key. Project
the individual keys as env vars and assemble the DSN in your app:

```yaml
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: app
          env:
            - name: PGHOST
              valueFrom: { secretKeyRef: { name: myapp-app-user-credentials, key: host } }
            - name: PGPORT
              valueFrom: { secretKeyRef: { name: myapp-app-user-credentials, key: port } }
            - name: PGUSER
              valueFrom: { secretKeyRef: { name: myapp-app-user-credentials, key: username } }
            - name: PGPASSWORD
              valueFrom: { secretKeyRef: { name: myapp-app-user-credentials, key: password } }
            - name: PGDATABASE
              valueFrom: { secretKeyRef: { name: myapp-app-user-credentials, key: database } }
```

The standard `PG*` env vars are consumed automatically by `libpq`-based clients
(`psql`, most drivers). For a URL-style DSN, compose
`postgres://$(PGUSER):$(PGPASSWORD)@$(PGHOST):$(PGPORT)/$(PGDATABASE)`.

## Supported Images

Any Docker image compatible with the official PostgreSQL image environment
variables. **PostgreSQL 14 is the oldest supported major version.**

- `postgres:18`
- `postgres:17`
- `postgres:16`
- `postgres:15`
- `postgres:14`
- Custom images that support `POSTGRES_USER` and `POSTGRES_PASSWORD` env vars

For extensions that ship outside the base image (e.g. PostGIS, TimescaleDB),
set `spec.image` to an image that bundles them, such as `postgis/postgis:18-3.5`.
See [Databases → Extensions](databases.md#common-extensions).

While a [physical Backup](backups.md#physical-backups-pgbackrest) names the
Cluster, the official `postgres:<major>` image (16, 17, 18) is replaced by
pgop's Postgres+pgBackRest image `ghcr.io/ruckc/pgop-postgres:<major>-<pgbackrest>`;
a custom image must contain pgBackRest itself (see
[Backups → Images](backups.md#images)).

## Connecting: Endpoint & TLS

- Apps connect through the Service `<cluster-name>.<namespace>.svc.cluster.local`
  on `spec.port` (default `5432`). It always routes to the primary. The same
  value is published on `status.endpoint`.
- With `replicas` > 1, read-only queries can use
  `<cluster-name>-ro.<namespace>.svc.cluster.local`
  (`status.readOnlyEndpoint`), which load-balances over the standbys. See
  [Replication](replication.md).
- The `<cluster-name>-credentials` Secret above holds the **operator** superuser
  (`pgop_operator`). Application workloads should connect using a per-app
  [Role](roles.md)/[Database](databases.md) credentials Secret rather than the
  operator superuser.
- **TLS:** off by default; the operator then connects with `sslmode=disable`.
  Set `spec.tls` to enable it (next section). Each credentials Secret carries
  the `sslmode` clients should use.

## TLS

Setting `spec.tls` turns on TLS for the PostgreSQL server. The server
certificate comes from one of three sources:

| `spec.tls` | Certificate source | Secret mounted into the pod |
|------------|--------------------|-----------------------------|
| `tls: {}` (neither field) | **Self-managed CA**: the operator generates a CA and a server certificate, and renews and rotates them | `<cluster>-server-cert` |
| `issuerRef: {name: …}` | **cert-manager**: the operator creates and owns a `Certificate` `<cluster>-server` | `<cluster>-server-tls` (written by cert-manager) |
| `secretName: …` | **Your own Secret** | the named Secret |

```yaml
spec:
  tls: {}                           # self-managed CA, or:
  # tls:
  #   issuerRef: {name: my-ca-issuer, kind: Issuer}   # cert-manager, or:
  #   secretName: production-db-tls                   # your own Secret
  #   requireTLS: true              # default: reject non-TLS TCP connections
  #   minProtocolVersion: TLSv1.2   # or TLSv1.3
```

`secretName` and `issuerRef` are mutually exclusive (rejected by the API
server). Switching between the sources is allowed: the pod restarts once onto
the new certificate, and the resources the operator created for the previous
source (the `Certificate` and its Secret, or the self-managed Secrets) are
deleted afterwards.

**Clusters without `spec.tls` are not affected.** `requireTLS` defaults to
`true`, but the default only applies inside a `tls` block: a Cluster that does
not set `spec.tls` keeps running exactly as before (no pod changes, no restart,
`sslmode=disable`). Upgrading the operator therefore never turns TLS on, or
starts rejecting connections, for existing Clusters.

### Self-managed CA

With `tls: {}` the operator needs nothing else (no cert-manager):

- It generates an ECDSA P-256 **CA** valid for 10 years and stores it in the
  Secret `<cluster>-ca` (`tls.crt`/`tls.key`: the CA, `ca.crt`: the trust
  bundle clients use). The CA private key is never mounted into the pod.
- It issues an ECDSA P-256 **server certificate** valid for 90 days for
  `<cluster>.<namespace>.svc.cluster.local`, `<cluster>.<namespace>.svc`,
  `<cluster>.<namespace>` and `<cluster>`, stored in `<cluster>-server-cert`
  (`tls.crt`, `tls.key`, `ca.crt`). With `replicas` > 1 the same four names
  of the read-only Service `<cluster>-ro` are added (the certificate is
  re-issued, without a restart, when the Cluster is scaled across 1).
- Both Secrets are owned by the Cluster and deleted with it (or when the
  Cluster stops using the self-managed CA). A pre-existing Secret of the same
  name that the Cluster does not own is never overwritten; `TLSReady` reports
  `InvalidTLSSecret` instead.
- **Renewal:** the server certificate is renewed 30 days before it expires and
  loaded with `pg_reload_conf()`, without a restart.
- **CA rotation** happens in two steps so that clients never see a server
  certificate from a CA they do not trust yet. Three years before the CA
  expires, a new CA is generated and **added** to the `ca.crt` trust bundle in
  every credentials Secret, while the server certificate is still issued by
  the old CA. One year before expiry the new CA takes over and a new server
  certificate is issued from it. The old CA stays in the bundle until it
  expires, so the operator verifies (and reloads) the server throughout: no
  restart. Clients that mount `ca.crt` from a credentials Secret (see
  [Client configuration](#client-configuration)) pick up the new CA through
  kubelet's Secret refresh, two years before they need it.
- If `<cluster>-ca` is deleted, a new CA and server certificate are generated.
  The old CA is not trusted any more, so the operator restarts the pod (see
  [Certificate rotation](#certificate-rotation)) and clients must pick up the
  new `ca.crt`.

### cert-manager (`issuerRef`)

```yaml
spec:
  tls:
    issuerRef:
      name: my-ca-issuer     # an Issuer in the Cluster namespace
      kind: Issuer           # default; or ClusterIssuer, or an external issuer kind
      # group: cert-manager.io   # default; set it for external issuers
```

The operator creates a cert-manager `Certificate` named `<cluster>-server`,
owned by the Cluster, for the same DNS names as the self-managed
certificate, with `secretName: <cluster>-server-tls`. cert-manager issues and
renews it; the Secret then goes through the same validation, mounting and
reload as a Secret you provide. The operator adds an owner reference to that
Secret so it is deleted with the Cluster (cert-manager does not delete the
Secrets of deleted Certificates by default).

- **The issuer must populate `ca.crt`** (CA, Vault and self-signed issuers
  do; ACME issuers do not).
- While the certificate is not issued yet, `TLSReady` is `False` with reason
  `CertificatePending` and the Certificate's `Ready` condition in the message;
  the StatefulSet is not touched until the Secret exists.
- **Without cert-manager** (the `cert-manager.io/v1` `Certificate` API is not
  installed), `TLSReady` is `False` with reason `CertManagerUnavailable`; the
  Cluster keeps running as before and is retried every minute, so installing
  cert-manager later picks it up. The operator itself starts and runs normally
  without cert-manager.
- A `Certificate` named `<cluster>-server` that the Cluster does not own is
  never modified (`CertificatePending` with an explanation).
- RBAC: the operator's ClusterRole includes `certificates.cert-manager.io`
  (create, get, list, watch, update, patch, delete).

### Your own Secret (`secretName`)

`secretName` names a Secret in the Cluster's namespace in the
`kubernetes.io/tls` layout:

| Key | Content |
|-----|---------|
| `tls.crt` | Server certificate (PEM), optionally followed by intermediates |
| `tls.key` | Private key (PEM) |
| `ca.crt` | CA that issued `tls.crt` (PEM). Required: the operator and clients verify against it |

The certificate must be valid for `<cluster>.<namespace>.svc.cluster.local`
(the operator and the standbys connect with `sslmode=verify-full` to that
name) and allow server authentication. Add any other names your clients use,
such as `<cluster>.<namespace>.svc` and `<cluster>`, and, for clients of the
standbys, `<cluster>-ro.<namespace>.svc.cluster.local`. Every instance serves
the same certificate.

A cert-manager `Certificate` produces exactly this layout (or let the
operator create it with [`issuerRef`](#cert-manager-issuerref)). Example with a
namespace-local CA:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: production-db-tls
  namespace: databases
spec:
  secretName: production-db-tls
  dnsNames:
    - production-db.databases.svc.cluster.local
    - production-db.databases.svc
    - production-db
  usages: [server auth, digital signature, key encipherment]
  issuerRef:
    name: my-ca-issuer     # a CA Issuer, so that ca.crt is populated
    kind: Issuer
```

Issuers that do not populate `ca.crt` (for example ACME) are not supported.

### What the operator does

1. **Validates the Secret** (whichever source it comes from) before touching
   the pod: all three keys present,
   `tls.crt`/`tls.key` form a key pair, and the certificate chains to `ca.crt`,
   is currently valid and names the Service host. On failure the `TLSReady`
   condition is `False` with reason `InvalidTLSSecret`, an `InvalidTLSSecret`
   Warning event is recorded, and the StatefulSet is **left unchanged** (for a
   new Cluster it is not created until the Secret is fixed). The Secret is
   watched, so fixing it resumes reconciliation.
2. **Configures the server:** mounts the Secret read-only at `/etc/pgop/tls`
   (mode `0640`, readable by the postgres group through `fsGroup`) and starts
   PostgreSQL with `ssl=on`, `ssl_cert_file`, `ssl_key_file` and
   `ssl_min_protocol_version`.
3. **Requires TLS** (`requireTLS: true`): creates a ConfigMap `<cluster>-hba`
   (owned by the Cluster) and starts PostgreSQL with `hba_file` pointing at it:

    ```
    local     all  all              trust
    hostssl   all  all  0.0.0.0/0   scram-sha-256
    hostssl   all  all  ::/0        scram-sha-256
    hostnossl all  all  all         reject
    ```

    Unix-socket connections (probes, the password-sync hook) are unaffected.
    `$PGDATA/pg_hba.conf` is never modified. With `requireTLS: false` the
    image's own `pg_hba.conf` is used and plaintext connections keep working.
    Clusters with standbys always use a managed `pg_hba.conf`, which also
    allows replication connections of `pgop_replicator` (over TLS when
    `requireTLS` is set); see [Replication](replication.md#pg_hbaconf).
4. **Confirms TLS is live:** once the pods are ready, the operator performs a
   TLS handshake against the Service (and against each standby pod) and checks
   that the server presents the certificate from the Secret. Then `TLSReady` becomes `True` (reason
   `TLSActive`) and `status.tlsSecretHash` is set.
5. **Connects securely:** while `TLSReady` is `True` the Role and Database
   controllers connect with `sslmode=verify-full`, verifying against `ca.crt`.
   While TLS is enabled but not yet confirmed (the pod is restarting onto TLS,
   or a rotated certificate is not loaded yet) they use `sslmode=prefer`, which
   still negotiates TLS when the server offers it.
6. **Publishes client settings:** the Cluster, Role and Database credentials
   Secrets get `sslmode` (`verify-full` while `TLSReady` is `True`, `prefer`
   while TLS is pending, `disable` without `spec.tls`), `ca.crt` (only while
   `TLSReady` is `True`) and a `uri`. Existing Secrets are updated in place;
   passwords are not changed.

Enabling or disabling TLS (or changing `requireTLS` / `minProtocolVersion`)
changes the pod template, so the PostgreSQL pods **restart** once (standbys
first, the primary last).

### Client configuration

A URI cannot carry the CA, so clients using `verify-full` mount `ca.crt` from
their credentials Secret and point libpq at it:

```yaml
env:
  - name: DATABASE_URL
    valueFrom: { secretKeyRef: { name: myapp-app-user-credentials, key: uri } }
  - name: PGSSLROOTCERT
    value: /etc/pgop-ca/ca.crt
volumeMounts:
  - name: pg-ca
    mountPath: /etc/pgop-ca
    readOnly: true
volumes:
  - name: pg-ca
    secret:
      secretName: myapp-app-user-credentials
      items: [{ key: ca.crt, path: ca.crt }]
```

`ca.crt` only exists while TLS is active; mark the volume `optional: true` if
the pod may start before that.

### Certificate rotation

Renewing a certificate does not restart the pod. When the Secret changes
(cert-manager and the self-managed CA renew certificates automatically), the
operator re-validates it and, if the running server still presents the old
certificate, runs `SELECT pg_reload_conf()`; PostgreSQL re-reads the
certificate files on reload. Standbys are checked and reloaded the same way,
each through its pod address (the certificate is still verified for the
Service name). Kubelet propagates Secret updates into the pod
with a delay of up to about a minute, so `TLSReady` is briefly `False` (reason
`CertificateReloading`) and the operator retries until the new certificate is
served.

The reload connection is verified against the **current** `ca.crt`, and the
operator never falls back to an unverified connection. If the old certificate
does not verify against the new `ca.crt` (the CA was replaced without an
overlap, or the old certificate already expired), a reload is impossible, so
the operator **restarts the pod** instead: it sets the pod template annotation
`pgop.ruck.io/tls-restart` to the hash of the new certificate, which rolls the
StatefulSet once. The restart happens at most once per certificate, so a
server that keeps presenting an unexpected certificate does not cause a
restart loop. The self-managed CA always overlaps old and new CAs and never
needs this restart, except after `<cluster>-ca` was deleted.

To replace a CA without a restart when you manage the certificate yourself,
put both the old and the new CA into `ca.crt` first, then switch `tls.crt` to
a certificate from the new CA.

### Turning TLS off

Removing `spec.tls` restarts the pod without `ssl=on` and without `hba_file`,
so PostgreSQL uses the `pg_hba.conf` in its data directory again (the image
default: password authentication over any address). The `<cluster>-hba`
ConfigMap is deleted, the `TLSReady` condition is removed, and the credentials
Secrets switch back to `sslmode=disable` and lose `ca.crt`. The operator's
`Certificate` (issuerRef) or self-managed CA and server Secrets are deleted;
turning the self-managed CA on again generates a new CA.

### Backups and restores

Logical backup Jobs (`pg_dump`, created from the `Backup` CronJobs) and logical
restore Jobs (`pg_restore`) connect the same way the operator does. They read
the Cluster credentials Secret when the Job pod starts:

- `PGSSLMODE` comes from the Secret's `sslmode` key: `verify-full` while
  `TLSReady` is `True`, `prefer` while TLS is pending, and `disable` without
  `spec.tls`;
- the Secret's `ca.crt` (the server CA) is mounted at `/etc/pgop/pg-ca/ca.crt`
  and `PGSSLROOTCERT` points to it;
- `PGHOST` is the Service name `<cluster>.<namespace>.svc.cluster.local`, which
  is the name `verify-full` checks against the certificate, and which only
  routes to the primary.

None of this is written into the CronJob, so turning TLS on or off, renewing
the certificate or rotating the CA needs no CronJob change: the next Job
simply uses the current settings. Each Job logs the `sslmode` it used
(`pg_dump: host=… sslmode=verify-full`).

The operator also keeps the backup CronJobs in step with what it generates.
CronJobs created by an older pgop version are updated when the operator
starts, and changes to the Cluster spec (for example its image or port) are
applied to its backup CronJobs. `spec.suspend` is left as you set it.

Physical (pgBackRest) backups and restores do not use libpq over the network,
so `spec.tls` does not change them:

- The backup Job reaches the data directory through the pgBackRest TLS server
  in the Cluster pod, with mutual TLS from a separate, operator-managed CA
  (see [Backups → pgBackRest TLS](backups.md#pgbackrest-tls)). pgBackRest
  itself connects to PostgreSQL over the pod's Unix socket.
- `pgbackrest restore` writes the data directory directly and opens no
  database connection.

The S3 repository connection always uses HTTPS and verifies the endpoint's
certificate (`caSecretRef` adds a private CA).

### Limitations

- While TLS is enabled but not yet confirmed (for example during the restart
  onto TLS), backup and restore Jobs use `sslmode=prefer`. The connection is
  encrypted but the server certificate is not verified, just as for the
  operator's own connections.
- An operator running outside the cluster (`make run`) cannot reach the
  Service DNS name, so `TLSReady` stays `False` (`WaitingForServer`).
- The self-managed CA's lifetimes (10 year CA, 90 day server certificate)
  and the requested DNS names are not configurable. Use `issuerRef` or
  `secretName` for other names (for example an external load balancer).

### FIPS

pgop does not configure FIPS mode itself. For a FIPS 140 deployment:

- **Operator:** build the operator binary with the Go FIPS 140 module
  (`GOFIPS140=v1.0.0` at build time, or `GODEBUG=fips140=on`/`only` at run
  time). The operator's TLS (verify-full connections and the certificate
  probe) then uses FIPS-approved algorithms only.
- **Server:** TLS on the server is provided by the PostgreSQL image's OpenSSL.
  Use an image whose OpenSSL runs a validated FIPS provider in FIPS mode.
- **Certificates:** use FIPS-approved key types and sizes (RSA ≥ 2048 or ECDSA
  P-256/P-384) and set `minProtocolVersion` to `TLSv1.2` or higher.
- **Authentication:** password authentication over TCP uses `scram-sha-256`.
  Avoid `md5` password hashes (not FIPS-approved); PostgreSQL 14+ defaults to
  SCRAM.
