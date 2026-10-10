# Databases

A Database resource represents a PostgreSQL database within a cluster.

## Overview

The Database controller:

1. Connects to the referenced PostgreSQL cluster
2. Creates the database with the specified owner
3. Applies per-database settings (`ALTER DATABASE ... SET`) and resets removed ones
4. Applies database-level grants (`GRANT ... ON DATABASE`) and revokes removed ones
5. Installs requested extensions (trusted ones, or those the Cluster's
   [role policy](clusters.md#role-policy) allows)
6. Creates schemas with ownership and applies schema grants
7. Drops the database on deletion

See [Security model](#security-model) for what a Database writer can and
cannot do.

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
    - name: postgis            # untrusted: needs the Cluster's rolePolicy.allowedExtensions
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
| `name` | string | **required** | Extension name (`[A-Za-z0-9_-]`, at most 63 characters) |
| `schema` | string | control file schema, else `public` | Schema to install extension in |
| `version` | string | default version | Extension version to install |

### SchemaSpec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Schema name (not `pg_*` or `information_schema`) |
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
  must not be `postgres`, `template0` or `template1`. A Database whose
  `metadata.name` is one of those must set `databaseName`; the operator never
  manages or drops these databases (reason `ReservedName`).
- It is **immutable** after creation.
- `owner` is always a **Role resource name**; the operator resolves it to that
  Role's PostgreSQL name (`spec.roleName`, or its `metadata.name`). In contrast,
  `schemas[].owner` and `schemas[].grants[].role` are raw **PostgreSQL** role
  names.
- Backups and restores of this Database target the `databaseName`.

## Grants and DDL

- `schemas[].grants` grant **schema-level** privileges (`USAGE`, `CREATE`,
  `ALL`/`ALL PRIVILEGES`, in any letter case) via `GRANT ... ON SCHEMA`. Table
  privileges such as `SELECT` are not schema privileges and are rejected.
- `grants` grant **database-level** privileges (`CONNECT`, `CREATE`,
  `TEMPORARY`) via `GRANT ... ON DATABASE`; see
  [Database Grants](#database-grants).
- The Database is created with `OWNER <owner>` and each schema with
  `AUTHORIZATION <owner>`, so the **owner Role has full DDL** (create tables,
  run migrations) on the database and its owned schemas — make your app's login
  role the `owner` if it needs to create tables at runtime.
- Arbitrary SQL (event triggers, setup that depends on an extension) is not
  supported yet; an `initSQL` escape hatch is tracked in issue #24.

!!! note "Upgrade note: schema privileges are validated"
    Earlier versions passed `schemas[].grants[].privileges` into the `GRANT`
    statement unchecked (a SQL injection path). They are now limited to
    `USAGE`, `CREATE`, `ALL` and `ALL PRIVILEGES` (case-insensitive, at most 8
    entries of at most 32 characters), both by the API server and by the
    operator. Anything else, such as `SELECT` (which PostgreSQL rejects on a
    schema anyway), makes the Database invalid: fix the spec before upgrading.
    On Kubernetes older than 1.30 (no CRD validation ratcheting) an existing
    object with such a value cannot be updated at all, not even to remove its
    finalizer, until the value is corrected.

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
  alias of `TEMPORARY`) or `ALL` (all three), in upper case. Anything else is
  rejected by the API server, and again by the operator before any SQL is
  built.
- Each `role` may appear once. The role must exist; until it does, the Database
  reports `Available=False` with a message naming the missing role and retries
  (it also reconciles as soon as a Role resource with that PostgreSQL name
  changes). A missing grantee or a refused setting does not hold up the rest
  of the Database: extensions, schemas and the credentials Secret are still
  reconciled, and the Database stays not ready until the problem is fixed.
- Grants are re-applied on every reconcile, so privileges revoked by hand are
  restored.
- pgop records what it granted in `status.managedGrants`. When a role is removed
  from `grants`, or a privilege from its list, pgop **revokes exactly what it
  had granted**. Turning `withGrantOption` off revokes the grant option it
  granted. Privileges granted outside pgop are never revoked.
- When pgop granted a privilege `WITH GRANT OPTION`, its revoke uses
  `CASCADE`: privileges the grantee passed on to other roles are revoked too
  (otherwise PostgreSQL refuses with "dependent privileges exist").
- PostgreSQL grants `CONNECT` and `TEMPORARY` on every new database to `PUBLIC`
  by default, so revoking `CONNECT` from a role does not stop it connecting
  unless `PUBLIC`'s privilege is revoked as well (not managed by pgop).
- Deleting the grantee's Role works while a Database still grants to it: the
  Database stops granting to a Role that is being deleted (reported as
  "paused" in its condition), and the Role controller revokes the role's
  database and schema privileges before dropping it (see
  [Roles: deletion](roles.md#deletion)). Remove the entry from `grants`
  afterwards, or the Database reports the missing role.

## Database Settings

`spec.settings` sets per-database defaults for configuration parameters, like
`ALTER DATABASE <db> SET <name> TO <value>`. They apply to sessions that start
after the change.

```yaml
spec:
  settings:
    # The YAML single quotes only quote the string; the value is
    # "$user", app, public (with the double quotes).
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
- For the list parameters `search_path` and `temp_tablespaces`, write the value
  as in `postgresql.conf`: a comma-separated list where unquoted names are
  lowercased and double-quoted names keep their case. In YAML, quote the whole
  value when it starts with a double quote, as above. An empty list (`""`)
  is rejected, because `SET search_path TO ''` would store a schema literally
  named `""`; remove the key instead to fall back to the server default.
- Settings are re-applied on every reconcile. pgop records the names it set in
  `status.managedSettings` and runs `ALTER DATABASE ... RESET <name>` for any
  of them removed from `settings`. Settings made outside pgop are never reset.

### Which parameters may be set

The operator runs `ALTER DATABASE ... SET` as the cluster superuser, so it
restricts what a Database author can set:

- Only parameters with context `user` in `pg_settings` (those any role may set
  in its own session) and custom parameters the server does not know yet (such
  as `myapp.tenant`) are applied. Superuser-only parameters (for example
  `log_statement`, `session_preload_libraries`), and parameters that cannot be
  set per database anyway (`postmaster`, `sighup`, `internal`, `backend`), are
  refused.
- These parameters are always refused, whatever their context: `role`,
  `session_authorization` (they would switch the identity of every session,
  the operator's included), `session_preload_libraries`,
  `local_preload_libraries`, `shared_preload_libraries`,
  `dynamic_library_path`, `jit_provider` (code loading),
  `session_replication_role` (disables triggers and foreign keys), `lo_compat_privileges` (disables large-object permission checks), and the
  `pgaudit.*`, `set_user.*`, `anon.*` and `sepgsql.*` namespaces (security
  extensions, which may not be loaded yet when the setting is checked). The
  API server rejects these names directly.
- A refused setting is skipped, the others are applied, and the Database
  reports `Available=False` with reason `SettingNotAllowed` naming the
  parameter. A setting pgop applied earlier that is no longer allowed is
  reset.

!!! warning "Remaining risk"
    User-context parameters still affect every other session in the
    database (the operator's own sessions are pinned, see below). Custom
    placeholder parameters are accepted without a context check;
    if an extension that defines them is loaded later, their value applies
    with that extension's rules. Restrict who may create or edit Database
    resources accordingly.

The operator's own sessions pin their settings, so neither `spec.settings`
nor an `ALTER DATABASE ... SET` / `ALTER ROLE ... IN DATABASE ... SET` run by
the database owner can subvert them. Sent as connection parameters (which take
precedence over those per-database and per-role defaults):
`search_path = pg_catalog, pg_temp` (otherwise unqualified functions and
operators in the operator's superuser queries could resolve to objects in a
schema the owner controls), `role = none` (the owner can otherwise make every
new session in their database start as their own role), `statement_timeout`,
`lock_timeout`, `idle_in_transaction_session_timeout`, `idle_session_timeout`
(all `0`), `default_transaction_read_only = off`, `check_function_bodies = on`,
`row_security = on`, `default_tablespace` and `temp_tablespaces` (empty), and
`exit_on_error = off` (a superuser parameter an owner could only set with a
`parameterGrants` grant). On connecting, `transaction_timeout`
(PostgreSQL 17+, so it cannot be a startup parameter on older servers) is
reset to `0` as well. Logging parameters are not pinned (see
[Roles: parameter grants](roles.md#parameter-grants)).

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

### Extension policy

The operator installs extensions as a superuser. To keep that from being a
way around the server's own rules, an extension is only installed when

- the server marks the requested version (the default version when
  `version` is unset) as **trusted** (`pg_available_extension_versions.trusted`;
  for example `pg_trgm`, `pgcrypto`, `uuid-ossp`, `citext`, `hstore`,
  `btree_gist`, `tablefunc`), or
- the Cluster lists it in
  [`spec.rolePolicy.allowedExtensions`](clusters.md#role-policy).

Trusted extensions are those PostgreSQL lets any role with `CREATE` on the
database install, so pgop installing them gives the Database writer nothing
extra. Untrusted extensions, such as `file_fdw`, `dblink`, `adminpack`,
`plpython3u`, `postgis` or anything added to a custom image, can give access to
the server's files, network or code execution, or were simply not reviewed for
that; a cluster administrator has to allow them.

A refused extension is not installed; the other extensions and the schemas are
still reconciled and the Database reports `Available=False` with reason
`ExtensionNotAllowed`. An extension already installed in the database is never
dropped (also not when it is removed from the list or no longer allowed).

Without `schema`, the extension goes to the schema its control file names, or
else `public` (the operator no longer uses the database's `search_path` for
this).

!!! warning "Extensions must exist in the image"
    The operator runs `CREATE EXTENSION IF NOT EXISTS`, which only succeeds if
    the extension's files are already present in the running image. It does
    **not** install packages. The default `postgres:18` image does **not**
    include PostGIS or TimescaleDB — to use `postgis` set the Cluster's
    `spec.image` to a PostGIS-capable image (e.g. `postgis/postgis:18-3.5`), and
    similarly use a TimescaleDB image for `timescaledb`.

## Security model

A Database writer is trusted with the contents of **their** database, not with
the server:

- they choose the owner (a Role resource), schema owners and grants, and
  receive the owner's credentials in the connection Secret;
- settings are limited to user-context parameters minus a denylist (see
  [Which parameters may be set](#which-parameters-may-be-set));
- extensions are limited to trusted ones unless the Cluster allows more (see
  [Extension policy](#extension-policy));
- system schemas (`pg_catalog`, `pg_toast`, other `pg_*` names and
  `information_schema`) cannot be created, owned or granted on (reason
  `SchemaNotAllowed`): `CREATE` on `pg_catalog` would let the grantee shadow
  built-in functions for every session in the database, superusers included;
- the `postgres`, `template0` and `template1` databases cannot be managed
  (owning `template1` would put objects into every future database);
- the operator's sessions pin `search_path` (see above).

The policy for untrusted extensions lives on the Cluster, so allowing them
needs RBAC to edit the Cluster, separately from RBAC to create Databases. See
[Roles: security model](roles.md#security-model) for the overall picture.

Known gap: `schemas[].owner` and `grants[].role` accept any PostgreSQL role
name on the Cluster (giving ownership or privileges away is not an
escalation for the writer).

### Ownership of the PostgreSQL database

As for [roles](roles.md#ownership-of-the-postgresql-role), pgop only changes
the owner, settings, grants, extensions and schemas of, or drops, a database
this Database owns: one its `status.databaseName` records, one pgop creates in
this reconcile, or an existing one a **Cluster editor** listed in the
Cluster's [`spec.rolePolicy.adoptableDatabases`](clusters.md#role-policy)
(then taken over and recorded). Any other existing database (created by a
DBA, a restore tool, another Database, or a role with `CREATEDB`) is left
alone: reason `DatabaseNotManaged`, nothing is altered and deleting the
Database never drops it. A database's comment never authorizes a take-over
(its owner can set it).

- pgop stores a signed marker
  (`COMMENT ON DATABASE <db> IS 'pgop:v2:Database/<Database name>:<HMAC>'`,
  keyed by the Cluster's `<cluster>-marker-key` Secret) on the databases it
  owns; a recorded database whose comment was replaced by something unrelated
  is left alone, and a missing, unsigned (`pgop:v1:`) or stale (lost key)
  marker is refreshed;
- of two Databases of a Cluster with the same PostgreSQL name only the older
  one is reconciled; the other reports `DuplicateDatabaseName`;
- a Database re-created without its status reports `DatabaseNotManaged` until
  a Cluster editor lists the database in `adoptableDatabases`;
- a database whose owner turned connections off (`ALTER DATABASE ... WITH
  ALLOW_CONNECTIONS false`, which also locks out superusers) reports
  `DatabaseNotConnectable`: its settings and grants are still applied, its
  extensions and schemas wait until connections are allowed again.

### Upgrade / breaking changes

- Untrusted extensions are no longer installed unless the Cluster lists them
  in `spec.rolePolicy.allowedExtensions` (reason `ExtensionNotAllowed`).
- Extension names must match `[A-Za-z0-9_-]` (at most 63 characters); without
  `schema`, extensions are installed into their control file schema or
  `public`, no longer into the first schema of the database's `search_path`.
- System schema names (`pg_*`, `information_schema`) are rejected in
  `schemas`, and a Database named `postgres`, `template0` or `template1` must
  set `databaseName`.
- Existing databases are no longer taken over (`DatabaseNotManaged`) unless a
  Cluster editor lists them in `spec.rolePolicy.adoptableDatabases`;
  databases an earlier pgop created and recorded in status keep working and
  are marked automatically.
- `lo_compat_privileges` is refused in `settings`.

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

