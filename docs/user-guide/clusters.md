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

## Status

| Field | Description |
|-------|-------------|
| `ready` | Whether the cluster is ready to accept connections |
| `endpoint` | Service endpoint (hostname:port) |
| `secretName` | Name of the credentials secret |
| `conditions` | Detailed status conditions (`Available`, `ExistingVolume`) |

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
- **TLS:** the operator itself connects with `sslmode=disable` over the
  in-cluster network, and TLS/`sslmode` is not currently configurable via the
  CRDs. Apps connecting in-cluster should use `sslmode=disable` (or
  `prefer`) unless you terminate TLS in front of PostgreSQL yourself.
