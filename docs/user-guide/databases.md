# Databases

A Database resource represents a PostgreSQL database within a cluster.

## Overview

The Database controller:

1. Connects to the referenced PostgreSQL cluster
2. Creates the database with the specified owner
3. Installs requested extensions
4. Creates schemas with ownership
5. Applies schema grants
6. Drops the database on deletion

## Example

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: myapp
  namespace: default
spec:
  clusterRef:
    name: my-cluster
  owner: app-user
  extensions:
    - name: uuid-ossp
    - name: pg_trgm
    - name: postgis
      schema: public
  schemas:
    - name: app
      owner: app-user
    - name: reports
      owner: app-user
      grants:
        - role: readonly_role
          privileges:
            - USAGE
            - SELECT
```

## Spec Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `clusterRef.name` | string | **required** | Name of the Cluster resource (same namespace) |
| `owner` | string | - | Role that owns the database |
| `extensions` | []ExtensionSpec | - | Extensions to install |
| `schemas` | []SchemaSpec | - | Schemas to create |

### ExtensionSpec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Extension name |
| `schema` | string | - | Schema to install extension in |

### SchemaSpec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Schema name |
| `owner` | string | - | Role that owns the schema |
| `grants` | []GrantSpec | - | Privileges to grant |

### GrantSpec

| Field | Type | Description |
|-------|------|-------------|
| `role` | string | Role to grant privileges to |
| `privileges` | []string | Privileges (USAGE, CREATE, SELECT, etc.) |

## Status

| Field | Description |
|-------|-------------|
| `ready` | Whether the database is ready |
| `installedExtensions` | List of installed extensions |
| `createdSchemas` | List of created schemas |
| `conditions` | Detailed status conditions |

## Connection Secret

For each Database, the operator emits a deterministic Secret named
**`<database-name>-<owner>-credentials`** (e.g. `myapp-app-user-credentials`)
containing everything an app needs to connect to that specific database:

```yaml
data:
  username: app-user       # the owner Role
  password: <owner's password>
  host: my-cluster.default.svc.cluster.local
  port: "5432"
  database: myapp          # this Database's name
```

The credentials mirror the owner Role's password (read from the Role's
`<cluster>-<owner>-credentials` Secret), with the `database` key set to this
Database. Because the name is deterministic, a Helm chart can mount it before
`status` is populated.

!!! note
    There is no `uri`/DSN key — build the connection string from the keys above,
    e.g. `postgres://$username:$password@$host:$port/$database`.

## Grants and DDL

- `schemas[].grants` grant **schema-level** privileges (`USAGE`, `CREATE`,
  `SELECT`, …) via `GRANT ... ON SCHEMA`.
- The Database is created with `OWNER <owner>` and each schema with
  `AUTHORIZATION <owner>`, so the **owner Role has full DDL** (create tables,
  run migrations) on the database and its owned schemas — make your app's login
  role the `owner` if it needs to create tables at runtime.
- Database-level grants (`GRANT CONNECT`/`CREATE ON DATABASE <db> TO <role>`)
  are **not** currently expressible in the spec. For a non-owner login role that
  needs to connect, grant it access at the schema level, or make it the owner.

## Ordering & Dependencies

The operator is largely order-independent (it requeues on transient errors),
but a Database depends on its owner Role:

- The Database controller waits for the referenced **Cluster** to be `ready`.
- It does **not** watch the owner Role's `status.ready`, but creating the
  database as `OWNER <owner>` and emitting the connection Secret both require the
  owner Role (and its `<cluster>-<owner>-credentials` Secret) to already exist.
  Missing prerequisites cause a requeue rather than a hard failure.

**Recommended apply order:** `Cluster` → `Role` → `Database`. Applying them all
at once also converges once the Cluster and Role become ready.

!!! note "Same-namespace only"
    `clusterRef` (and a Database's `owner` Role) must live in the **same
    namespace** as the Database. Cross-namespace references are not supported.

## Common Extensions

```yaml
extensions:
  # UUID generation
  - name: uuid-ossp

  # Full-text search
  - name: pg_trgm

  # JSON functions
  - name: pgcrypto

  # Geographic data
  - name: postgis

  # Time-series
  - name: timescaledb
```

!!! warning "Extensions must exist in the image"
    The operator runs `CREATE EXTENSION IF NOT EXISTS`, which only succeeds if
    the extension's files are already present in the running image. It does
    **not** install packages. The default `postgres:18` image does **not**
    include PostGIS or TimescaleDB — to use `postgis` set the Cluster's
    `spec.image` to a PostGIS-capable image (e.g. `postgis/postgis:18-3.5`), and
    similarly use a TimescaleDB image for `timescaledb`.

## Schema with Grants

Create a schema with read-only access for reporting:

```yaml
schemas:
  - name: app
    owner: app-user
  - name: app
    grants:
      - role: readonly_user
        privileges:
          - USAGE
          - SELECT
```

## Multi-Schema Application

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: ecommerce
spec:
  clusterRef:
    name: production
  owner: ecommerce-admin
  schemas:
    - name: products
      owner: product-service
    - name: orders
      owner: order-service
    - name: users
      owner: user-service
    - name: analytics
      owner: analytics-user
      grants:
        - role: product-service
          privileges: [USAGE, SELECT]
        - role: order-service
          privileges: [USAGE, SELECT]
```

