# Databases

A Database resource represents a PostgreSQL database within a cluster.

## Overview

The Database controller:

1. Connects to the referenced PostgreSQL cluster
2. Creates the database with the specified owner
3. Applies per-database settings (`ALTER DATABASE ... SET`) and resets removed ones
4. Applies database-level grants (`GRANT ... ON DATABASE`) and revokes removed ones
5. Installs requested extensions
6. Creates schemas with ownership and applies schema grants
7. Drops the database on deletion

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
  grants:
    - role: readonly_role      # PostgreSQL role name
      privileges: [CONNECT]
  settings:
    search_path: '"$user", app, public'
    statement_timeout: 30s
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
```

## Spec Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `clusterRef.name` | string | **required** | Name of the Cluster resource (same namespace) |
| `databaseName` | string | `metadata.name` | Database name in PostgreSQL (see [PostgreSQL Database Name](#postgresql-database-name)) |
| `owner` | string | - | Name of the **Role resource** that owns the database (operator superuser if unset) |
| `extensions` | []ExtensionSpec | - | Extensions to install |
| `schemas` | []SchemaSpec | - | Schemas to create |
| `grants` | []DatabaseGrantSpec | - | Database-level privileges (see [Database Grants](#database-grants)) |
| `settings` | map[string]string | - | Per-database parameter defaults (see [Database Settings](#database-settings)) |

### ExtensionSpec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Extension name |
| `schema` | string | - | Schema to install extension in |

### SchemaSpec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Schema name |
| `owner` | string | - | PostgreSQL role name that owns the schema |
| `grants` | []GrantSpec | - | Privileges to grant |

### GrantSpec

| Field | Type | Description |
|-------|------|-------------|
| `role` | string | PostgreSQL role name to grant privileges to |
| `privileges` | []string | Schema privileges: `USAGE`, `CREATE` or `ALL` |
| `withGrantOption` | bool | Allow the grantee to grant the privileges to others |

### DatabaseGrantSpec

| Field | Type | Description |
|-------|------|-------------|
| `role` | string | PostgreSQL role name to grant privileges to (unique within `grants`) |
| `privileges` | []string | Database privileges: `CONNECT`, `CREATE`, `TEMPORARY` (or `TEMP`), or `ALL` |
| `withGrantOption` | bool | Allow the grantee to grant the privileges to others |

## Status

| Field | Description |
|-------|-------------|
| `ready` | Whether the database is ready |
| `databaseName` | The effective PostgreSQL database name that was reconciled |
| `installedExtensions` | List of installed extensions |
| `createdSchemas` | List of created schemas |
| `managedGrants` | Database privileges pgop granted (revoked when removed from `grants`) |
| `managedSettings` | Parameter names pgop set (reset when removed from `settings`) |
| `conditions` | Detailed status conditions |

## Connection Secret

For each Database, the operator emits a deterministic Secret named
**`<database-name>-<owner>-credentials`** (e.g. `myapp-app-user-credentials`)
containing everything an app needs to connect to that specific database:

```yaml
data:
  username: app-user       # the owner Role's PostgreSQL name
  password: <owner's password>
  host: my-cluster.default.svc.cluster.local
  port: "5432"
  database: myapp          # this Database's PostgreSQL name
  sslmode: disable         # verify-full once Cluster TLS is active
  uri: postgresql://app-user:<password>@my-cluster.default.svc.cluster.local:5432/myapp?sslmode=disable
  ca.crt: <PEM>            # only while Cluster TLS is active
```

`sslmode`, `uri` and `ca.crt` follow the Cluster's [TLS](clusters.md#tls)
state. With `sslmode=verify-full`, mount `ca.crt` and point `sslrootcert` (or
`PGSSLROOTCERT`) at it; a URI cannot carry the CA itself.

The credentials mirror the owner Role's password (read from the Role's
`<cluster>-<owner>-credentials` Secret), with the `database` key set to this
Database. When the owner's password changes (a new `passwordSecretRef` value or
a [rotation](roles.md#password-rotation)), the operator updates this Secret
too. Because the name is deterministic, a Helm chart can mount it before
`status` is populated.

The Secret is only created when `owner` is set and the owner Role has `login`
enabled; a Database without an owner (or owned by a NOLOGIN group role) gets no
connection Secret.

!!! note
    There is no `uri`/DSN key — build the connection string from the keys above,
    e.g. `postgres://$username:$password@$host:$port/$database`.

## PostgreSQL Database Name

By default the PostgreSQL database is named after the Database resource
(`metadata.name`). Set `spec.databaseName` when it should differ, for example to
use underscores:

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: rs-app-db
spec:
  clusterRef:
    name: my-cluster
  databaseName: rs_app_db   # PostgreSQL database name
  owner: rs-app             # Role resource name (its roleName may be rs_app)
