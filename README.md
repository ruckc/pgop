# pgop — PostgreSQL Operator

`pgop` is a lightweight Kubernetes Operator for managing PostgreSQL clusters, roles, and databases with a security-first, least-privilege design.

## Why pgop?

Existing PostgreSQL operators (CloudNativePG, Zalando, CrunchyData PGO) are powerful but bring significant operational weight: large footprints, complex CRD schemas, and opinionated HA/backup stacks. They also tend to surface a single superuser connection string to applications, leaving it to you to enforce access boundaries.

`pgop` takes a different approach:

- **Minimal footprint** — one controller binary, three CRDs (`Cluster`, `Role`, `Database`). No sidecars, no Patroni, no external dependency managers.
- **Apps never run as superuser** — the `Cluster` superuser credential is used only by the operator itself. Applications get a `Role`-scoped credential secret with only the privileges they need.
- **Credential secrets are automatic** — each `Role` reconciliation creates a `<database>-<role-name>-credentials` Secret containing `username`, `password`, `host`, and `port`. Applications consume secrets directly; there is no manual credential hand-off.
- **Declarative least-privilege** — `Database` CRs express schema ownership and grants inline, so the privilege model lives in version-controlled YAML rather than ad-hoc SQL scripts.
- **Restricted pod security out of the box** — pods run as UID 999 (postgres), non-root, drop ALL Linux capabilities, and use RuntimeDefault seccomp without any additional configuration.

## Custom Resources

### Cluster

Provisions a PostgreSQL StatefulSet, headless Service, and a `<name>-credentials` superuser Secret. The operator uses this credential internally; it is not distributed to application workloads.

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: example-cluster
  namespace: default
spec:
  image: postgres:18        # any postgres image tag
  # postgresMajorVersion: 18  # only needed if the tag can't be auto-detected
  replicas: 1
  port: 5432
  storage:
    size: 5Gi
  resources:
    requests:
      memory: "256Mi"
      cpu: "250m"
    limits:
      memory: "512Mi"
      cpu: "500m"
```

#### Data directory layout

The operator follows the official PostgreSQL image conventions, which changed in
PostgreSQL 18, and pins `PGDATA` explicitly so the data directory always lands
inside the persistent volume regardless of the image's own default:

| PostgreSQL major | PVC mount point         | `PGDATA`                              |
| ---------------- | ----------------------- | ------------------------------------- |
| ≤ 17             | `/var/lib/postgresql/data` | `/var/lib/postgresql/data`         |
| ≥ 18             | `/var/lib/postgresql`   | `/var/lib/postgresql/<major>/docker`  |

The major version is auto-detected from the image tag (`postgres:18`,
`postgis/postgis:16-3.4`, etc.). If the tag does not encode a parseable version
(e.g. `latest`, a digest-pinned reference, or a private mirror), set
`spec.postgresMajorVersion` explicitly — otherwise the cluster reconcile fails
with a clear condition rather than guessing a layout and risking data loss.

> **⚠️ Upgrading from pgop ≤ 0.4.1:** earlier releases hardcoded the PVC mount at
> `/var/lib/postgresql` and never set `PGDATA`, so the data directory depended on
> the image. Clusters running the default `postgres:18` image are unaffected (the
> path resolves to the same `/var/lib/postgresql/18/docker`). However, any cluster
> that relied on the old implicit behavior with a `≤ 17` image (whose data lived at
> `/var/lib/postgresql/data`) will resolve to a different mount after upgrading and
> must be treated as a **data migration** (dump/restore or `pg_upgrade`), not an
> in-place restart.

### Role

Creates a PostgreSQL user scoped to a `Cluster`. The operator auto-generates a password and writes a `<cluster>-<role>-credentials` Secret used internally by the `Database` controller.

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: app-user
  namespace: default
spec:
  clusterRef:
    name: example-cluster   # must exist in the same namespace
  login: true
  superuser: false
  createDB: false
  createRole: false
  inherit: true
  connectionLimit: 10
```

### Database

Creates a PostgreSQL database owned by a `Role`, declaratively manages schema grants, and writes a `<database>-<role>-credentials` Secret with everything an application needs to connect — no post-provisioning SQL scripts or credential hand-off required.

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: myapp
  namespace: default
spec:
  clusterRef:
    name: example-cluster
  owner: app-user             # must be the name of a Role CR
  extensions:
    - name: uuid-ossp
    - name: pg_trgm
  schemas:
    - name: app
      owner: app-user
      grants:
        - role: app-user
          privileges:
            - USAGE
            - CREATE
```

The resulting Secret `myapp-app-user-credentials` contains:

| Key        | Value                                              |
|------------|----------------------------------------------------|
| `username` | `app-user`                                         |
| `password` | auto-generated (sourced from role credentials)     |
| `host`     | `example-cluster.<namespace>.svc.cluster.local`    |
| `port`     | `5432`                                             |
| `database` | `myapp`                                            |

### Backup (alpha)

Schedules logical backups to S3-compatible storage.

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Backup
metadata:
  name: myapp-backup
  namespace: default
spec:
  type: logical
  databaseRef:
    name: myapp
  schedule: "0 2 * * *"      # cron — daily at 02:00
  retention:
    disabled: true
  backupRunTTL: "168h"        # keep job pods for 7 days
  destination:
    type: s3
    s3:
      bucket: pgop-backups
      prefix: myapp
      region: us-east-1
      endpoint: http://rustfs:9000
      credentialsSecretRef:
        name: rustfs-credentials
```

## Getting Started

### Prerequisites

- Kubernetes v1.24+
- `kubectl`

### Installation

```sh
helm install pgop oci://ghcr.io/ruckc/charts/pgop \
  --namespace pgop-system \
  --create-namespace
```

This installs the CRDs, creates the `pgop-system` namespace, configures RBAC, and deploys the controller from the Helm chart published to GHCR.

### Uninstallation

```sh
helm uninstall pgop --namespace pgop-system
```

Helm leaves CRDs in place so existing custom resources are not deleted automatically. To remove the CRDs too, delete them manually after uninstalling the release.

## Development

```sh
make build       # build binary (runs manifests, generate, fmt, vet)
make test        # unit tests via envtest (no Kind required)
make test-e2e    # e2e tests against a Kind cluster
make lint        # golangci-lint
make lint-fix    # golangci-lint with auto-fix
make manifests   # regenerate CRDs/RBAC from +kubebuilder markers
make generate    # regenerate DeepCopy methods
make run         # run controller locally against current kubeconfig context
```

Build and push a custom image:

```sh
make docker-build IMG=example.com/pgop:latest
make docker-push  IMG=example.com/pgop:latest
```

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.
