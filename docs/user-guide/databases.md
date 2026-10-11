# Databases

A Database resource represents a PostgreSQL database within a cluster.

## Overview

The Database controller:

1. Connects to the referenced PostgreSQL cluster
2. Creates the database with the specified owner
3. Applies per-database settings (`ALTER DATABASE ... SET`) and resets removed ones
4. Applies database-level grants (`GRANT ... ON DATABASE`) and revokes removed ones
5. Revokes PostgreSQL's default `PUBLIC` privileges when asked
   ([`publicPrivileges`](#default-public-privileges))
6. Creates schemas with ownership
7. Installs and updates requested [extensions](#extensions) (trusted ones, or
   those the Cluster's [role policy](clusters.md#role-policy) allows, and only
   into schemas no untrusted role can write to); drops removed ones only when
   asked (`dropOnRemoval`)
8. Applies schema grants and grants on extension objects, and revokes removed
   ones
9. Applies [object grants](#object-grants) (tables, sequences, functions,
   procedures, types) and [default privileges](#default-privileges), and
   revokes removed ones
10. Drops the database on deletion

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
      schema: postgis          # and a schema only superusers can write to (created by pgop)
  schemas:
    - name: app
      owner: app-user
    - name: reports
      owner: app-user
      grants:
        - role: readonly_role
          privileges:
            - USAGE
      objectGrants:             # existing objects
        - role: readonly_role
          kind: table
          objects: ["*"]        # every table (and view) of the schema
          privileges: [SELECT]
      defaultPrivileges:        # objects app-user creates later
        - forRole: app-user     # PostgreSQL role name of a Role of this Cluster
          role: readonly_role
          kind: table
          privileges: [SELECT]
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
| `publicPrivileges` | PublicPrivilegesSpec | - | Default `PUBLIC` privileges to revoke (see [Default PUBLIC privileges](#default-public-privileges)) |

### ExtensionSpec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Extension name (`[A-Za-z0-9_-]`, at most 63 characters; unique, at most 64 extensions) |
| `schema` | string | control file schema, else `public` | Schema to install the extension in (not `pg_*` or `information_schema`); created by pgop, owned by the operator, when missing and not in `schemas`. See [the schema rules](#the-schema-an-extensions-scripts-run-in) |
| `version` | string | default version | Version to install; changing it updates the extension (no downgrades). See [Versions](#versions) |
| `cascade` | bool | `false` | Install missing dependencies too (each must pass the policy). See [Dependencies](#dependencies-and-cascade) |
| `dropOnRemoval` | bool | `false` | Drop the extension (without `CASCADE`) once removed from the list, if pgop created it. See [Removing an extension](#removing-an-extension) |
| `grants` | []ExtensionGrantSpec | - | Privileges on the extension's objects (at most 16). See [Grants on extension objects](#grants-on-extension-objects) |

### ExtensionGrantSpec

| Field | Type | Description |
|-------|------|-------------|
| `role` | string | Grantee: a PostgreSQL role name or `PUBLIC` (unique within the extension's `grants`; see [Grantee policy](#grantee-policy)) |
| `schema` | []string | `USAGE`, `CREATE`, `ALL` on the extension's own schema |
| `tables` | []string | `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `MAINTAIN`, `ALL` on its plain and partitioned tables |
| `sequences` | []string | `USAGE`, `SELECT`, `UPDATE`, `ALL` on its sequences |
| `functions` | []string | `EXECUTE` (or `ALL`) on its SQL and PL/pgSQL functions and procedures that are not `SECURITY DEFINER` |

At least one of `schema`, `tables`, `sequences` and `functions` is required.

### SchemaSpec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Schema name (not `pg_*` or `information_schema`; unique, at most 64 schemas) |
| `owner` | string | database owner | PostgreSQL role name that owns the schema (see [Which schemas a Database manages](#which-schemas-a-database-manages)) |
| `grants` | []GrantSpec | - | Schema privileges to grant (at most 16; see [Schema Grants](#schema-grants)) |
| `objectGrants` | []ObjectGrantSpec | - | Privileges on the schema's tables, sequences, functions, procedures and types (at most 32; see [Object Grants](#object-grants)) |
| `defaultPrivileges` | []DefaultPrivilegeSpec | - | Privileges on objects a role creates in the schema later (at most 32; see [Default Privileges](#default-privileges)) |

### ObjectGrantSpec

| Field | Type | Description |
|-------|------|-------------|
| `role` | string | Grantee: a PostgreSQL role name or `PUBLIC` (see [Grantee policy](#grantee-policy)) |
| `kind` | string | `table` (also views, materialized views, foreign tables), `sequence`, `function` (also aggregates), `procedure` or `type` (also domains, enums, ranges) |
| `objects` | []string | Object names in the schema (at most 64, each at most 255 characters), or `["*"]` for every object of the kind. Functions and procedures: `name(argtypes)` for one, `name` for every overload |
| `privileges` | []string | `table`: `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER`, `MAINTAIN` (PostgreSQL 17+), `ALL` (all but `MAINTAIN`); `sequence`: `USAGE`, `SELECT`, `UPDATE`, `ALL`; `function`/`procedure`: `EXECUTE`, `ALL`; `type`: `USAGE`, `ALL` |
| `withGrantOption` | bool | Allow the grantee to grant the privileges to others (not for `PUBLIC`) |

### DefaultPrivilegeSpec

| Field | Type | Description |
|-------|------|-------------|
| `forRole` | string | PostgreSQL role whose future objects get the privileges; must be managed by a Role of the same Cluster and not a superuser |
| `role` | string | Grantee: a PostgreSQL role name or `PUBLIC` (see [Grantee policy](#grantee-policy)) |
| `kind` | string | `table`, `sequence`, `function` (also procedures) or `type` |
| `privileges` | []string | As for `objectGrants` of the same kind |
| `withGrantOption` | bool | Allow the grantee to grant the privileges to others (not for `PUBLIC`) |

`forRole`, `role` and `kind` identify an entry (each combination at most once).

### GrantSpec

| Field | Type | Description |
|-------|------|-------------|
| `role` | string | Grantee: a PostgreSQL role name or `PUBLIC` (unique within the schema's `grants`; see [Grantee policy](#grantee-policy)) |
| `privileges` | []string | Schema privileges: `USAGE`, `CREATE` or `ALL` |
| `withGrantOption` | bool | Allow the grantee to grant the privileges to others (not for `PUBLIC`) |

### DatabaseGrantSpec

| Field | Type | Description |
|-------|------|-------------|
| `role` | string | Grantee: a PostgreSQL role name or `PUBLIC` (unique within `grants`; see [Grantee policy](#grantee-policy)) |
| `privileges` | []string | Database privileges: `CONNECT`, `CREATE`, `TEMPORARY` (or `TEMP`), or `ALL` |
| `withGrantOption` | bool | Allow the grantee to grant the privileges to others (not for `PUBLIC`) |

### PublicPrivilegesSpec

| Field | Type | Description |
|-------|------|-------------|
| `connect` | bool | `false` revokes `CONNECT` on the database from `PUBLIC` |
| `temporary` | bool | `false` revokes `TEMPORARY` on the database from `PUBLIC` |
| `publicSchemaUsage` | bool | `false` revokes `USAGE` on the schema `public` from `PUBLIC` |
| `publicSchemaCreate` | bool | `false` revokes `CREATE` on the schema `public` from `PUBLIC` (a default only before PostgreSQL 15) |

Unset or `true` leaves PostgreSQL's default alone (and restores what pgop revoked).

## Status

| Field | Description |
|-------|-------------|
| `ready` | Whether the database is ready |
| `databaseName` | The effective PostgreSQL database name that was reconciled |
| `installedExtensions` | The extensions of `extensions` that are installed |
| `extensions` | Per extension: installed `version` and `schema`, whether pgop `created` it (with the installation's `oid` and `owner`), `dropOnRemoval` as last reconciled, `reason`/`message` when it is not as requested, `skippedObjects` (see [Extensions](#extensions)) |
| `managedExtensionGrants` | Privileges pgop added on extension objects, per extension, grantee and kind (revoked when removed from `extensions[].grants`) |
| `createdSchemas` | Schemas the Database manages (created by it, or existing and owned by the declared or database owner) |
| `managedGrants` | Database privileges and grant options pgop added (revoked when removed from `grants`) |
| `managedSchemaGrants` | Schema privileges and grant options pgop added (revoked when removed from `schemas[].grants`) |
| `managedObjectGrants` | Privileges and grant options pgop added on schema objects, per object and grantee (revoked once no `objectGrants` entry selects the object) |
| `managedDefaultPrivileges` | Default privileges pgop added, per schema, `forRole`, kind and grantee (removed when removed from `defaultPrivileges`) |
| `objectGrants` | Per schema and kind: how many selected objects pgop grants on (`granted`), how many it skips (`skipped`) and up to five `skippedExamples` |
| `revokedPublicPrivileges` | Default `PUBLIC` privileges pgop revoked (granted back when no longer requested) |
| `managedSettings` | Parameter names pgop set (reset when removed from `settings`) |
| `conditions` | `Available`, and `ObjectGrantsComplete` (see [Object Grants](#object-grants)) |

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
  `ALL`/`ALL PRIVILEGES`, in any letter case) via `GRANT ... ON SCHEMA`; see
  [Schema Grants](#schema-grants). Table privileges such as `SELECT` are not
  schema privileges and are rejected.
- `grants` grant **database-level** privileges (`CONNECT`, `CREATE`,
  `TEMPORARY`) via `GRANT ... ON DATABASE`; see
  [Database Grants](#database-grants).
- The Database is created with `OWNER <owner>` and each schema with
  `AUTHORIZATION <owner>`, so the **owner Role has full DDL** (create tables,
  run migrations) on the database and its owned schemas — make your app's login
  role the `owner` if it needs to create tables at runtime.
- `extensions[].grants` grant privileges on an extension's own objects
  (owned by the superuser that installed it), such as pg_partman's tables and
  functions; see [Grants on extension objects](#grants-on-extension-objects).
- `schemas[].objectGrants` grant privileges on tables, sequences, functions,
  procedures and types that exist in a schema, and
  `schemas[].defaultPrivileges` on the ones a role creates later; see
  [Object Grants](#object-grants) and
  [Default Privileges](#default-privileges).
- There is no arbitrary SQL (`initSQL`): pgop declares grants instead (issue
  #24). Objects your own roles own can also be granted on from your
  migrations.

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
- Each `role` may appear once. It is a role the [grantee policy](#grantee-policy)
  allows, or `PUBLIC`. The role must exist; until it does, the Database
  reports `Available=False` with a message naming the missing role and retries
  (it also reconciles as soon as a Role resource with that PostgreSQL name
  changes). A missing or refused grantee, or a refused setting, does not hold
  up the rest of the Database: the other grants, extensions, schemas and the
  credentials Secret are still reconciled, and the Database stays not ready
  until the problem is fixed.
- On every reconcile pgop compares each grant with what the grantee holds
  directly on the database and grants only what is missing, so privileges
  revoked by hand are restored.
- pgop records in `status.managedGrants` **only what it added**: the
  privileges (and grant options) the grantee did not hold before. When a role
  is removed from `grants`, or a privilege from its list, pgop revokes exactly
  that. See [What pgop tracks](#what-pgop-tracks) for what this means for
  privileges the grantee already held.
- PostgreSQL grants `CONNECT` and `TEMPORARY` on every new database to `PUBLIC`
  by default, so revoking `CONNECT` from a role does not stop it connecting
  unless `PUBLIC`'s privilege is revoked as well: see
  [Default PUBLIC privileges](#default-public-privileges).
- Deleting the grantee's Role works while a Database still grants to it: the
  Database stops granting to a Role that is being deleted (reported as
  "paused" in its condition), and the Role controller revokes the role's
  database and schema privileges before dropping it (see
  [Roles: deletion](roles.md#deletion)). Remove the entry from `grants`
  afterwards, or the Database reports the missing role.

## Schema Grants

`schemas[].grants` grants schema privileges (`GRANT ... ON SCHEMA`) once the
schema exists:

```yaml
spec:
  schemas:
    - name: app
      owner: app-owner
      grants:
        - role: app_reader        # raw PostgreSQL role name
          privileges: [USAGE]
        - role: app_migrator
          privileges: [USAGE, CREATE]
          withGrantOption: true
```

They follow the same rules as [database grants](#database-grants):

- missing privileges are granted on every reconcile, so privileges revoked by
  hand are restored;
- pgop records what it added in `status.managedSchemaGrants` and, when a
  grant, a privilege or a whole schema entry is removed, **revokes exactly what
  it added** (the schema itself is never dropped). See
  [What pgop tracks](#what-pgop-tracks);
- a grant on a schema that was dropped, or to a role that was dropped, is
  forgotten without a statement (nothing is left to revoke);
- grants are only applied on schemas the Database manages (see
  [Which schemas a Database manages](#which-schemas-a-database-manages));
- grantees follow the [grantee policy](#grantee-policy), and grants to a Role
  being deleted are paused.

### What pgop tracks

The same rules hold for `grants`, `schemas[].grants`, `schemas[].objectGrants`,
`schemas[].defaultPrivileges`, `extensions[].grants` and Role
`parameterGrants`:

- pgop compares each declared grant with the privileges the grantee holds
  **directly** on the object from the object's owner (the grantor of every
  `GRANT` a superuser issues), PostgreSQL's built-in defaults included. It
  grants and records only what is missing. A declared privilege the grantee
  already held is **not** recorded and is **never revoked** when it leaves the
  spec: PostgreSQL's defaults (`PUBLIC`'s `CONNECT`/`TEMPORARY` on a database,
  `PUBLIC`'s `USAGE` on the schema `public`), the owner's own privileges, and
  grants made by hand all survive. Consequence: to have pgop take over a
  privilege that was granted by hand (so that removing it from the spec
  revokes it), revoke it by hand first; pgop then grants it again and records
  it. To take away one of PostgreSQL's `PUBLIC` defaults, use
  [`publicPrivileges`](#default-public-privileges).
- Grant options are tracked separately: declaring `withGrantOption` for a
  privilege the grantee held without it records only the grant option, and
  turning `withGrantOption` off (or removing the grant) revokes only that.
- Revoking a grant option pgop added uses `CASCADE`: privileges the grantee
  passed on go too. A plain revoke that PostgreSQL refuses because the
  grantee passed the privilege on with a grant option pgop did **not** give
  (granted by hand) is not forced: pgop stops tracking that privilege and
  reports it once with reason `RevokeSkipped`; revoke it with `CASCADE` by
  hand if wanted. A failing statement does not hold up the other grants and
  revokes. When one privilege of a `REVOKE` has such dependents, the others
  are revoked one at a time, so only that privilege is skipped.
- The ledger is an intent log: pgop writes what it is about to add to the
  status **before** it runs the `GRANT`. So a status update that is lost
  after the `GRANT` (a conflict, an operator restart) cannot leave a
  privilege pgop granted untracked. If recording fails (for example because
  the cached copy of the resource was stale), nothing is granted and the
  next reconcile, from the current copy, tries again; a stale copy never
  overwrites a newer status.
  An entry recorded for a `GRANT` that then failed is harmless: the privilege
  is granted again while declared, and revoking a privilege the grantee does
  not hold changes nothing.
- pgop cannot tell a privilege it recorded from an identical one granted by
  hand later: `GRANT` of a privilege the grantee holds changes nothing, so if
  someone grants by hand what pgop already granted, removing the entry from
  the spec revokes it.
- The ledgers are bounded (`managedGrants` 512 entries, `managedSchemaGrants`
  2048, `managedExtensionGrants` 1024, `managedObjectGrants` 4096 (one entry
  per object and grantee), `managedDefaultPrivileges` 2048,
  `managedParameterGrants` 512). When the declared grants plus the grants pgop
  still tracks would exceed that, nothing of that kind is granted (reason
  `TooManyGrants`) until grants are removed from the spec; revokes of grants
  removed from the spec, or whose grantee the policy no longer allows, still
  run, so the ledger only shrinks and nothing tracked is lost.
- The first reconcile of a Database without a ledger records what it adds
  without revoking anything. If the status is lost (the Database is
  re-created, or restored without status), grants removed in the meantime are
  not revoked.
- Ledger entries written by pgop v0.15 recorded every declared database and
  parameter privilege, whether or not the grantee already held it, with a
  `withGrantOption` flag. They are still honoured: such an entry is revoked
  when it leaves the spec, **including a PostgreSQL default it recorded** (for
  example a declared `PUBLIC` `CONNECT`), and `withGrantOption: true` is read
  as the grant option for all its privileges. New entries are recorded with
  `grantOptions` instead.

### Which schemas a Database manages

A Database changes the owner of, and grants on, only schemas it manages:

- schemas it created (recorded in `status.createdSchemas`); a schema without
  a declared `owner` is created owned by the database's owner; and
- existing schemas owned by a role that is not a superuser and is the
  schema's declared `owner`, the database's owner when the Database declares
  it in `owner` (a database adopted without `owner` may keep an owner no Role
  manages, whose schemas are not taken over), or `pg_database_owner` (the
  owner of `public` since PostgreSQL 15); and
- the schema `public` of a database created by PostgreSQL 14 or older, owned
  by the bootstrap superuser: it is managed for grants only, and its owner is
  never changed (a declared `owner` is ignored for it).

Any other existing schema, for example one an extension script created
(owned by the operator), or one another role owns, is left alone: its owner
is not changed, no grants are applied on it (grants pgop added earlier are
revoked), and the Database reports `SchemaNotManaged`. A schema that leaves
`schemas` also leaves `status.createdSchemas`. On a Database without an
`owner`, the database owner is the operator, so schemas without an `owner`
are created owned by the operator; such a schema that is removed from
`schemas` and listed again later is no longer recorded and reports
`SchemaNotManaged`. Give the schema an `owner` (or the Database an `owner`) to
avoid that.

## Object Grants

`schemas[].objectGrants` grants privileges on objects that exist in a schema
the Database manages: tables (also views, materialized views and foreign
tables), sequences, functions (also aggregates), procedures and types (also
domains, enums and ranges).

```yaml
spec:
  schemas:
    - name: app
      owner: app_owner
      grants:
        - role: app_ro
          privileges: [USAGE]          # needed to reach the objects at all
      objectGrants:
        - role: app_ro
          kind: table
          objects: ["*"]               # every table of app, re-evaluated on every reconcile
          privileges: [SELECT]
        - role: app_rw
          kind: sequence
          objects: [orders_id_seq]
          privileges: [USAGE, SELECT]
        - role: rs_control
          kind: function
          objects: ["caller_access(text)", "refresh"]   # one overload; every overload of refresh
          privileges: [EXECUTE]
        - role: app_ro
          kind: type
          objects: [mood]
          privileges: [USAGE]
```

### Selecting objects

- `objects` lists names as PostgreSQL stores them (case-sensitive, without
  quotes and without the schema): `Orders` and `orders` are different
  tables. Names are only ever passed to catalog queries as parameters, never
  spliced into SQL, so a name with quotes or semicolons is just a name.
- A function or procedure is named `name(argtypes)`, for example
  `caller_access(text)` or `f(integer, app.mood)`; the server resolves the
  signature (`to_regprocedure`), so `int4` and `integer` are the same.
  Argument types outside `pg_catalog` must be schema-qualified. `name` alone
  (without parentheses) selects every overload.
- `["*"]` selects every object of the kind in the schema at the time of the
  reconcile, listed again on every reconcile. At most 5000 objects per kind
  and schema: beyond that pgop grants nothing for that kind and schema and
  reports `TooManyObjects`; what it granted there stays, except grants to a
  grantee the policy no longer allows, which are revoked.
- **`"*"` is evaluated when the Database reconciles, not when objects are
  created.** Reconciles are event-driven: a change of the Database, a change
  of a Role the Database names, and the controller's periodic resync (about
  every 10 hours by default). A table created by a migration is therefore
  granted on by `"*"` only at the next reconcile. Pair `"*"` with
  [default privileges](#default-privileges), which apply the moment the
  object is created, or touch the Database after a migration, for example
  `kubectl annotate database <name> pgop.ruck.io/reconcile="$(date +%s)" --overwrite`.
- A named object that does not exist (yet) is reported with reason
  `ObjectNotFound` and retried; the other grants are still applied.
- pgop reads each kind of each schema with one catalog query per reconcile.

### Which objects pgop grants on

pgop runs `GRANT` as a superuser, which would let it grant on any object
whatever its owner. It does not: a Database writer may only reach objects of
the Cluster's trust domain. An object is granted on only when:

- it is in a schema the Database manages (see
  [Which schemas a Database manages](#which-schemas-a-database-manages); never
  `pg_catalog`, `information_schema` or other system schemas);
- it does not belong to an extension (`pg_depend` type `e`): grant on
  extension objects with [`extensions[].grants`](#grants-on-extension-objects),
  which has its own safety rules; and
- its owner is a role managed by a Role of the same Cluster (not a
  superuser), or the operator itself. The database owner and the schema's
  declared `owner` count only when they are such a role: a database adopted
  without `owner` may keep a legacy owner no Role manages, and a Database
  writer can declare any existing role the `owner` of an existing schema that
  role owns (see
  [Which schemas a Database manages](#which-schemas-a-database-manages));
  neither must open up that role's objects.

Objects the operator owns (created by a bootstrap Job with the Cluster
credentials, for example) act with superuser privileges where they act as
their owner, so the [extension rules](#grants-on-extension-objects) apply to
them: `EXECUTE` only on SQL and PL/pgSQL functions that are not
`SECURITY DEFINER`, table privileges only on plain and partitioned tables (a
view reads its tables with its owner's privileges), and never `TRIGGER` or
`MAINTAIN` (a trigger, or an index expression evaluated by maintenance, on a
table that superuser jobs write to would run code as that superuser).

Everything else is skipped: objects owned by another superuser or by a role
outside the Cluster's Roles (including roles listed in
`allowedExistingRoles`), extension members, the operator's objects above, and
objects whose rendered name is longer than 1024 characters (pgop could not
record it). A `SECURITY DEFINER` function owned by a managed (non-superuser)
role is fine: it runs as that role.

!!! warning "`TRIGGER` runs the grantee's code as whoever writes"
    `TRIGGER` lets the grantee create triggers on the table, and a trigger
    function runs as the role that inserts, updates or deletes, not as the
    trigger's creator. pgop only grants `TRIGGER` on tables a managed role
    owns (never on the operator's), so only roles of the Cluster's trust
    domain can be affected through pgop. Still, grant it only when every role
    that writes to the table (the owner, the application, a DBA's jobs)
    trusts the grantee: a superuser writing to the table would run the
    grantee's code as a superuser.

- Objects selected with `"*"` that are skipped are counted in
  `status.objectGrants[]` (`skipped`, with up to five `skippedExamples`
  naming the object and the reason) and make the `ObjectGrantsComplete`
  condition `False` with reason `ObjectGrantSkipped`; they do not make the
  Database unavailable (an extension in the schema is enough to skip
  objects).
- A **named** object that is skipped makes the Database `Available=False`
  with reason `ObjectGrantSkipped`; the other grants are still applied.
- An object that stops qualifying (its owner changed, a function became
  `SECURITY DEFINER` under a superuser) has the privileges pgop added revoked.

### Tracking and revoking

- On every reconcile pgop compares what each grantee holds on each selected
  object with the declared privileges and grants only what is missing, as
  for the other grants (see [What pgop tracks](#what-pgop-tracks)).
- `status.managedObjectGrants` records, **per object and grantee**, the
  privileges and grant options pgop added (written before the `GRANT`), with
  the object's OID. An object that an entry no longer selects (the entry or a
  name was removed, a `"*"` became a list, the object no longer qualifies)
  has exactly those revoked; privileges granted by hand or held before are
  never revoked.
- pgop follows an object by its OID: a renamed object keeps its entry (the
  recorded name is updated), and the revoke reaches it wherever it is now. A
  dropped object is forgotten; a new object created under the same name is
  another object, with an entry of its own.
- The ledger holds at most 4096 entries (one per object and grantee) and
  about 512 KiB. A change that would exceed either grants nothing new
  (`TooManyGrants`); revokes still run. For large schemas, grant to one
  group role and make the application roles members of it.
- `status.objectGrants[].granted` counts the objects with a grant to an
  allowed grantee; a kind and schema whose objects could not be listed is
  left out of `status.objectGrants`.
- Grantees follow the [grantee policy](#grantee-policy); grants to a Role
  being deleted are paused and revoked (see
  [Roles: deletion](roles.md#deletion)).
- Column privileges (`GRANT SELECT (col) ON ...`) are not supported.

## Default Privileges

`schemas[].defaultPrivileges` sets privileges for objects a role creates in
the schema **later** (`ALTER DEFAULT PRIVILEGES FOR ROLE ... IN SCHEMA ...
GRANT ...`). The typical case is a migrator role that creates tables which a
read-only role must be able to read:

```yaml
spec:
  schemas:
    - name: app
      owner: app_owner
      grants:
        - role: app_migrator
          privileges: [USAGE, CREATE]
        - role: app_ro
          privileges: [USAGE]
      defaultPrivileges:
        - forRole: app_migrator        # tables app_migrator creates in app ...
          role: app_ro                 # ... can be read by app_ro
          kind: table
          privileges: [SELECT]
        - forRole: app_migrator
          role: app_rw
          kind: sequence
          privileges: [USAGE, SELECT]
      objectGrants:                    # tables that exist already
        - role: app_ro
          kind: table
          objects: ["*"]
          privileges: [SELECT]
```

- `forRole` is a raw PostgreSQL role name. It must be managed by a Role of the
  same Cluster (created or adopted by it, recorded in its status, carrying its
  marker) and must not be a superuser; `postgres`, `pgop_*` (the operator),
  `pg_*` and `PUBLIC` are rejected by the API server. Anything else is refused
  with reason `DefaultPrivilegeNotAllowed` (and what pgop set for it earlier
  is removed); a `forRole` that does not exist yet is retried. Default
  privileges only affect objects `forRole` creates in this schema of this
  database.
- `role` follows the [grantee policy](#grantee-policy) (`PUBLIC` allowed).
- `kind`: `table` (tables, views, materialized views, foreign tables),
  `sequence`, `function` (functions and procedures) or `type`, with the
  privileges of `objectGrants`.
- Default privileges only apply to objects created **after** they were set.
  Use `objectGrants` with `"*"` for the existing ones (as above).
- pgop records what it added in `status.managedDefaultPrivileges` and, when an
  entry or a privilege is removed, runs `ALTER DEFAULT PRIVILEGES ...
  REVOKE` for exactly that. Objects created in the meantime keep the
  privileges they were created with (PostgreSQL copies them into the
  object's own ACL); revoke them with `objectGrants` removal or by hand.
- Deleting the `forRole`'s or the grantee's Role pauses the entry and
  removes what pgop set. The grantee's Role deletion also revokes the
  privileges the entry's default privileges gave it on the objects `forRole`
  owns in the schema, for entries still declared in a Database's spec or
  recorded in its ledger (see [Roles: deletion](roles.md#deletion)).

## Grantee policy

pgop grants as a superuser, so the grantee of every entry in `grants`,
`schemas[].grants`, `schemas[].objectGrants`, `schemas[].defaultPrivileges`
and `extensions[].grants` must be one of:

- `PUBLIC` (see below);
- a role managed by a Role of the same Cluster: the Role created or adopted it
  (its `status.roleName` and `status.clusterUID` record it) and the role
  carries that Role's signed ownership marker; or
- a role a Cluster editor lists in the Cluster's
  [`spec.rolePolicy.allowedExistingRoles`](clusters.md#role-policy) (roles a
  DBA or a bootstrap Job created).

Superusers (also when allowlisted), `postgres`, `none`, the operator's `pgop_*`
roles and predefined `pg_*` roles are never accepted. The API server rejects
the reserved names; the operator checks the rest on every reconcile. A grant
to a grantee that is not allowed is not applied, is **revoked if pgop granted
it earlier** (for example before the Cluster's policy changed), and is
reported with reason `GranteeNotAllowed`; the other grants are still applied.
A Role that is created later is picked up as soon as it is recorded (the
Database reconciles when the Role changes).

### PUBLIC

`role: PUBLIC` grants to the `PUBLIC` pseudo-role, that is, to every role,
including roles created later. Write it in upper case. pgop always emits the
bare keyword `PUBLIC`, never a quoted identifier: PostgreSQL reads both
`PUBLIC` and `"public"` as the pseudo-role, while `"PUBLIC"` (quoted, upper
case) would be an ordinary role of that name. A role literally named `PUBLIC`
can therefore never be a grantee. `withGrantOption` cannot be set for
`PUBLIC` (PostgreSQL does not allow it).

!!! warning
    `CREATE` on a schema for `PUBLIC` lets every role create objects there,
    which can shadow objects for other roles whose `search_path` includes the
    schema. The operator's own sessions pin `search_path` and are not
    affected.

## Default PUBLIC privileges

PostgreSQL gives `PUBLIC` `CONNECT` and `TEMPORARY` on every new database and
`USAGE` on the schema `public` (and, before PostgreSQL 15, `CREATE` on it).
`spec.publicPrivileges` revokes them:

```yaml
spec:
  publicPrivileges:
    connect: false            # REVOKE CONNECT ON DATABASE ... FROM PUBLIC
    temporary: false          # REVOKE TEMPORARY ON DATABASE ... FROM PUBLIC
    publicSchemaUsage: false  # REVOKE USAGE ON SCHEMA public FROM PUBLIC
    publicSchemaCreate: false # REVOKE CREATE ON SCHEMA public FROM PUBLIC
  grants:
    - role: app_user          # roles that should still connect need their own grant
      privileges: [CONNECT]
```

- `false` revokes the privilege from `PUBLIC`; unset or `true` leaves
  PostgreSQL's default alone.
- pgop revokes a privilege, and records it in `status.revokedPublicPrivileges`,
  only while `PUBLIC` actually holds it. The check runs on every reconcile, so
  a privilege granted back to `PUBLIC` by hand is revoked again.
- When a field is unset or set back to `true`, pgop grants **exactly what it
  revoked** back to `PUBLIC`, and nothing it did not revoke (for example
  `CREATE` on `public`, which PostgreSQL 15 and later do not grant).
- A `PUBLIC` entry in `grants` (or in `grants` of the schema `public`) that
  grants the same privilege is rejected by the API server; the operator
  leaves such a privilege alone and reports `PublicPrivilegeConflict`.
- If the schema `public` does not exist, the `publicSchema*` fields do nothing.
- pgop revokes as the object's owner. When `PUBLIC` also holds the privilege
  from another grantor (a role that has it `WITH GRANT OPTION` granted it to
  `PUBLIC`), that grant survives; the Database reports
  `PublicPrivilegeStillHeld` until it is revoked by that role, or with
  `CASCADE` from that role's grant option.
- What pgop revokes is recorded before the `REVOKE`, so a lost status write
  cannot make pgop forget to grant it back.

!!! warning "Locking roles out"
    With `connect: false`, only the database owner, superusers and roles with
    their own `CONNECT` grant can connect. Grant `CONNECT` in `grants` to every
    other role that needs the database.

## How two resources interact

Every privilege pgop tracks belongs to exactly one resource: database,
schema, extension and object grants and default privileges are on the
Database's own PostgreSQL database, which no other
Database manages (a second Database with the same PostgreSQL name reports
`DuplicateDatabaseName` and does nothing), and a Role's parameter grants and
memberships are for its own role (`DuplicateRoleName` likewise). So two
resources never track the same privilege, and removing a grant from one
resource never revokes a privilege another resource declares: two Databases
that both grant `CONNECT` to `app_reader` each grant it on their own
database, and removing it from one leaves the other alone.

Should two resources ever declare the same privilege, "grant wins": every
resource grants what it declares and finds missing on every reconcile, so
a privilege revoked by one is restored by the next reconcile of the other.
Privileges granted outside pgop that are also declared are not tracked by
the declaring resource and never revoked by it (see
[What pgop tracks](#what-pgop-tracks)). Deleting a Role revokes the privileges its role holds on
every database and schema, whatever Database granted them, and the object
grants and default privileges the Databases recorded for it; the Databases
stop granting to it while it is being deleted.

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
    myapp.tenant: acme            # needs rolePolicy.allowedSettingPrefixes: [myapp]
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

- Parameters the server knows (listed in `pg_settings`: the built-in ones,
  and those of extensions loaded in every session) are applied only with
  context `user`, which any role may set in its own session. Superuser-only
  parameters (for example `log_statement`, `session_preload_libraries`), and
  parameters that cannot be set per database anyway (`postmaster`, `sighup`,
  `internal`, `backend`), are refused.
- Custom parameters the server does not know (placeholders, such as
  `myapp.tenant`) are applied only when their namespace (the part before the
  first dot) is listed in the Cluster's
  [`rolePolicy.allowedSettingPrefixes`](clusters.md#role-policy). Their
  context cannot be checked, and PostgreSQL trusts a value stored by a
  superuser: if an extension that defines the parameter as superuser-only is
  loaded later (for example `auto_explain`, `plperl.on_plperl_init`, which
  runs Perl code, or `postgis.gdal_enabled_drivers`, which reads server
  files), the stored value applies even though the role could not set it
  itself. So without a Cluster editor's decision no placeholder is set, and
  extension namespaces whose settings run code, read server files or are
  superuser-only (`plperl`, `pltcl`, `plv8`, `plpgsql`, `postgis`,
  `auto_explain`, `pg_stat_statements`, `pgaudit`, `cron`, ...; the full list
  is `DeniedSettingPrefixes` in the API) can never be listed. `plpgsql.*`
  is among them because `plpgsql.variable_conflict` is superuser-only; set the
  user-context `plpgsql.*` options in sessions or function `SET` clauses
  instead.
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
- The same policy (and code) applies to a Role's
  [`settings` and `databaseSettings`](roles.md#role-settings).

!!! warning "Remaining risk"
    User-context parameters still affect every other session in the
    database (the operator's own sessions are pinned, see below), and so do
    placeholders in the namespaces the Cluster allows: only list namespaces
    that belong to your applications, never to an extension. Restrict who may
    create or edit Database resources accordingly.

!!! note "Upgrade / breaking change"
    Earlier versions applied every custom placeholder (`myapp.tenant`). They
    are now refused (`SettingNotAllowed`, and reset if pgop set them) until
    the Cluster lists their namespace in `rolePolicy.allowedSettingPrefixes`.

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

## Extensions

```yaml
spec:
  extensions:
    - name: pg_trgm                 # trusted: installed into public
    - name: hstore
      version: "1.8"                # changing it later runs ALTER EXTENSION ... UPDATE TO
    - name: earthdistance           # untrusted: needs the Cluster's allowedExtensions
      schema: geo                   # created by pgop (owned by the operator) if missing
      cascade: true                 # also installs cube, which must pass the policy too
    - name: pg_partman              # in a custom image; untrusted
      schema: partman
      dropOnRemoval: false          # the default: removing the entry leaves it installed
      grants:
        - role: rs_owner
          schema: [USAGE, CREATE]
          tables: [ALL]
          sequences: [ALL]
          functions: [EXECUTE]
        - role: rs_control
          schema: [USAGE]
          tables: [SELECT]
```

The operator runs `CREATE EXTENSION` and `ALTER EXTENSION ... UPDATE` as a
superuser, so an extension's install and update scripts run **as a
superuser** inside the database. Installing an extension is therefore a
privileged operation, and a Database writer (who must not become
superuser-equivalent) can only request it within the limits below. Every
refusal is reported in the `Available` condition and in
`status.extensions[].reason`/`message`; the other extensions, schemas and
grants are still reconciled.

### Extension policy

An extension is only installed (or updated) when

- the server marks the requested version (the default version when
  `version` is unset) as **trusted** (`pg_available_extension_versions.trusted`;
  for example `pg_trgm`, `pgcrypto`, `uuid-ossp`, `citext`, `hstore`,
  `btree_gist`, `ltree`, `cube`), or
- the Cluster lists it in
  [`spec.rolePolicy.allowedExtensions`](clusters.md#role-policy).

Trusted extensions are those PostgreSQL lets any role with `CREATE` on the
database install, so pgop installing them gives the Database writer nothing
extra. Untrusted extensions, such as `file_fdw`, `dblink`, `earthdistance`,
`plpython3u`, `postgis`, `pg_partman` or anything added to a custom image, can
give access to the server's files, network or code execution, or were simply
not reviewed for that; a cluster administrator has to allow them. Listing an
extension there is the Cluster editor's explicit decision to let pgop install
it as a superuser; grants on its objects still follow the rules in
[Grants on extension objects](#grants-on-extension-objects).

Trust is checked for **every version whose scripts would run**, not only the
requested one: `CREATE EXTENSION` may run an older version's install script
and then every update script up to the requested version, and
`ALTER EXTENSION ... UPDATE` runs every update script on its path. pgop reads
the update paths (`pg_extension_update_paths`) and requires each version on
them to be trusted (PostgreSQL itself checks every step for a non-superuser);
since it cannot see which path PostgreSQL will pick for an install, it checks
all paths that end at the requested version and start at an installable
version. A version the server does not list in
`pg_available_extension_versions` counts as not trusted. If any of them is not
trusted, the extension is only installed when the Cluster lists it, and the
[stricter schema rule](#the-schema-an-extensions-scripts-run-in) applies.

A refused extension is not installed (reason `ExtensionNotAllowed`). An
installed extension the policy no longer allows is reported the same way, is
not updated, and the grants pgop made on its objects are revoked; it is not
dropped.

### Dependencies and `cascade`

An extension can require others (`earthdistance` requires `cube`). Without
`cascade`, they must be installed already: list them before the extension.
Otherwise the Database reports `ExtensionDependencyMissing`.

With `cascade: true`, pgop runs `CREATE EXTENSION ... CASCADE`, which installs
the missing dependencies (recursively, at their default versions) **as a
superuser too**. So pgop resolves the whole dependency list first, from
`pg_available_extension_versions.requires`, and checks every extension it
would install against the same policy. If one of them is neither trusted nor
allowed, **nothing is installed** (`ExtensionNotAllowed`, naming the
dependency). A dependency the server does not have is reported as
`ExtensionVersionNotAvailable`. Dependencies installed by `cascade` go into the
schema their control file names, or else into the extension's schema; they
are not listed in `status.extensions` and are never dropped by pgop.

### The schema an extension's scripts run in

While an extension's script runs, `search_path` is the extension's target
schema (followed by the schemas of the extensions it requires). A role that
can create objects in one of those schemas can plant a function or operator
there that the superuser-run script then calls instead of the one it meant
(the class of CVE-2022-2625 and CVE-2023-39417; third-party scripts are not
always written defensively). So before every `CREATE EXTENSION` and
`ALTER EXTENSION ... UPDATE` pgop checks each of these schemas and refuses
(`ExtensionSchemaNotAllowed`, nothing runs) unless:

- the schema is owned by a superuser, by the database owner (also through
  `pg_database_owner`, the owner of `public` since PostgreSQL 15), or is a
  schema the Database manages (see
  [Which schemas a Database manages](#which-schemas-a-database-manages)); and
- no other role, and not `PUBLIC`, holds `CREATE` on it (superusers, the
  schema's owner and the database owner may).

For an extension that is **not trusted** (installed only because the Cluster
allows it) the rule is stricter: its scripts were never meant to be safe for a
non-superuser to install, and the database owner is controlled by whoever
writes Databases. So each schema must be **owned by a superuser and writable
by superusers only**. In practice:

- Without `schema`, an untrusted extension goes to `public`, which is owned
  by the database owner: allowed only for a Database without `owner` (owned by
  the operator). Give it a `schema` of its own instead.
- A `schema` that does not exist and is not listed in `spec.schemas` is
  **created by pgop, owned by the operator** (a superuser), so no other role
  can write to it. pgop never drops it. Such a schema is not one `schemas`
  manages (listing it there reports `SchemaNotManaged`): grant on it with
  `extensions[].grants[].schema`. A schema listed in `spec.schemas` is
  created there first, owned by the database owner (fine for trusted
  extensions, refused for untrusted ones).
- On PostgreSQL 14 and older, `PUBLIC` holds `CREATE` on `public` by default:
  revoke it with
  [`publicPrivileges.publicSchemaCreate: false`](#default-public-privileges)
  before installing extensions there.
- Granting `CREATE` on an extension's schema to another role (in
  `schemas[].grants` or `extensions[].grants[].schema`) makes later updates
  of every extension in that schema refused. Remove the grant for the update
  (pgop revokes what it granted) and add it back afterwards. Objects that role
  already created in the schema are not checked: review the schema before
  updating.

pgop creates every missing schema an install needs itself, with a plain
`CREATE SCHEMA` owned by the operator, before it runs `CREATE EXTENSION`:
the extension's `schema`, and the schemas control files name (of the
extension and of the dependencies `cascade` installs). `CREATE EXTENSION`
would otherwise create a control-file schema itself, but it uses a schema of
that name that already exists without checking who owns it, and between
pgop's check and the `CREATE EXTENSION` the database owner could create it
with planted objects. If someone creates the schema first, pgop checks it
again with the rules above and refuses (`ExtensionSchemaNotAllowed`) unless
it qualifies.

When the control file of an extension names a schema (`schema = ...`), it is
always installed there, and a different `schema` in the spec is refused.
pgop never moves an installed extension: a `schema` that differs from where
the extension is installed is reported as `ExtensionSchemaMismatch` (and the
extension is not updated).

### Versions

- Without `version`, the default version is installed, and an installed
  extension is left at its version (pgop does not update it on its own).
- With `version`, a missing extension is installed at that version, and an
  installed one at another version is updated with
  `ALTER EXTENSION ... UPDATE TO '<version>'` when the server has an update
  path (`pg_extension_update_paths`). Objects the update adds are picked up by
  the [grants](#grants-on-extension-objects) on the same reconcile.
- A version **lower** than the installed one is refused
  (`ExtensionDowngradeNotAllowed`), even if the extension ships a downgrade
  script: downgrades are not something a Database writer can undo.
  Versions are compared numerically (`1.10` is higher than `1.9`); versions
  that are not dotted numbers (`1.0beta1`) are only checked for an update
  path.
- A version the server does not have, or cannot update to, is reported as
  `ExtensionVersionNotAvailable`; the extension stays at its version.
- The policy and the schema rules apply to the version being installed or
  updated to (the `trusted` flag is per version).
- `version` must match `^[A-Za-z0-9][A-Za-z0-9._+~-]*$` (at most 64
  characters); names and versions are always sent as quoted identifiers and
  literals.

`status.extensions[]` reports each extension's installed `version` and
`schema`, whether pgop `created` it, and the `reason` and `message` when it is
not as requested.

### Grants on extension objects

An extension's objects belong to the superuser that installed it, so their
owner cannot grant on them for the application (for example pg_partman's
configuration tables and maintenance functions). `extensions[].grants` lets
pgop do it:

| Field | Privileges | Applied to |
|-------|------------|------------|
| `schema` | `USAGE`, `CREATE`, `ALL` | the extension's own schema |
| `tables` | `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `MAINTAIN` (PostgreSQL 17+), `ALL` (all but `MAINTAIN`) | the extension's plain and partitioned tables |
| `sequences` | `USAGE`, `SELECT`, `UPDATE`, `ALL` | the extension's sequences |
| `functions` | `EXECUTE` (or `ALL`) | the extension's SQL and PL/pgSQL functions and procedures that are not `SECURITY DEFINER` |

- The objects are the extension's members (`pg_depend` entries of type `e`),
  listed again on every reconcile, so objects an update adds are granted on
  too. Only what a grantee is missing is granted.
- **Function safety rule.** `EXECUTE` is only granted on functions and
  procedures written in `sql` or `plpgsql` that are not `SECURITY DEFINER`.
  Two kinds of member functions are skipped, because granting on them would
  let a Database writer escalate beyond their database:
    - `SECURITY DEFINER` functions owned by the superuser run **as that
      superuser**;
    - C (and other internal-language) functions often have no access check
      of their own and rely on `REVOKE EXECUTE ... FROM PUBLIC` instead
      (`dblink_connect_u`, file readers, `pageinspect`, ...).

    An invoker SQL or PL/pgSQL function runs with the caller's privileges, so
    it gives nothing the caller could not do itself. If an update turns a
    function pgop granted `EXECUTE` on into one that no longer qualifies, pgop
    revokes that `EXECUTE`.
- Table privileges are only granted on plain and partitioned tables: a view
  or materialized view reads its tables with its owner's (superuser)
  privileges, and a foreign table may read server files. `TRIGGER` is not
  offered: a trigger on a table the extension's superuser-run code writes to
  (a background worker, for example) would run as that superuser. Write
  privileges on an extension's configuration tables still influence what that
  code does: grant them only to roles you would trust with the extension.
- **Extensions the server does not trust** (installed only because the
  Cluster lists them in `allowedExtensions`) only get read-only grants:
  `USAGE` on the schema, `SELECT` on tables and sequences, and `EXECUTE` (by
  the rules above). `CREATE` on their schema would let the grantee plant
  objects where the extension's superuser-run code (background workers,
  `SECURITY DEFINER` functions, a DBA calling its functions) resolves names,
  and write privileges (`INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`,
  `REFERENCES`, `MAINTAIN`, sequence `USAGE`/`UPDATE`) would let it change
  the state that code reads: the same situation the stricter install rule
  forbids. They are refused with reason `ExtensionGrantNotAllowed` and
  revoked if pgop granted them earlier (for example before an update made a
  version untrusted).
- Skipped objects are counted in `status.extensions[].skippedObjects`; they do
  not make the Database not ready.
- `schema` grants are only applied on a schema of the extension's own: not
  `public`, not a system schema, and not a schema listed in `spec.schemas`
  (grant on those in [`schemas[].grants`](#schema-grants); reason
  `ExtensionGrantNotAllowed`).
- Grantees follow the [grantee policy](#grantee-policy) (`PUBLIC`, roles of
  the Cluster's Roles, or `allowedExistingRoles`). Grants are only applied
  while the extension is installed and allowed by the policy.
- pgop records what it added in `status.managedExtensionGrants` (per
  extension, grantee and kind, before granting, as for the other ledgers).
  When a grant, a privilege or the extension's entry is removed, pgop revokes
  the recorded privilege **from every object of that kind of the extension**,
  including the same privilege granted on one of them by hand. Grants on an
  extension that was dropped are forgotten (its objects are gone); a privilege
  on a schema another extension still grants to the same role is kept. When
  the state of an extension cannot be read (a transient error), its tracked
  grants are left alone.
- Deleting a grantee's Role revokes its privileges on extension objects too
  (see [Roles: deletion](roles.md#deletion)).

### Removing an extension

Removing an extension from `extensions` **does not drop it**: `DROP EXTENSION`
deletes every column, index and object that uses its types or functions,
which is data loss. Grants pgop made on its objects are revoked.

To drop it, set `dropOnRemoval: true` and let the Database reconcile (the
setting is recorded in `status.extensions[].dropOnRemoval`), then remove the
entry. pgop then runs `DROP EXTENSION ... RESTRICT`, and only for the
installation it created for this Database: `status.extensions[].created` is
recorded before `CREATE EXTENSION` runs, and the extension's `oid` and `owner`
right after it succeeds. An extension that was there before, that someone
else created while pgop was creating it (`ExtensionNotManaged`), that was
dropped and created again since (another `oid`), or whose creation pgop could
not confirm (the status write after `CREATE EXTENSION` was lost) is never
dropped: pgop stops treating it as its own. pgop never drops with `CASCADE`: when other objects depend on the
extension, the Database reports `ExtensionDropBlocked` and retries until they
are gone (or the entry is listed again). Dependencies installed by `cascade`
are not dropped. `status.extensions` holds at most 128 entries (the spec's
extensions and the removed ones pgop still has to drop); removed extensions
beyond that are no longer tracked, not dropped, and reported.

!!! warning "Extensions must exist in the image"
    The operator does **not** install packages: `CREATE EXTENSION` only
    succeeds if the extension's files are present in the running image
    (otherwise: `ExtensionVersionNotAvailable`). The default `postgres:18`
    image includes the contrib extensions but **not** PostGIS, TimescaleDB or
    pg_partman: set the Cluster's `spec.image` to an image that has them
    (e.g. `postgis/postgis:18-3.5`).

### Common extensions

```yaml
extensions:
  - name: uuid-ossp     # UUID generation (trusted)
  - name: pg_trgm       # trigram matching (trusted)
  - name: pgcrypto      # cryptographic functions (trusted)
  - name: citext        # case-insensitive text (trusted)
  - name: postgis       # geographic data (untrusted, custom image)
    schema: postgis
  - name: timescaledb   # time-series (untrusted, custom image)
    schema: timescale
```

PostgreSQL 13 and later also let a role with `CREATE` on the database install
trusted extensions itself (grant it in [`grants`](#database-grants)); pgop is
only needed for untrusted ones and for grants on extension objects.

## Security model

A Database writer is trusted with the contents of **their** database, not with
the server:

- they choose the owner (a Role resource), schema owners and grants, and
  receive the owner's credentials in the connection Secret;
- settings are limited to user-context parameters minus a denylist (see
  [Which parameters may be set](#which-parameters-may-be-set));
- extensions are limited to trusted ones unless the Cluster allows more (see
  [Extension policy](#extension-policy)), dependencies installed by `cascade`
  included; their superuser-run scripts only run in schemas no untrusted role
  can write to (see
  [the schema rules](#the-schema-an-extensions-scripts-run-in));
- grants on extension objects never include `EXECUTE` on `SECURITY DEFINER`
  or C functions, privileges on views, or `TRIGGER` (see
  [Grants on extension objects](#grants-on-extension-objects));
- object grants only reach objects in schemas the Database manages whose
  owner is in the Cluster's trust domain (a role of the Cluster's Roles that
  is not a superuser, or the operator under the extension rules); objects of other superusers or
  outside roles, extension members, and the operator's `SECURITY DEFINER`
  and C functions, views and `TRIGGER`/`MAINTAIN` on its tables are refused
  (see [Which objects pgop grants on](#which-objects-pgop-grants-on)).
  pgop's `GRANT` runs as a superuser and could reach any of them; these
  restrictions are what keeps a Database writer inside the trust domain;
- default privileges are only set for non-superuser roles managed by the
  Cluster's Roles, never for the operator, `postgres` or a DBA's role (see
  [Default Privileges](#default-privileges));
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

- grantees (`grants[].role`, `schemas[].grants[].role`) are limited to
  `PUBLIC`, roles managed by Roles of the same Cluster and roles a Cluster
  editor allowlisted (see [Grantee policy](#grantee-policy));
- grants only ever name objects of the Database's own PostgreSQL database
  (its own schemas): there is no field that names another database or
  Cluster.

- an existing schema is only re-owned or granted on when the Database
  manages it (see
  [Which schemas a Database manages](#which-schemas-a-database-manages)), so
  schemas created by extensions or other roles cannot be taken over.

Known gap: `schemas[].owner` accepts any PostgreSQL role name on the Cluster
for schemas the Database manages (giving ownership away is not an escalation
for the writer).

### Ownership of the PostgreSQL database

As for [roles](roles.md#ownership-of-the-postgresql-role), pgop only changes
the owner, settings, grants, extensions and schemas of, or drops, a database
this Database owns: one its `status.databaseName` records on the referenced
Cluster (`status.clusterUID` matches the Cluster's UID; `clusterRef` is
immutable), one pgop creates in this reconcile (a plain `CREATE DATABASE`; one
created by someone else in the meantime is reported, not altered), or an
existing one a **Cluster editor** listed in the
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
- a Database re-created without its status, or whose Cluster was deleted and
  re-created, reports `DatabaseNotManaged` (and deleting it drops nothing)
  until a Cluster editor lists the database in `adoptableDatabases`; removing
  the name from the list later does not un-adopt it. Databases recorded by an
  earlier pgop (no `status.clusterUID`) count as recorded when their
  credentials Secret, controlled by the Database, points at this Cluster;
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
- **Extensions** (see [Extensions](#extensions)):
    - an untrusted extension (listed in the Cluster's `allowedExtensions`) is
      only installed or updated in a schema owned by a superuser that only
      superusers can create in. Without `schema` it goes to `public`, which
      the database owner owns: a Database with an `owner` now reports
      `ExtensionSchemaNotAllowed`. Set `schema` to a new schema (pgop creates
      it, owned by the operator). Extensions already installed are not
      touched until they are updated;
    - any extension is refused in a schema `PUBLIC` or another role can
      create in, notably `public` on PostgreSQL 14 and older (revoke with
      `publicPrivileges.publicSchemaCreate: false`);
    - `version` is now enforced: an installed extension at another version is
      updated (or the downgrade reported), where `CREATE EXTENSION IF NOT
      EXISTS` used to ignore it;
    - schemas are created before extensions are installed, so an extension can
      be installed into a schema of `schemas`;
    - extension names must be unique, `version` must match
      `^[A-Za-z0-9][A-Za-z0-9._+~-]*$`, and `schema` cannot be a system
      schema;
    - `status.installedExtensions` only lists extensions of `extensions` that
      are installed; `status.extensions` has the details. Extensions installed
      before this release are not recorded as created by pgop, so
      `dropOnRemoval` never drops them.
- **Schema grants are tracked and revoked.** `schemas[].grants` entries
  pgop adds from now on are revoked when they are removed (see
  [Schema Grants](#schema-grants)). There is no opt-out.
- **Only what pgop adds is tracked** (see [What pgop tracks](#what-pgop-tracks)).
  Earlier versions never recorded schema grants, so schema privileges they
  granted are already held after upgrading: pgop does not record them, treats
  them like grants made by hand and does **not revoke** them when they leave
  the spec, also not when their grantee is now refused by the grantee policy.
  Database and parameter grants recorded by v0.15 keep their ledger entries
  (`withGrantOption: true` is read as the grant option for all recorded
  privileges) and are revoked when removed, including PostgreSQL defaults
  v0.15 recorded. Review and revoke untracked privileges by hand, for example
  per database:

  ```sql
  -- schema privileges held by roles other than the schema owner
  SELECT n.nspname, a.grantee::regrole AS grantee, a.privilege_type, a.is_grantable
  FROM pg_namespace n, aclexplode(n.nspacl) a
  WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
    AND a.grantee <> n.nspowner;            -- grantee "-" is PUBLIC
  -- database privileges
  SELECT d.datname, a.grantee::regrole AS grantee, a.privilege_type, a.is_grantable
  FROM pg_database d, aclexplode(d.datacl) a WHERE a.grantee <> d.datdba;
  ```

  Once revoked by hand, a privilege still declared is granted again and
  tracked.
- **Grantees are checked** (see [Grantee policy](#grantee-policy)): a grant
  to a role that no Role of the Cluster manages (and that the Cluster's
  `rolePolicy.allowedExistingRoles` does not list), to a superuser, or to
  `postgres`, `pgop_*` or `pg_*` roles is refused with `GranteeNotAllowed`,
  and revoked if pgop recorded adding it (see above for grants made by
  earlier versions). `postgres`, `none`, `pgop_*` and `pg_*` grantees, a
  lower-case `public` and grantee names longer than 63 characters are
  rejected by the API server.
- **Existing schemas are only taken over when the Database manages them**
  (see [Which schemas a Database manages](#which-schemas-a-database-manages)):
  earlier versions ran `ALTER SCHEMA ... OWNER TO` on any existing schema
  listed in `schemas`. Schemas an earlier version reconciled are recorded in
  `status.createdSchemas` and stay managed. A schema without a declared
  `owner` is now created owned by the database's owner instead of the
  operator.
- `PUBLIC` must be written in upper case and cannot get `withGrantOption`.
- `schemas` names must be unique (at most 64 schemas) and each schema's
  `grants` lists a role at most once (at most 16 grants).
- New: `schemas[].objectGrants` and `schemas[].defaultPrivileges` (see
  [Object Grants](#object-grants)). Nothing changes for Databases that do not
  use them. A Database that does gets the `ObjectGrantsComplete` condition.

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
`SELECT` are not schema privileges: grant them with
[`objectGrants`](#object-grants) and [`defaultPrivileges`](#default-privileges),
or in your migrations.

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

