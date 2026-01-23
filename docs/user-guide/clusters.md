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

Reference the secret in your application:

```yaml
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: app
          env:
            - name: DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: production-db-credentials
                  key: password
```

## Supported Images

Any Docker image compatible with the official PostgreSQL image environment variables:

- `postgres:18`
- `postgres:15`
- `postgres:14`
- `bitnami/postgresql:16`
- Custom images that support `POSTGRES_USER` and `POSTGRES_PASSWORD` env vars