```

- `databaseName` must match `^[a-z_][a-z0-9_]*$`, be at most 63 characters, and
  must not be `postgres`, `template0` or `template1`.
- It is **immutable** after creation.
- `owner` is always a **Role resource name**; the operator resolves it to that
  Role's PostgreSQL name (`spec.roleName`, or its `metadata.name`). In contrast,
  `schemas[].owner` and `schemas[].grants[].role` are raw **PostgreSQL** role
  names.
- Backups and restores of this Database target the `databaseName`.

## Grants and DDL

- `schemas[].grants` grant **schema-level** privileges (`USAGE`, `CREATE`,
  `ALL`) via `GRANT ... ON SCHEMA`. Table privileges such as `SELECT` are not
  schema privileges and are rejected.
- `grants` grant **database-level** privileges (`CONNECT`, `CREATE`,
  `TEMPORARY`) via `GRANT ... ON DATABASE`; see
  [Database Grants](#database-grants).
- The Database is created with `OWNER <owner>` and each schema with
  `AUTHORIZATION <owner>`, so the **owner Role has full DDL** (create tables,
  run migrations) on the database and its owned schemas — make your app's login
  role the `owner` if it needs to create tables at runtime.
- Arbitrary SQL (event triggers, setup that depends on an extension) is not
  supported yet; an `initSQL` escape hatch is tracked in issue #24.

## Database Grants

`spec.grants` grants database-level privileges to PostgreSQL roles:

```yaml
spec:
  grants:
    - role: app_reader          # raw PostgreSQL role name
      privileges: [CONNECT]
    - role: app_migrator
      privileges: [CONNECT, CREATE, TEMP]
      withGrantOption: false
```

- `privileges` may only contain `CONNECT`, `CREATE`, `TEMPORARY`, `TEMP` (an
  alias of `TEMPORARY`) or `ALL` (all three). Anything else is rejected by the
  API server, and again by the operator before any SQL is built.
- Each `role` may appear once. The role must exist; until it does, the Database
  reports `Available=False` with a message naming the missing role and retries
  (it also reconciles as soon as a Role resource with that PostgreSQL name
  changes).
- Grants are re-applied on every reconcile, so privileges revoked by hand are
  restored.
- pgop records what it granted in `status.managedGrants`. When a role is removed
  from `grants`, or a privilege from its list, pgop **revokes exactly what it
  had granted**. Turning `withGrantOption` off revokes the grant option it
  granted. Privileges granted outside pgop are never revoked. Revoking a
  privilege the grantee has passed on to others fails (PostgreSQL requires
  `CASCADE`, which pgop never uses); the error is reported in the `Available`
  condition.
- PostgreSQL grants `CONNECT` and `TEMPORARY` on every new database to `PUBLIC`
  by default, so revoking `CONNECT` from a role does not stop it connecting
  unless `PUBLIC`'s privilege is revoked as well (not managed by pgop).
- A role that holds privileges on a database cannot be dropped. Delete the
  Database, or remove the grant, before deleting the grantee's Role.

## Database Settings

`spec.settings` sets per-database defaults for configuration parameters, like
`ALTER DATABASE <db> SET <name> TO <value>`. They apply to sessions that start
after the change.

```yaml
spec:
  settings:
    search_path: '"$user", app, public'
    statement_timeout: 30s
    work_mem: 64MB
    myapp.tenant: acme            # custom parameters are allowed
```

- Keys must be parameter names: identifiers (`[A-Za-z_][A-Za-z0-9_]*`),
  optionally separated by dots, at most 127 characters. Names are
  case-insensitive; pgop uses them lowercased.
- Values are always sent as SQL string literals (quotes and backslashes are
  escaped), at most 4096 characters. PostgreSQL validates them: an unknown
  parameter or an invalid value is reported in the `Available` condition.
- For list parameters (`search_path`, `temp_tablespaces`,
  `session_preload_libraries`, `local_preload_libraries`) write the value as in
  `postgresql.conf`: a comma-separated list where unquoted names are
  lowercased and double-quoted names keep their case, e.g.
  `'"$user", app, public'`.
- Settings are re-applied on every reconcile. pgop records the names it set in
  `status.managedSettings` and runs `ALTER DATABASE ... RESET <name>` for any
  of them removed from `settings`. Settings made outside pgop are never reset.

!!! warning "Settings run as the superuser"
    The operator applies settings as the cluster superuser, so a Database author
    can set superuser-only parameters for every session in that database. For
    example `session_preload_libraries` loads any shared library present in the
    image into every session, and `default_transaction_read_only` also affects
    the operator's own connections to the database. Restrict who may create or
    edit Database resources accordingly.

## Ordering & Dependencies

The operator is largely order-independent (it requeues on transient errors),
but a Database depends on its owner Role:

- The Database controller waits for the referenced **Cluster** to be `ready`.
- When `owner` is set, it waits for that **Role** to exist and be `ready`
  (reported in the `Available` condition), and reconciles again as soon as the
  Role changes. It then creates the database as `OWNER` the Role's PostgreSQL
  name. Missing prerequisites cause a requeue rather than a hard failure.

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

Create a schema that a reporting role may use:

```yaml
schemas:
  - name: app
    owner: app-user
    grants:
      - role: readonly_user
        privileges:
          - USAGE
```

`USAGE` lets the role look up objects in the schema. Table privileges such as
`SELECT` are not schema privileges and are not managed by pgop; grant them in
your migrations (for example with `ALTER DEFAULT PRIVILEGES`).

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
          privileges: [USAGE]
        - role: order-service
          privileges: [USAGE]
```

