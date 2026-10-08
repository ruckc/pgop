# Clusters

A Cluster resource represents a PostgreSQL instance managed by the operator.

## Overview

The Cluster controller:

1. Creates a Kubernetes Secret with auto-generated credentials
2. Deploys a StatefulSet running PostgreSQL
3. Creates a Service for client connections
4. Manages persistent storage for data

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
| `replicas` | int | `1` | Number of instances (currently only 1 supported) |
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

## Status

| Field | Description |
|-------|-------------|
| `ready` | Whether the cluster is ready to accept connections |
| `endpoint` | Service endpoint (hostname:port) |
| `secretName` | Name of the credentials secret |
| `tlsSecretHash` | Hash of the certificate the server was last confirmed to present (TLS only) |
| `conditions` | Detailed status conditions (`Available`, `ExistingVolume`, `TLSReady`) |

## Storage Retention

Each Cluster stores its data in a PersistentVolumeClaim named
`data-<cluster-name>-0`, created by the StatefulSet. What happens to that PVC
when the Cluster is deleted is controlled by `spec.storage.retainPolicy`:

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
  `Retain`, so scaling never destroys data.
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
```

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

Any Docker image compatible with the official PostgreSQL image environment variables:

- `postgres:18`
- `postgres:17`
- `postgres:16`
- `postgres:15`
- `postgres:14`
- Custom images that support `POSTGRES_USER` and `POSTGRES_PASSWORD` env vars

For extensions that ship outside the base image (e.g. PostGIS, TimescaleDB),
set `spec.image` to an image that bundles them, such as `postgis/postgis:18-3.5`.
See [Databases → Extensions](databases.md#common-extensions).

## Connecting: Endpoint & TLS

- Apps connect through the Service `<cluster-name>.<namespace>.svc.cluster.local`
  on `spec.port` (default `5432`). The same value is published on
  `status.endpoint`.
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
  (`tls.crt`, `tls.key`, `ca.crt`).
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
owned by the Cluster, for the same four DNS names as the self-managed
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
(the operator connects with `sslmode=verify-full` to that name) and allow
server authentication. Add any other names your clients use, such as
`<cluster>.<namespace>.svc` and `<cluster>`.

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
4. **Confirms TLS is live:** once the pod is ready, the operator performs a
   TLS handshake against the Service and checks that the server presents the
   certificate from the Secret. Then `TLSReady` becomes `True` (reason
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
changes the pod template, so the single PostgreSQL pod **restarts** once.

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
certificate files on reload. Kubelet propagates Secret updates into the pod
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
  is the name `verify-full` checks against the certificate.

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

- `pgbackrest restore` writes the data directory directly and opens no
  database connection.
- The backup Job reaches the database host through pgBackRest's own remote
  protocol (`--pg1-host`), not through PostgreSQL. pgop does not run a
  pgBackRest server in the Cluster pod, so this channel, and pgBackRest TLS
  for it, is not set up by pgop.

The S3 repository connection verifies the endpoint's certificate by default
(pgBackRest `repo1-s3-verify-tls`).

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
