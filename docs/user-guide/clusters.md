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
| `storage.size` | string | - | PVC size (e.g., "10Gi") |
| `storage.storageClassName` | string | - | Storage class name |
| `resources` | ResourceRequirements | - | CPU/memory requests/limits |

## Status

| Field | Description |
|-------|-------------|
| `ready` | Whether the cluster is ready to accept connections |
| `endpoint` | Service endpoint (hostname:port) |
| `secretName` | Name of the credentials secret |
| `conditions` | Detailed status conditions |

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
