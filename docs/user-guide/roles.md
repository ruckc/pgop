# Roles

A Role resource represents a PostgreSQL role (user) within a cluster.

## Overview

The Role controller:

1. Connects to the referenced PostgreSQL cluster
2. Creates or updates the role with specified permissions (never as a
   superuser; privileged attributes only as far as the Cluster's
   [role policy](clusters.md#role-policy) allows)
3. Auto-generates a password and creates a credentials Secret
4. Manages role memberships (GRANT, option changes, and REVOKE of memberships
   it granted), refusing memberships the [membership policy](#membership-policy)
   does not allow
5. Grants privileges on configuration parameters (`parameterGrants`, PostgreSQL 15+)
6. Sets per-role defaults for configuration parameters (`settings`,
   `databaseSettings`, see [Role Settings](#role-settings))
7. Cleans up the role on deletion, revoking its privileges first (see [Deletion](#deletion))

See [Security model](#security-model) for what a Role writer can and cannot
obtain.

## Example

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: app-user
  namespace: default
spec:
  clusterRef:
    name: my-cluster
  login: true
  createDB: false
  connectionLimit: 100
  memberships:
    - role: app_read_role
```

## Spec Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `clusterRef.name` | string | **required** | Name of the Cluster resource (same namespace) |
| `roleName` | string | `metadata.name` | Role name in PostgreSQL (see [PostgreSQL Role Name](#postgresql-role-name)) |
| `login` | bool | `true` | Can role log in? |
| `createDB` | bool | `false` | Can role create databases? |
| `createRole` | bool | `false` | Can role create other roles? **Privileged**: needs the Cluster's `rolePolicy.allowedAttributes` |
| `inherit` | bool | `true` | Inherit privileges from member roles |
| `replication` | bool | `false` | Can role initiate replication? **Privileged**: needs the Cluster's `rolePolicy.allowedAttributes` |
| `bypassRLS` | bool | `false` | Bypass row-level security? **Privileged**: needs the Cluster's `rolePolicy.allowedAttributes` |
| `connectionLimit` | int | `-1` | Max concurrent connections (-1 = unlimited) |
| `memberships` | []RoleMembership | - | Roles this role is a member of, with grant options (see [Memberships](#memberships)) |
| `memberOf` | []string | - | **Deprecated**, use `memberships`. Roles this role is a member of |
| `revokeRemovedMemberships` | bool | `true` | Revoke pgop-granted memberships removed from the spec (transitional opt-out) |
| `passwordSecretRef` | SecretKeySelector | - | Take the password from a key of a Secret you manage (see [Password Source](#password-source)) |
| `passwordRotation.every` | duration | - | Rotate the generated password on this interval, e.g. `720h` (minimum `1h`; see [Password Rotation](#password-rotation)) |
| `parameterGrants` | []ParameterGrantSpec | - | `GRANT SET ON PARAMETER` privileges (see [Parameter Grants](#parameter-grants)) |
| `settings` | map[string]string | - | `ALTER ROLE ... SET` defaults for this role's sessions (see [Role Settings](#role-settings)) |
| `databaseSettings` | []RoleDatabaseSettings | - | `ALTER ROLE ... IN DATABASE ... SET` defaults, per PostgreSQL database (see [Role Settings](#role-settings)) |

## Status

| Field | Description |
|-------|-------------|
| `ready` | Whether the role exists in PostgreSQL |
| `roleName` | The effective PostgreSQL role name that was reconciled |
| `secretName` | Name of the auto-generated credentials secret (`<cluster>-<role>-credentials`) |
| `managedMemberships` | PostgreSQL roles whose membership pgop granted to this role |
| `managedParameterGrants` | Parameter privileges pgop granted to this role |
| `managedSettings` | Parameter names pgop set with `ALTER ROLE ... SET` |
| `managedDatabaseSettings` | Per database, the parameter names pgop set with `ALTER ROLE ... IN DATABASE ... SET` |
| `passwordHash` | **Deprecated**, no longer written and cleared on the next reconcile (the fingerprint moved to the credentials Secret, see [Password Source](#password-source)) |
| `passwordRotatedAt` | When the operator last generated the password; the rotation schedule counts from it |
| `passwordRotationRequest` | Last `pgop.ruck.io/rotate-password` annotation value acted on |
| `conditions` | Detailed status conditions |

## Credentials Secret

The operator creates a Secret named **`<cluster-name>-<role-name>-credentials`**
(e.g. `my-cluster-app-user-credentials`). The name is deterministic — it is
derived from `clusterRef.name` and the Role's own name — so you can reference it
from other manifests (including a Helm chart at template time) **before**
`status.secretName` is populated.

```yaml
data:
  username: app-user                          # = the PostgreSQL role name
  password: <generated, or copied from passwordSecretRef>
  host: my-cluster.default.svc.cluster.local
  port: "5432"
  sslmode: disable                            # verify-full once Cluster TLS is active
  uri: postgresql://app-user:<password>@my-cluster.default.svc.cluster.local:5432/postgres?sslmode=disable
  ca.crt: <PEM>                               # only while Cluster TLS is active
```

!!! note
    This Secret does **not** contain a `database` key, and its `uri` points at
    the `postgres` maintenance database. Apps should usually use the
    [Database](databases.md) per-database Secret, whose `database` and `uri`
    name the application database.

The connection keys (`host`, `port`, `sslmode`, `uri`, `ca.crt`) follow the
Cluster: they are updated when its port or [TLS](clusters.md#tls) state
changes. The `password` (and the `uri` built from it) changes only when the
[password source](#password-source) changes it: a new value in the
`passwordSecretRef` Secret, or a [rotation](#password-rotation).
Database credentials Secrets of Databases owned by the Role follow it.

## PostgreSQL Role Name

By default the PostgreSQL role is named after the Role resource
(`metadata.name`). Kubernetes names cannot contain underscores, so set
`spec.roleName` when the PostgreSQL name should differ:

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: rs-app            # Kubernetes name (Secret names, labels, references)
spec:
  clusterRef:
    name: my-cluster
  roleName: rs_app        # PostgreSQL role name
```

- `roleName` must match `^[a-z_][a-z0-9_]*$` (lowercase, so it never needs
  quoting), be at most 63 characters, must not start with `pg_` (reserved by
  PostgreSQL) or `pgop_` (reserved for the operator's own roles, such as
  `pgop_operator` and `pgop_replicator`), and must not be `postgres`. A Role
  whose `metadata.name` is `postgres` must set `roleName`. The operator checks
  the same rules again and reports `Available=False` with reason
  `ReservedName` without touching PostgreSQL.
- It is **immutable**: it cannot be added, changed or removed after the Role is
  created. To rename, create a new Role.
- Kubernetes-side names still use `metadata.name`: the credentials Secret is
  `<cluster>-rs-app-credentials`, and a Database refers to this Role with
  `owner: rs-app`. The Secret's `username` key holds `rs_app`.
- `memberships[].role` and `memberOf` entries are raw PostgreSQL role names (for example `rs_app`), not
  Role resource names.

### Ownership of the PostgreSQL role

pgop only creates, alters, sets the password of, or drops a PostgreSQL role
that this Role **owns**:

- a role the Role's own `status.roleName` records **on this Cluster**: pgop
  created or adopted it earlier, and `status.clusterUID` is the UID of the
  Cluster the Role references (Role writers cannot write status; a Cluster
  deleted and re-created under the same name has a new UID, and `clusterRef`
  cannot be changed after creation);
- a role pgop creates in this reconcile (with a plain `CREATE ROLE`: if
  someone creates the role in the meantime, pgop reports `RoleNotManaged`
  instead of altering theirs); or
- an existing role a **Cluster editor** listed in the Cluster's
  [`spec.rolePolicy.adoptableRoles`](clusters.md#role-policy). pgop then takes
  it over (sets its attributes and password, hands the password out, drops it
  when the Role is deleted) and records it in status. It still refuses
  (`RolePolicyViolation`) a role that is a superuser or that belongs, directly
  or indirectly, to roles the [membership policy](#membership-policy) does not
  allow, and alters privileged attributes down to the policy.

Any other existing role (created by a DBA, a bootstrap Job, another Role or
tool) is left alone: nothing is changed in PostgreSQL, no credentials Secret is
written, and the Role reports `Available=False` with reason `RoleNotManaged`.
Without this rule a Role writer could reset the password of any existing role
and log in as it. **Role writers cannot take over existing roles; only a
Cluster editor can allow it.** A role's comment never authorizes a take-over:
whoever administers a role can set its comment.

pgop also stores a signed marker with every role it owns, in the
transaction that creates it:

```sql
-- pgop:v2:Role/<Role name>:<key id>.<HMAC-SHA256 of kind, namespace, Cluster and name>
COMMENT ON ROLE app_user IS 'pgop:v2:Role/app-user:Xk3f9a.3q2-...'
```

The HMAC key is a random per-Cluster key in the Secret
`<cluster>-marker-key`, owned by the Cluster, labeled as managed by pgop (so
`passwordSecretRef` cannot read it) and never mounted into a pod. The marker is
a consistency check: a role recorded in status whose comment was replaced by
something unrelated is left alone (`RoleNotManaged`), and it tells which roles
Roles of the Cluster manage for the membership policy. It does not grant
ownership by itself.

- **Two Roles, one PostgreSQL name:** of two Roles on the same Cluster that
  resolve to the same PostgreSQL name, only the older one (by creation time)
  is reconciled; the other reports `DuplicateRoleName` and never touches or
  drops the role.
- **Deletion** drops only the role recorded in `status.roleName`, and only
  while it carries this Role's marker (or a missing, unsigned or stale one,
  below).
- **Upgrade from earlier pgop, lost key:** a role recorded in
  `status.roleName` whose comment is empty, the unsigned `pgop:v1:Role/<name>`
  marker of an earlier build, or a `pgop:v2:Role/<name>:...` marker naming
  another (lost) key is re-marked on the next reconcile. A marker that names
  the current key but does not verify is never accepted. Roles recorded by an
  earlier pgop (no `status.clusterUID` yet) count as recorded only when the
  Role's credentials Secret for that Cluster (`<cluster>-<role>-credentials`,
  controlled by the Role) proves it; NOLOGIN roles without a Secret need the
  allowlist once. If `<cluster>-marker-key` is
  deleted, the Cluster generates a new key and recorded roles are re-marked
  automatically; nothing has to be done by hand.
- **Re-created Role without status, or re-created Cluster** (restored from
  Git or a backup without status, or a Cluster deleted and re-created under
  the same name): the role is no longer recorded for that Cluster, so the Role
  reports `RoleNotManaged` and deleting it drops nothing, until a Cluster
  editor lists the role in `adoptableRoles`. Removing a name from
  `adoptableRoles` later does not un-adopt it: once adopted, the role is
  recorded in the Role's status and stays managed.

## Memberships

`spec.memberships` makes this role a member of other PostgreSQL roles
(`GRANT <role> TO <this role>`), with per-grant options:

```yaml
spec:
  clusterRef:
    name: my-cluster
  memberships:
    - role: app_read_role          # plain membership, PostgreSQL defaults
    - role: app_owner
      inherit: false               # must SET ROLE app_owner to use its privileges
      set: true
    - role: app_admins
      admin: true                  # may grant app_admins to others
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `role` | string | **required** | PostgreSQL name of the role to be a member of |
| `inherit` | bool | PostgreSQL default | `INHERIT` grant option: use the role's privileges automatically |
| `set` | bool | PostgreSQL default | `SET` grant option: allow `SET ROLE` to the role |
| `admin` | bool | `false` | `ADMIN` grant option: allow granting the role to others |

- `inherit` and `set` require **PostgreSQL 16 or later**. On an older server,
  a Role that sets them fails with a clear `ReconcileError` condition; plain
  and `admin` memberships still work.
- When `inherit` or `set` is unset, a new grant gets PostgreSQL's default
  (`inherit` follows this role's `inherit` attribute, `set` is true) and the
  option of an existing grant is left as it is.
- Changing an option updates the existing grant. Setting `admin` back to
  `false` (or omitting it) runs `REVOKE ADMIN OPTION FOR`.
- Each role may appear only once in `memberships`.

### Membership policy

pgop grants memberships as a superuser, so it checks every target role
(in `memberships` and in `memberOf`) before granting it. A membership is
**refused** when the target role

- is `postgres`, any `pgop_*` role, `pg_execute_server_program`,
  `pg_read_server_files` or `pg_write_server_files` (these are also rejected
  by the API server);
- is any other predefined `pg_*` role that the Cluster does not list in
  [`spec.rolePolicy.allowedPredefinedRoles`](clusters.md#role-policy);
- is a superuser (`pg_roles.rolsuper`);
- has `CREATEROLE`, `REPLICATION` or `BYPASSRLS` while the Cluster's
  `rolePolicy.allowedAttributes` does not allow that attribute;
- is not managed by a Role of the same Cluster (a role a DBA or a bootstrap
  Job created, or one restored from another Cluster), unless the Cluster lists
  it in [`spec.rolePolicy.allowedExistingRoles`](clusters.md#role-policy); or
- is itself a member, directly or indirectly, of any role above.

Memberships between roles managed by Roles of the same Cluster are allowed:
everyone who can write Roles for a Cluster is in the Cluster's namespace and
is treated as one trust domain (a Role writer can make their role a member
of another Role's role and read what it can read). Roles created outside pgop
are a different trust domain; only a Cluster editor can open them up.

Grant options (`inherit`, `set`) do not change the decision, and membership
chains are followed whatever their options: options can be changed later,
outside the Role. The rule is deliberately simple: anything this role could
become by `SET ROLE` (attributes such as `BYPASSRLS` and `CREATEROLE` apply to
the current role after `SET ROLE`) must stay within what the Cluster's policy
allows a Role writer to request directly.

A refused membership is not granted; the other memberships are still applied
and the Role reports `Available=False` with reason `MembershipNotAllowed`,
naming each refused role and why. If pgop granted the membership earlier
(it is in `status.managedMemberships`, for example because the Cluster's
policy allowed it then), it is **revoked immediately**, whatever
`revokeRemovedMemberships` says. A forbidden membership granted outside pgop
is reported but left alone.

The check runs on every reconcile of the Role, and Roles reconcile when their
Cluster's `rolePolicy` changes. A target role that a DBA changes later (made
superuser, or granted a forbidden role) is caught at the Role's next
reconcile, not immediately.

### Revoking removed memberships

pgop records the memberships it manages in `status.managedMemberships`.
When an entry is removed from `memberships` (or `memberOf`), pgop revokes that
membership. Memberships granted outside pgop (for example by a DBA with
`GRANT`) are never revoked, because they were never in
`status.managedMemberships`.

Things to know:

- **Upgrading:** existing Roles have no `status.managedMemberships` yet. The
  first reconcile after upgrading records the current spec without revoking
  anything, so a membership removed from the spec *before* the upgrade stays
  in place. Revoke it manually if needed.
- **Status loss:** if the status is lost (the Role is deleted and recreated,
  or restored from a backup without status), memberships removed in the
  meantime are not revoked.
- Listing a role that a DBA granted manually adopts it: it becomes managed and
  is revoked when later removed from the spec.

To keep the previous grant-only behavior, set `revokeRemovedMemberships:
false`. Memberships removed while it is `false` are dropped from
`status.managedMemberships` without being revoked, and are not revoked later
if the setting is turned back on.

!!! warning
    `revokeRemovedMemberships` is a transitional opt-out. It will be removed
    together with the deprecated `memberOf` field, after which removed
    memberships are always revoked.

### Deprecated `memberOf`

`spec.memberOf` is deprecated in favor of `spec.memberships` and will be
removed in a future API version. Each `memberOf` entry behaves like a
`memberships` entry with only `role` set. A role must not be listed in both
fields; the API server rejects such a Role. To migrate, move each entry:

```yaml
# before
memberOf:
  - app_read_role
# after
memberships:
  - role: app_read_role
```

Moving an entry from `memberOf` to `memberships` in a single update does not
revoke and re-grant it.

## Parameter Grants

`spec.parameterGrants` lets a role change configuration parameters that
normally only a superuser may set, with `GRANT SET ON PARAMETER` (PostgreSQL
15 or later):

```yaml
spec:
  parameterGrants:
    - parameter: log_statement      # SET is the default privilege
    - parameter: myapp.tenant_id    # custom parameters work too
      privileges: [SET]
      withGrantOption: true
```

- `parameter` is a parameter name: identifiers optionally separated by dots,
  at most 127 characters, case-insensitive (pgop lowercases it). Each parameter
  may appear once.
- `privileges` only accepts `SET` (the default). `ALTER SYSTEM` is
  deliberately not offered: it would let the role rewrite
  `postgresql.auto.conf`, which overrides the Cluster's `spec.parameters`.
- On a server older than PostgreSQL 15 the Role reports `Available=False` with
  reason `UnsupportedServerVersion`; nothing is granted.
- pgop records what it granted in `status.managedParameterGrants`. Removing a
  parameter (or turning `withGrantOption` off) revokes what pgop granted;
  privileges granted outside pgop are never revoked. A revoke of privileges
  granted with `withGrantOption` uses `CASCADE`, so grants the role passed on
  are revoked too.
- Only parameters whose `pg_settings.context` is `superuser` (the point of
  the feature) or `user`, and custom placeholders the server does not know,
  are granted. Other contexts are refused with `ParameterNotAllowed` (and a
  managed grant on them is revoked): `sighup`, `postmaster` and `internal`
  parameters cannot be changed in a session anyway, and `backend` /
  `superuser-backend` ones (such as `log_connections`) would be settable by
  the role at connection start.
- Some parameters can never be granted, because SET on them lets the role
  switch identity, load code or bypass safeguards: `role`,
  `session_authorization`, `session_preload_libraries`,
  `local_preload_libraries`, `shared_preload_libraries`,
  `dynamic_library_path`, `jit_provider`, `session_replication_role`,
  `lo_compat_privileges` (disables large-object permission checks) and the
  `pgaudit.*`, `set_user.*`, `anon.*` and `sepgsql.*` namespaces. The API
  server rejects them; a Role that still lists one (or an older object) gets
  `Available=False` with reason `ParameterNotAllowed`, the other grants are
  applied, and a managed grant on such a parameter is revoked.
- Granting SET on any other superuser-only parameter is a real privilege
  escalation for that role: for example SET on `log_statement` lets it turn
  statement logging off for its own sessions. If the role also owns a
  database, it can set the parameter as a default for **every** session in
  that database (`ALTER DATABASE ... SET log_statement = 'none'`), including
  other roles' and the operator's: the logging and auditing of that database
  is then in its hands. pgop pins the settings that could break its own
  sessions (`exit_on_error` among them, see
  [Databases: settings](databases.md#which-parameters-may-be-set)), but not
  logging parameters, whose server-wide value it does not override. Grant
  logging parameters only to roles that do not own databases, or accept that
  risk. Grant only what the role needs.

## Role Settings

`spec.settings` sets defaults for configuration parameters in this role's
sessions, like `ALTER ROLE <role> SET <name> TO <value>`.
`spec.databaseSettings` does the same for the role's sessions in one database
(`ALTER ROLE <role> IN DATABASE <db> SET ...`), which takes precedence over
both `settings` and the database's own [settings](databases.md#database-settings).

```yaml
spec:
  settings:
    statement_timeout: 30s
    idle_in_transaction_session_timeout: 60s
    # The YAML single quotes only quote the string; the value is
    # "$user", app (with the double quotes).
    search_path: '"$user", app'
    myapp.tenant: acme            # needs rolePolicy.allowedSettingPrefixes: [myapp]
  databaseSettings:
    - database: app_db            # PostgreSQL database name
      settings:
        work_mem: 64MB
```

- Keys and values follow the rules of
  [Database settings](databases.md#database-settings): parameter names
  (identifiers optionally separated by dots, at most 127 characters,
  case-insensitive), values sent as SQL string literals (at most 4096
  characters), and comma-separated lists for `search_path` and
  `temp_tablespaces` (an empty list is rejected). `settings` takes up to 256
  entries; `databaseSettings` up to 32 databases with 64 entries each.
- They apply to sessions that **log in** as the role after the change, not to
  sessions that switch to it with `SET ROLE`. `NOLOGIN` roles accept settings
  too, but they only take effect if the role later logs in.
- `databaseSettings[].database` is a raw PostgreSQL database name (not a
  Database resource name) and may be any database of the Cluster, including
  ones pgop does not manage: the setting only affects this role's sessions
  there. A database that does not exist yet does not fail the Role: it stays
  `Available` (a Database owned by this Role waits for that), the condition
  message lists the pending databases, and pgop applies the settings once the
  database appears (it watches Databases and retries every 30 seconds).
  Dropping a database removes the role's settings in it; pgop then forgets
  them.
- Settings are re-applied on every reconcile, which repairs drift. pgop
  records the names it set in `status.managedSettings` and
  `status.managedDatabaseSettings`, and resets (`ALTER ROLE ... RESET <name>`)
  any of them removed from the spec. Settings made outside pgop are never
  reset. Without that status (a re-created Role, a restore without status, or
  a Cluster re-created under the same name) removed settings are not reset.
- The same parameter policy as for Database settings applies (see
  [Which parameters may be set](databases.md#which-parameters-may-be-set)):
  only `user`-context parameters, and custom placeholders in namespaces the
  Cluster lists in [`rolePolicy.allowedSettingPrefixes`](clusters.md#role-policy),
  are set. A refused
  setting is skipped, the others are applied, and the Role reports
  `Available=False` with reason `SettingNotAllowed` naming each one; a setting
  pgop applied earlier that is no longer allowed is reset. The denylisted
  names (`role`, `session_authorization`, `*_preload_libraries`,
  `dynamic_library_path`, `jit_provider`, `session_replication_role`,
  `lo_compat_privileges`, `pgaudit.*`, `set_user.*`, `anon.*`, `sepgsql.*`)
  are rejected by the API server. A `superuser`-context parameter is refused
  even when the role holds a [parameter grant](#parameter-grants) for it; the
  role can still `SET` it in its own sessions.
- Nothing is applied to a role pgop does not own (`RoleNotManaged`,
  `DuplicateRoleName`, `ReservedName`) or whose attributes violate the
  Cluster's role policy (`RolePolicyViolation`); the settings ledger is kept
  until the Role is reconciled normally again. Reserved roles (`postgres`,
  `pgop_*`, `pg_*`) can never be targeted, so the operator's own sessions are
  never affected (they also pin their settings, see
  [Databases: settings](databases.md#which-parameters-may-be-set)).
- Settings are deleted with the role: `DROP ROLE` removes them.
- Known limitation: a database renamed outside pgop (`ALTER DATABASE ...
  RENAME`) keeps the role's settings (they follow the database, not its
  name), but pgop then sees the old name as missing and forgets them, so they
  are never reset. Remove them by hand (`ALTER ROLE ... IN DATABASE <new
  name> RESET ...`) or rename the entry in `databaseSettings`.
- `databaseSettings[].database` is limited to 63 bytes (PostgreSQL's
  identifier limit; non-ASCII characters take several bytes). Longer names
  are rejected by the API server; an older object that still has one gets
  `Available=False` naming the entry, and its other settings are applied.
- While a database is pending, the Role is retried after 30 seconds, then
  less and less often up to every 5 minutes (each retry re-applies the whole
  Role); creating the database (through a Database resource of the Cluster)
  wakes it up immediately.

!!! warning "Settings are not secret"
    Role settings are stored in `pg_db_role_setting` and shown in
    `pg_roles.rolconfig`, which every role on the server can read. Do not put
    passwords or tokens in them.

**Why this is safe to hand to Role writers.** A role-level default only
affects sessions of that role, so it cannot change other roles' sessions.
For parameters the server knows with context `user` it gives the role nothing
it could not do itself: any role may run `ALTER ROLE <itself> [IN DATABASE
...] SET` for such parameters. pgop runs the statement as a superuser, which
is why superuser-only parameters are refused, and why custom placeholders
need the Cluster's `allowedSettingPrefixes`: PostgreSQL does not let a role
store a placeholder for itself, and a superuser-stored placeholder applies
even if an extension later defines the parameter as superuser-only (for
example `plperl.on_plperl_init`, which runs code in the role's sessions). See
[Which parameters may be set](databases.md#which-parameters-may-be-set).

## Deletion

When a Role is deleted, pgop drops the PostgreSQL role recorded in
`status.roleName` (nothing is dropped when no role was recorded, for example
when pgop refused to take over an
[existing role](#ownership-of-the-postgresql-role)).
PostgreSQL refuses to drop a role that still holds privileges, so pgop first
revokes:

- the parameter privileges it granted (`status.managedParameterGrants`),
- every database-level privilege the role holds on any database of the
  cluster, and
- every schema privilege the role holds, in every database,

all with `CASCADE` (privileges the role passed on go with it, as with
`DROP OWNED`). A database that does not accept connections (its owner can run
`ALTER DATABASE ... WITH ALLOW_CONNECTIONS false`, which also locks out
superusers) or cannot be reached is skipped instead of holding the deletion
up; if privileges there still block `DROP ROLE`, the `RoleDropBlocked`
condition names the database. Databases whose `grants` list the role stop granting to it while
it is being deleted, so they do not undo this.

Anything else that depends on the role, such as objects it owns or privileges
on tables, is left alone. The drop then fails, and the Role reports
`Available=False` with reason `RoleDropBlocked` and PostgreSQL's list of
dependents. pgop retries every 30 seconds until you resolve them (for
example with `REASSIGN OWNED BY ... TO ...` and `DROP OWNED BY ...` in each
database), after which the finalizer is removed.

## Role Types

### Application User

```yaml
spec:
  clusterRef:
    name: my-cluster
  login: true
  connectionLimit: 50
```

### Read-Only Role

```yaml
spec:
  clusterRef:
    name: my-cluster
  login: false  # Group role, not a login
  inherit: true
```

### Admin Role

```yaml
spec:
  clusterRef:
    name: my-cluster
  login: true
  createDB: true
  createRole: true   # needs allowedAttributes: [createRole] on the Cluster
```

`createRole` is refused unless the Cluster's
[`rolePolicy.allowedAttributes`](clusters.md#role-policy) lists it. On
PostgreSQL 15 and older, `CREATEROLE` lets the role grant itself membership
in any non-superuser role, including `pg_execute_server_program`, so it is
close to superuser there; PostgreSQL 16 limits it to roles the role created.

## Security model

pgop runs every statement as a superuser on behalf of whoever can create or
edit Role, Database and Cluster resources. Kubernetes RBAC on those kinds
decides who can do what.

**The trust boundary is the namespace.** `clusterRef` only names a Cluster in
the resource's own namespace (the operator looks the Cluster up in the Role's
or Database's namespace; there is no namespace field), so every Role and
Database that can act on a Cluster lives in the Cluster's namespace. Nothing
in another namespace can reach it. Below, "Role writer" means anyone with
write access to Role resources in that namespace (likewise for Databases);
all of them form one trust domain for the Cluster.

| Who | Can | Cannot |
|-----|-----|--------|
| **Cluster editors** | Everything pgop offers, including widening `spec.rolePolicy`: privileged attributes, predefined roles, memberships in existing roles, untrusted extensions, custom setting namespaces, and **taking over existing roles and databases** (`adoptableRoles`, `adoptableDatabases`). Cluster editors are trusted with the whole server. | — |
| **Role writers** | Create non-superuser roles with `login`, `createDB`, `inherit`, `connectionLimit`, passwords, parameter grants (with a denylist), role settings (user-context parameters, and placeholders in namespaces the Cluster allows), and memberships in roles that pass the [membership policy](#membership-policy). Read the other Secrets of the namespace via `passwordSecretRef`. | Create superusers (the field no longer exists; roles are always `NOSUPERUSER`). Read pgop's own Secrets (the Cluster's superuser credentials, TLS keys) through `passwordSecretRef`. Get `createRole`, `replication` or `bypassRLS`, or membership in predefined `pg_*` roles, unless the Cluster allows it. Become a member of a superuser, `postgres`, `pgop_*` or the server-file roles, or of any role that leads to them. Take over, reset the password of, or drop an existing role pgop did not create for them, whatever its comment says, unless a Cluster editor allowlists it (`RoleNotManaged`); reuse another Role's PostgreSQL name (`DuplicateRoleName`). Join roles that no Role of the Cluster manages unless the Cluster lists them. Grant `SET` on parameters that are not `user`/`superuser` context or on the denylist. Set role defaults (`settings`, `databaseSettings`) other than `user`-context parameters or placeholders in namespaces the Cluster lists, or on any role but their own. Use the `pgop_` prefix, `pg_` prefix or `postgres` as role names. |
| **Database writers** | Everything inside *their* database: owner, schemas, grants, settings (user-context parameters, and placeholders in namespaces the Cluster allows), trusted extensions (see [Databases: security model](databases.md#security-model)). | Take over or drop a database pgop did not create for them (`DatabaseNotManaged`, `DuplicateDatabaseName`). Install untrusted extensions unless the Cluster lists them. Manage the `postgres`, `template0` or `template1` databases or system schemas (`pg_*`, `information_schema`). Change the operator's session settings (`search_path`, `role`, timeouts, read-only) with `ALTER DATABASE ... SET`. |

Because the policy lives on the **Cluster**, granting someone RBAC to create
Roles (or Databases) no longer makes them superuser-equivalent: widening what
Roles may get requires RBAC to edit the Cluster. Give Cluster edit rights only
to people you would give the superuser password.

Known limits of this model: memberships among Roles of the same Cluster are
deliberately not restricted (any Role writer can join any non-privileged role
another Role manages); `passwordSecretRef` reads
Secrets of the namespace that pgop does not manage; and predefined roles a Cluster allows act on
every database of the Cluster (for example `pg_read_all_data`).

## Upgrade / breaking changes

This release changes the Role API (v1alpha1, no compatibility shims):

- **`spec.superuser` is removed.** The API server rejects it (or, with lax
  field validation, drops it). Every role pgop manages is `NOSUPERUSER`; a role
  that an older pgop made superuser is altered to `NOSUPERUSER` on its next
  reconcile. Use the Cluster credentials Secret for superuser work.
- **`createRole`, `replication` and `bypassRLS` need the Cluster's
  `spec.rolePolicy.allowedAttributes`.** Without it the Role reports
  `RolePolicyViolation` and the role is altered to `NOCREATEROLE
  NOREPLICATION NOBYPASSRLS` (none of them, even those that would be
  allowed). Add the attributes to the Cluster to restore them.
- **Memberships are checked** (see [Membership policy](#membership-policy)).
  Membership in a predefined `pg_*` role (such as `pg_monitor` or
  `pg_read_all_data`) needs the Cluster's `rolePolicy.allowedPredefinedRoles`;
  memberships in superuser, `postgres`, `pgop_*` and the server-file roles are
  no longer possible. Forbidden memberships pgop granted before are
  **revoked** on the next reconcile.
- **Names starting with `pgop_` are reserved** for `roleName` (previously only
  `pgop_operator` and `pgop_replicator`), and a Role named `postgres` must set
  `roleName`.
- **Existing roles** that are superusers, or members of forbidden roles, are no
  longer taken over, and deleting a Role drops only the role recorded in
  `status.roleName`.
- **No more taking over existing roles:** an existing role that the Role's
  `status.roleName` does not record is left alone (`RoleNotManaged`) unless a
  Cluster editor lists it in `spec.rolePolicy.adoptableRoles`. Roles created by
  an earlier pgop and recorded in status keep working and get a signed
  ownership marker (`COMMENT ON ROLE`). Two Roles with the same PostgreSQL
  name: the later one reports `DuplicateRoleName`.
- **Memberships in roles no Role of the Cluster manages** need the Cluster's
  `rolePolicy.allowedExistingRoles`; pgop-granted ones are revoked.
- **`parameterGrants`** only accept `user` and `superuser` context parameters
  (and custom placeholders); `lo_compat_privileges` joins the denylist.
- **`passwordSecretRef` cannot name a Secret pgop manages** (such as
  `<cluster>-credentials`) or a Cluster's TLS Secret (reason
  `RolePolicyViolation`); see
  [who can read a passwordSecretRef Secret](#security-who-can-read-a-passwordsecretref-secret).

## Password Source

The credentials Secret above is always the place apps read the password from,
whichever source it comes from:

1. **`passwordSecretRef` set**: the value of `data[key]` of the named Secret in
   the Role's namespace. The operator watches that Secret: a change is applied
   to PostgreSQL and copied into the credentials Secret (and the Database
   credentials Secrets). The operator never modifies or owns the referenced
   Secret. If the Secret or key is missing (or empty), the Role reports
   `Available=False` with reason `PasswordSecretNotFound` and the operator does
   **not** fall back to a generated password. A value that is not valid UTF-8,
   contains a NUL byte, or is already a PostgreSQL password hash (starts with
   `SCRAM-SHA-256$`, or is `md5` followed by 32 hex digits) is rejected with
   reason `PasswordSecretInvalid`: put the plaintext password in the Secret.
2. **Otherwise**: the operator generates a random password when the Role is
   first created and keeps it, unless [rotation](#password-rotation) replaces
   it.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: app-user-password
stringData:
  password: s3cr3t-from-vault
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: app-user
spec:
  clusterRef:
    name: my-cluster
  passwordSecretRef:
    name: app-user-password
    key: password
```

When `passwordSecretRef` is removed from a Role, the current password is kept
(it stays in the credentials Secret); the operator does not generate a new one
until a rotation is due or requested.

### How the password reaches PostgreSQL

- The operator never sends the plaintext password to PostgreSQL. It computes a
  SCRAM-SHA-256 verifier (random salt, 4096 iterations, SASLprep like
  PostgreSQL) and sends `PASSWORD 'SCRAM-SHA-256$...'`, which PostgreSQL stores
  as-is whatever `password_encryption` is set to. The plaintext therefore
  never appears in server logs. Clients authenticate with `scram-sha-256` (or
  `md5`/`password` `pg_hba.conf` methods, which also accept SCRAM verifiers).

!!! warning "Server logs still contain the verifier"
    With `log_statement=ddl` (or `all`), and for a failing statement with the
    default `log_min_error_statement=error`, the `CREATE ROLE`/`ALTER ROLE`
    statement is logged **with the SCRAM verifier**. A verifier is not the
    password, but it is sensitive: it can be attacked offline (only 4096
    PBKDF2 iterations, PostgreSQL's default), and its ServerKey lets whoever
    holds it impersonate the server to clients. Treat PostgreSQL logs that may
    contain role DDL as secret, like `pg_authid`.
- `ALTER ROLE ... PASSWORD` is only sent when the password changed. The
  operator tells by a salted SHA-256 fingerprint in the
  `pgop.ruck.io/password-fingerprint` annotation of the credentials Secret,
  next to the password itself, so it reveals nothing to anyone who cannot
  already read the password. (Earlier versions kept it in
  `status.passwordHash`, readable by anyone who can read the Role; that field
  is deprecated and cleared.) Before sending a password because the cached
  Secret's fingerprint does not match, the operator re-reads the Secret from
  the API server, so a cache lagging behind a rotation can never roll the
  password back.
- Errors from `CREATE ROLE`/`ALTER ROLE` are redacted before they reach
  conditions, Events or logs. The operator never logs passwords.

!!! note
    Editing `password` in the operator-managed credentials Secret directly sets
    that password in PostgreSQL on the next reconcile, unless
    `passwordSecretRef` is set, in which case the referenced value wins. An
    edited password gets the same checks as a `passwordSecretRef` value (UTF-8,
    no NUL byte, not pre-hashed); one that fails them is not set and the Role
    reports `Available=False` with reason `PasswordSecretInvalid` until the
    Secret is fixed.

### Security: who can read a `passwordSecretRef` Secret

The operator reads the Secret in the Role's namespace that a Role names in
`passwordSecretRef`, and copies the named key into the Role's credentials
Secret (and the Database credentials Secrets), with the operator's own
permissions. So **anyone who can create or update Roles in a namespace can
read the Secrets of that namespace**, including ones their own RBAC does not
let them read, such as other apps' Secrets: they point a Role at the Secret and
read the resulting credentials Secret (or log in with it).

pgop refuses (reason `RolePolicyViolation`) Secrets it manages itself, so the
operator's own credentials cannot be read this way: Secrets labeled
`app.kubernetes.io/managed-by: pgop` (such as `<cluster>-credentials` with the
operator's superuser password, other Roles' credentials, TLS and backup
Secrets), Secrets owned by a `pgop.ruck.io` resource, and the Secret a Cluster
of the namespace uses as `spec.tls.secretName` (the server's private key).
Everything else in the namespace is readable.

Treat the permission to create/update `roles.pgop.ruck.io` as equivalent to
`get` on the other Secrets of the namespace:

- Grant `create`/`update`/`patch` on `roles.pgop.ruck.io` only to subjects that
  may already read the namespace's Secrets (typically namespace admins and the
  CD system). The aggregated `edit` ClusterRole should not be used to hand
  out Role write access to users who must not see the namespace's Secrets.
- Keep Secrets that such users must not read in a namespace where they
  cannot create Roles; a Role can only reference Secrets in its own namespace.
- Audit `passwordSecretRef` values (e.g. with an admission policy that
  restricts which Secret names a Role may reference) where Role authors are
  less trusted than Secret readers.

### Upgrade notes

- **`passwordSecretRef` is now honored** (v0.10.0, issue #25). Before,
  `spec.passwordSecretRef` was accepted but ignored. Roles that
  already set it switch to the referenced password **immediately** after the
  upgrade: PostgreSQL and the credentials Secret get that password, and apps
  using the old generated password fail on their next connection. If the
  referenced Secret or key is missing, the Role goes `Available=False`
  (`PasswordSecretNotFound`) and is not ready until it exists; the existing
  password keeps working meanwhile. Check your Roles before upgrading:
  `kubectl get roles.pgop.ruck.io -A -o jsonpath='{range .items[?(@.spec.passwordSecretRef)]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'`
- **Password fingerprint moved.** `status.passwordHash` is deprecated: the
  first reconcile after upgrading moves the fingerprint into the
  `pgop.ruck.io/password-fingerprint` annotation of the credentials Secret and
  clears the status field. The password is not re-sent for this.

## Password Rotation

`spec.passwordRotation` rotates the operator-generated password on a schedule:

```yaml
spec:
  clusterRef:
    name: my-cluster
  passwordRotation:
    every: 720h   # 30 days; Go duration, minimum 1h
```

When `status.passwordRotatedAt + every` has passed, the operator generates a
new password, sets it in PostgreSQL, then writes it to the credentials Secret
and the Database credentials Secrets, records `status.passwordRotatedAt` and
emits a `PasswordRotated` Event. If the Secret write fails, the next reconcile
rotates again, so apps are never handed a password PostgreSQL does not know.
For Roles created before rotation was enabled, the schedule starts when the
operator first sees `passwordRotation`.

`passwordRotation` and `passwordSecretRef` are mutually exclusive (the API
rejects a Role with both). With `passwordSecretRef`, rotate the referenced
Secret yourself (e.g. with your secret manager); the operator follows it.

### Rotating on demand

Set the `pgop.ruck.io/rotate-password` annotation to a new value (any string,
for example a timestamp or an incident ID) to rotate immediately, with or
without `passwordRotation`:

```bash
kubectl annotate role.pgop.ruck.io app-user --overwrite \
  pgop.ruck.io/rotate-password="$(date +%s)"
```

The operator acts on each value once and records it in
`status.passwordRotationRequest`. On a Role with `passwordSecretRef` the
annotation does not generate a password; it sets the referenced password in
PostgreSQL again (useful after a physical restore brought back an older
password).

### What a rotation means for apps

PostgreSQL has a single password per role, so there is no overlap period:

- **New connections** must use the new password as soon as it is set.
- **Existing sessions** stay connected; PostgreSQL only checks the password
  when a connection is opened.
- Apps must re-read the Secret before reconnecting. Mounted Secret volumes are
  refreshed by the kubelet (after a short delay), while environment variables
  need a restart (for example with a tool like
  [Reloader](https://github.com/stakater/Reloader)). Connection pools should
  pick up the new password before opening new connections.

Dual-password overlap (two login roles alternating) is not supported.

The Cluster's own operator credentials (`<cluster>-credentials`) are not
rotated by this feature.
