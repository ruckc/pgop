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
| `tls.secretName` | string | - | Secret with `tls.crt`, `tls.key`, `ca.crt`. Setting `tls` enables server TLS. See [TLS](#tls) |
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

Setting `spec.tls` turns on TLS for the PostgreSQL server:

```yaml
spec:
  tls:
    secretName: production-db-tls   # tls.crt, tls.key, ca.crt
    # requireTLS: true              # default: reject non-TLS TCP connections
    # minProtocolVersion: TLSv1.2   # or TLSv1.3
```

**Clusters without `spec.tls` are not affected.** `requireTLS` defaults to
`true`, but the default only applies inside a `tls` block: a Cluster that does
not set `spec.tls` keeps running exactly as before (no pod changes, no restart,
`sslmode=disable`). Upgrading the operator therefore never turns TLS on, or
starts rejecting connections, for existing Clusters.

### The certificate Secret

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

A cert-manager `Certificate` produces exactly this layout. Example with a
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

1. **Validates the Secret** before touching the pod: all three keys present,
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

Rotation does not restart the pod. When the Secret changes (cert-manager
renews certificates automatically), the operator re-validates it and, if the
running server still presents the old certificate, runs `SELECT
pg_reload_conf()`; PostgreSQL re-reads the certificate files on reload.
Kubelet propagates Secret updates into the pod with a delay of up to about a
minute, so `TLSReady` is briefly `False` (reason `CertificateReloading`) and the
operator retries until the new certificate is served.

The reload connection is verified against the **current** `ca.crt`. If the CA
itself was replaced, the old certificate cannot be verified and the operator
does not fall back to an unverified connection: `TLSReady` stays `False` with
a message asking you to restart the pod (`kubectl delete pod <cluster>-0`).

### Turning TLS off

Removing `spec.tls` restarts the pod without `ssl=on` and without `hba_file`,
so PostgreSQL uses the `pg_hba.conf` in its data directory again (the image
default: password authentication over any address). The `<cluster>-hba`
ConfigMap is deleted, the `TLSReady` condition is removed, and the credentials
Secrets switch back to `sslmode=disable` and lose `ca.crt`.

### Limitations

- **Logical backups** (`pg_dump` Jobs) connect with libpq's default
  `sslmode=prefer`, so they keep working with `requireTLS` (encrypted, but the
  server certificate is not verified).
- **Physical (pgBackRest) backups** do not use TLS yet; this is planned as a
  follow-up.
- An operator running outside the cluster (`make run`) cannot reach the
  Service DNS name, so `TLSReady` stays `False` (`WaitingForServer`).
- Not configurable yet: `issuerRef` (operator-created cert-manager
  `Certificate`) and an operator-managed CA when no Secret is given. Both are
  planned.

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
