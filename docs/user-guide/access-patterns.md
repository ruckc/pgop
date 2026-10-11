# Users and Access Patterns

This page puts [Clusters](clusters.md), [Roles](roles.md) and
[Databases](databases.md) together into complete setups: who owns the
database, who runs the migrations, how the application, read-only users,
reporting tools and monitoring get exactly the access they need, and what
happens when access is changed or a user is deleted. The manifests are in the
repository's [`examples/`](https://github.com/ruckc/pgop/tree/main/examples)
directory, and every manifest on this page is validated against the CRDs by
`make test-manifests`.

## Building blocks

| You want | Use | Notes |
|----------|-----|-------|
| A user that logs in | Role with `login: true` (the default) | Gets a password and the Secret `<cluster>-<role>-credentials` |
| A group of privileges | Role with `login: false` | No password, no Secret. Grant to it, make users members |
| A PostgreSQL name with underscores | Role `spec.roleName`, Database `spec.databaseName` | Immutable. Kubernetes references (`owner`, Secret names) keep using `metadata.name` |
| Membership in a group | Role `spec.memberships` | Raw PostgreSQL role names. Removed entries are revoked |
| The password from your secret manager | Role `spec.passwordSecretRef` | pgop copies it into the credentials Secret and follows changes |
| Regular password changes | Role `spec.passwordRotation.every` or the `pgop.ruck.io/rotate-password` annotation | `passwordRotation` cannot be combined with `passwordSecretRef`; the annotation works with both (with `passwordSecretRef` it sets the referenced password again) |
| Per-user session defaults | Role `spec.settings`, `spec.databaseSettings` | `ALTER ROLE ... [IN DATABASE ...] SET` |
| Who owns a database | Database `spec.owner` (a Role resource name) | The owner has full DDL; its credentials go to `<database>-<owner>-credentials` |
| Who may connect | Database `spec.grants` (`CONNECT`) and `publicPrivileges.connect: false` | PostgreSQL lets every role connect by default |
| Access to a schema | `schemas[].grants` (`USAGE`, `CREATE`) | |
| Access to existing tables, sequences, functions, types | `schemas[].objectGrants` | `"*"` for every object of a kind |
| Access to tables created later | `schemas[].defaultPrivileges` | For the role that creates them (the migrator) |
| Privileges a Role writer must not get alone | Cluster `spec.rolePolicy` | Privileged attributes, `pg_*` roles, outside roles, untrusted extensions, custom settings, adoption |

Two kinds of names appear in the specs: **Role resource names** (Kubernetes,
with dashes) in `Database.spec.owner`, and **PostgreSQL role names**
everywhere else (`memberships[].role`, `grants[].role`, `schemas[].owner`,
`objectGrants[].role`, `defaultPrivileges[].forRole`). With `roleName` set
they differ: `owner: shop-owner` but `forRole: shop_owner`.

## One application role

The smallest useful setup: one login role owns the database, runs its
migrations and serves the application. The application uses the Database's
connection Secret, `myapp-app-user-credentials`, which has the `database` key.

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: app-user
spec:
  clusterRef:
    name: my-cluster
  connectionLimit: 20
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: myapp
spec:
  clusterRef:
    name: my-cluster
  owner: app-user
  schemas:
    - name: app             # owned by app-user (the database owner)
```

This is fine for small services. Its drawback: the application's own
credentials can drop every table. The next pattern separates the duties.

## Owner, application, read-only, reporting and monitoring roles

The [`examples/app-access`](https://github.com/ruckc/pgop/tree/main/examples/app-access)
setup for an application called *shop*:

| PostgreSQL role | Kind | Purpose | Gets its access from |
|-----------------|------|---------|----------------------|
| `shop_owner` | login | Owns the database and schema `app`, runs the migrations | Ownership |
| `shop_rw` | group | Read/write on `app`'s tables and sequences | Database grants, object grants, default privileges |
| `shop_ro` | group | Read-only on `app`'s tables | Database grants, object grants, default privileges |
| `shop_app` | login | The application | Membership in `shop_rw` |
| `shop_reporting` | login | A BI tool, on the read replica | Membership in `shop_ro` |
| `shop_monitoring` | login | A metrics exporter | Membership in `pg_monitor` |

### The Cluster

The Cluster editor decides what the Roles and the Database may obtain. Here:
membership in `pg_monitor`, custom settings in the `shop.*` namespace, and
the untrusted extension `pg_stat_statements`.

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: shop-db
spec:
  image: postgres:18
  replicas: 2                     # primary + 1 standby behind shop-db-ro
  storage:
    size: 20Gi
  tls: {}                         # self-managed CA; Secrets switch to verify-full
  parameters:
    shared_preload_libraries: pg_stat_statements
    max_connections: "200"
  rolePolicy:
    allowedPredefinedRoles: [pg_monitor]
    allowedSettingPrefixes: [shop]
    allowedExtensions: [pg_stat_statements]   # not a trusted extension
```

Without these entries the monitoring Role reports `MembershipNotAllowed`
and the Database reports `SettingNotAllowed` for `shop.region` and
`ExtensionNotAllowed` for `pg_stat_statements`; everything else still works. See [Clusters: Role Policy](clusters.md#role-policy).

### The Roles

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: shop-owner
spec:
  clusterRef:
    name: shop-db
  roleName: shop_owner
  connectionLimit: 5
  passwordRotation:
    every: 2160h                  # 90 days
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: shop-rw
spec:
  clusterRef:
    name: shop-db
  roleName: shop_rw
  login: false                    # group role
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: shop-ro
spec:
  clusterRef:
    name: shop-db
  roleName: shop_ro
  login: false
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: shop-app
spec:
  clusterRef:
    name: shop-db
  roleName: shop_app
  connectionLimit: 50
  memberships:
    - role: shop_rw
  settings:
    statement_timeout: 15s
    idle_in_transaction_session_timeout: 60s
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: shop-reporting
spec:
  clusterRef:
    name: shop-db
  roleName: shop_reporting
  connectionLimit: 10
  passwordSecretRef:              # a Secret you manage, in the same namespace
    name: shop-reporting-password
    key: password
  memberships:
    - role: shop_ro
  settings:
    default_transaction_read_only: "on"
  databaseSettings:
    - database: shop
      settings:
        statement_timeout: 5min
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: shop-monitoring
spec:
  clusterRef:
    name: shop-db
  roleName: shop_monitoring
  connectionLimit: 3
  memberships:
    - role: pg_monitor
```

- Memberships between roles managed by Roles of the same Cluster are always
  allowed (the namespace is one trust domain); `pg_monitor` needs the
  Cluster's policy. See [Roles: membership policy](roles.md#membership-policy).
- `inherit` is on by default, so `shop_app` uses `shop_rw`'s privileges
  without `SET ROLE`.
- `settings` apply to new sessions that log in as the role.
  `default_transaction_read_only` makes the reporting user's transactions
  read-only by default (a session can still turn it off: real read-only
  access comes from the grants). `databaseSettings` override `settings` in
  one database.

### The Database

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: shop
spec:
  clusterRef:
    name: shop-db
  owner: shop-owner               # Role resource name
  publicPrivileges:
    connect: false                # only roles with a CONNECT grant (and the owner)
    temporary: false
  grants:
    - role: shop_rw
      privileges: [CONNECT, TEMPORARY]
    - role: shop_ro
      privileges: [CONNECT]
    - role: shop_monitoring       # per-database statistics need a connection
      privileges: [CONNECT]
  settings:
    search_path: '"$user", app'
    shop.region: eu-west-1
  extensions:
    - name: pg_trgm
    - name: citext
    - name: pg_stat_statements    # untrusted: an operator-owned schema
      schema: monitoring
      grants:
        - role: shop_monitoring
          schema: [USAGE]
  schemas:
    - name: app
      owner: shop_owner
      grants:
        - role: shop_rw
          privileges: [USAGE]
        - role: shop_ro
          privileges: [USAGE]
      objectGrants:               # tables and sequences that exist now
        - role: shop_rw
          kind: table
          objects: ["*"]
          privileges: [SELECT, INSERT, UPDATE, DELETE]
        - role: shop_rw
          kind: sequence
          objects: ["*"]
          privileges: [USAGE, SELECT]
        - role: shop_ro
          kind: table
          objects: ["*"]
          privileges: [SELECT]
      defaultPrivileges:          # tables and sequences shop_owner creates later
        - forRole: shop_owner
          role: shop_rw
          kind: table
          privileges: [SELECT, INSERT, UPDATE, DELETE]
        - forRole: shop_owner
          role: shop_rw
          kind: sequence
          privileges: [USAGE, SELECT]
        - forRole: shop_owner
          role: shop_ro
          kind: table
          privileges: [SELECT]
```

What each piece does:

- **`owner`**: the database is created `OWNER shop_owner`, the schema `app`
  `AUTHORIZATION shop_owner`. The owner has full DDL there. Because the owner
  can log in, pgop writes the connection Secret `shop-shop-owner-credentials`
  (with `database: shop`) for the migration Job.
- **`publicPrivileges`** revokes the `CONNECT` and `TEMPORARY` every role has
  on a new database. `shop_reporting` and `shop_app` can still connect through
  their groups' `CONNECT` grants, and `shop_monitoring` through its own. A
  monitoring role needs `CONNECT` on every database it should report on:
  some of `pg_monitor`'s views are cluster-wide (`pg_stat_database`,
  `pg_stat_activity`, readable from the `postgres` database), but
  `pg_stat_user_tables`, the `pg_statio_*` views and `pg_stat_statements`
  only show the database the session is connected to. See
  [Default PUBLIC privileges](databases.md#default-public-privileges).
- **Schema `grants`**: `USAGE` lets the groups see the objects in `app`; it
  grants nothing on the objects themselves.
- **`objectGrants`** grant on the tables and sequences that exist when the
  Database reconciles. `"*"` is evaluated on every reconcile, so tables a
  migration creates are picked up at the next reconcile.
- **`defaultPrivileges`** make every table and sequence `shop_owner` creates
  in `app` from now on carry the grants immediately, without waiting for a
  reconcile. Together with `objectGrants` this covers both existing and future
  objects. Default privileges only apply to objects created by `forRole`: if
  migrations run as another role, name that role.
- **`settings`**: `search_path` for every session in the database (unless a
  role setting overrides it); `shop.region` is an application setting that
  the Cluster's `allowedSettingPrefixes` permits.
- **`extensions`**: `pg_trgm` and `citext` are trusted extensions, installed
  into `public`. `pg_stat_statements` is not trusted: the Cluster allows it,
  and it goes into the schema `monitoring`, which pgop creates owned by the
  operator (untrusted extensions are refused in schemas other roles own or
  can write to). Its library is loaded by the Cluster's
  `shared_preload_libraries`, and `CREATE EXTENSION` makes its view available
  in this database only; the exporter connects to `shop` to read it. The
  extension grants `SELECT` on the view to `PUBLIC` itself;
  [`extensions[].grants`](databases.md#grants-on-extension-objects) adds
  `USAGE` on its schema.

pgop only grants on objects owned by roles of the Cluster's Roles (here
`shop_owner`), or by the operator with stricter rules: see [Which objects pgop grants on](databases.md#which-objects-pgop-grants-on).
It records what it granted and revokes exactly that when an entry is removed
(see [What pgop tracks](databases.md#what-pgop-tracks)).

### Connecting the workloads

| Workload | Secret | Host | Database |
|----------|--------|------|----------|
| Migration Job | `shop-shop-owner-credentials` (Database Secret) | `shop-db` (primary) | from the Secret |
| Application | `shop-db-shop-app-credentials` (Role Secret) | `shop-db` (primary) | `shop` (set `PGDATABASE`) |
| Reporting | `shop-db-shop-reporting-credentials` | `shop-db-ro` (standbys) | `shop` |
| Monitoring | `shop-db-shop-monitoring-credentials` | `shop-db` | `shop` (and `postgres` for the cluster-wide views) |

A Role's credentials Secret has `username`, `password`, `host`, `port`,
`sslmode`, `uri` and, while TLS is active, `ca.crt`. It has **no `database`
key**: only its `uri` names a database, `postgres`. Only the owner's
credentials end up in a Database Secret. So for the other users, take the
individual keys and set the database yourself (`PGDATABASE=shop`, or
`PGDATABASE=postgres` for what the `uri` would have reached):

<!-- pgop-validate: skip (container spec excerpt) -->
```yaml
env:
  - name: PGHOST
    value: shop-db-ro.shop.svc.cluster.local   # reporting: the read-only Service
  - name: PGDATABASE
    value: shop
  - name: PGUSER
    valueFrom: { secretKeyRef: { name: shop-db-shop-reporting-credentials, key: username } }
  - name: PGPASSWORD
    valueFrom: { secretKeyRef: { name: shop-db-shop-reporting-credentials, key: password } }
  - name: PGSSLMODE
    valueFrom: { secretKeyRef: { name: shop-db-shop-reporting-credentials, key: sslmode } }
  - name: PGSSLROOTCERT
    value: /etc/pgop-ca/ca.crt                 # mount ca.crt from the same Secret
```

Roles and passwords are part of the data that streams to the standbys, so the
same credentials work on `shop-db-ro`. The self-managed CA's certificate
also names the `-ro` Service. Standbys may lag behind the primary and reject
writes; see [Replication](replication.md).

### Migrations and `"*"`

1. The migration Job connects as `shop_owner` and creates tables in `app`.
2. The default privileges apply at `CREATE TABLE`: `shop_rw` and `shop_ro`
   can use the table immediately.
3. At the Database's next reconcile, `objectGrants` with `"*"` see the table
   too and find nothing missing.

Default privileges cover objects created **after** they were set; for
objects that existed before (or were created by another role), `objectGrants`
does the work at the next reconcile. Reconciles are event-driven (a change
of the Database or of a Role it names); otherwise the controller's periodic
resync only comes about every 10 hours (the controller-runtime default). So
after a migration, trigger one yourself:
`kubectl annotate database shop pgop.ruck.io/reconcile="$(date +%s)" --overwrite`
(for example as the last step of the migration Job).

### Checking the result

Connect as the operator (`shop-db-credentials`) or the owner and look at what
pgop set up:

```sql
\du shop_*              -- attributes, memberships ("Member of")
\l shop                 -- database ACL: shop_rw=Tc/shop_owner, shop_ro=c/shop_owner
\dn+ app                -- schema ACL
\dp app.*               -- table and sequence ACLs
\ddp app                -- default privileges for shop_owner
SELECT rolname, rolconfig FROM pg_roles WHERE rolname LIKE 'shop_%';   -- role settings
SELECT has_table_privilege('shop_ro', 'app.orders', 'INSERT');        -- false
```

The resources report the same in their status: `Role.status.managedMemberships`,
`managedSettings`, and `Database.status.managedGrants`,
`managedSchemaGrants`, `managedObjectGrants`, `managedDefaultPrivileges` and
`objectGrants` (counts of granted and skipped objects).

## NOLOGIN owner and a migrator that uses SET ROLE

When the owner's password should never exist, the owner is a group role and a
migrator switches to it with `SET ROLE` (from
[`examples/set-role-migrator`](https://github.com/ruckc/pgop/tree/main/examples/set-role-migrator)):

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: inventory-owner
spec:
  clusterRef:
    name: shop-db
  roleName: inventory_owner
  login: false
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: inventory-migrator
spec:
  clusterRef:
    name: shop-db
  roleName: inventory_migrator
  connectionLimit: 2
  memberships:
    - role: inventory_owner
      inherit: false              # PostgreSQL 16+: no owner privileges until SET ROLE
      set: true
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: inventory-app
spec:
  clusterRef:
    name: shop-db
  roleName: inventory_app
  connectionLimit: 30
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: inventory
spec:
  clusterRef:
    name: shop-db
  owner: inventory-owner          # NOLOGIN: no Database Secret
  publicPrivileges:
    connect: false
    temporary: false
  grants:
    - role: inventory_migrator
      privileges: [CONNECT]
    - role: inventory_app
      privileges: [CONNECT]
  schemas:
    - name: app
      grants:
        - role: inventory_app
          privileges: [USAGE]
      objectGrants:
        - role: inventory_app
          kind: table
          objects: ["*"]
          privileges: [SELECT, INSERT, UPDATE, DELETE]
        - role: inventory_app
          kind: sequence
          objects: ["*"]
          privileges: [USAGE, SELECT]
      defaultPrivileges:
        - forRole: inventory_owner
          role: inventory_app
          kind: table
          privileges: [SELECT, INSERT, UPDATE, DELETE]
        - forRole: inventory_owner
          role: inventory_app
          kind: sequence
          privileges: [USAGE, SELECT]
```

- The migration tool starts with `SET ROLE inventory_owner;`, so every object
  is owned by `inventory_owner`, and `defaultPrivileges` name it as
  `forRole`. A migrator that forgets the `SET ROLE` creates objects it owns
  itself, which `inventory_app` cannot use (and which block deleting the
  migrator later).
- The Database has no connection Secret (its owner cannot log in); the
  migrator and the application use their Role Secrets with
  `PGDATABASE=inventory`.
- Rotating or deleting the migrator never touches the objects.
- On PostgreSQL 15 and older, leave out `inherit` and `set`: a plain
  membership inherits the owner's privileges and allows `SET ROLE`.

## Passwords

Every login role has exactly one current password, in its credentials Secret:

- **Generated** (default): created once, kept until rotated.
- **From your Secret**: `passwordSecretRef` names a key in a Secret of the
  same namespace (plaintext, not a hash). pgop sets it in PostgreSQL, copies
  it into the credentials Secret and follows changes; it never writes to your
  Secret. Secrets pgop manages (such as `<cluster>-credentials`) are refused.
  Anyone who can write Roles can read the namespace's other Secrets this way:
  see [who can read a passwordSecretRef Secret](roles.md#security-who-can-read-a-passwordsecretref-secret).
- **Rotated**: `passwordRotation.every` (minimum `1h`) or the
  `pgop.ruck.io/rotate-password` annotation. New connections need the new
  password at once; open sessions stay connected; the owner's Database
  Secrets follow. There is no overlap period: make applications re-read the
  Secret (a mounted volume, or a restart).

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: shop-reporting-password
type: Opaque
stringData:
  password: change-me-from-your-secret-manager
```

```bash
kubectl annotate role.pgop.ruck.io shop-app --overwrite \
  pgop.ruck.io/rotate-password="$(date +%s)"
```

See [Roles: Password Source](roles.md#password-source) and
[Password Rotation](roles.md#password-rotation).

## Roles created outside pgop

A DBA's role, or one a bootstrap Job created, is not managed by any Role. By
default Roles cannot join it and Databases cannot grant to it
(`MembershipNotAllowed`, `GranteeNotAllowed`). A Cluster editor opens it up:

<!-- pgop-validate: kind=Cluster -->
```yaml
spec:
  rolePolicy:
    allowedExistingRoles: [dba_readonly]   # may be joined and granted to
```

To have pgop **take over** an existing role or database (set its attributes
and password, drop it on deletion), the Cluster editor lists it in
`adoptableRoles` / `adoptableDatabases` instead, as in
[`examples/adopt-existing`](https://github.com/ruckc/pgop/tree/main/examples/adopt-existing):

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: legacy-db
spec:
  image: postgres:17
  storage:
    size: 50Gi
  rolePolicy:
    adoptableRoles: [legacy_app]
    adoptableDatabases: [legacy]
    allowedExistingRoles: [dba_readonly]
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: legacy-app
spec:
  clusterRef:
    name: legacy-db
  roleName: legacy_app
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Database
metadata:
  name: legacy
spec:
  clusterRef:
    name: legacy-db
  databaseName: legacy
  owner: legacy-app               # declare the owner, see below
  grants:
    - role: dba_readonly
      privileges: [CONNECT]
```

- Adopting a login role gives it a new generated password unless the Role
  sets `passwordSecretRef` (to keep the password the application already
  uses). Superusers and roles that reach forbidden roles are never adopted.
- An adopted database's existing schemas are only managed (re-owned, granted
  on) when they are owned by its declared owner, the schema's declared
  `owner` (a non-superuser) or `pg_database_owner`. **Set `owner` on adopted
  Databases**: without it the database keeps its previous owner and its
  schemas report `SchemaNotManaged`. See
  [Which schemas a Database manages](databases.md#which-schemas-a-database-manages).
- A Role or Database re-created without its status (from Git after a restore,
  or after the Cluster was re-created) needs the same allowlist entry once to
  find its object again.

See [Roles: ownership](roles.md#ownership-of-the-postgresql-role) and
[Databases: ownership](databases.md#ownership-of-the-postgresql-database).

## Changing and removing access

pgop revokes what it granted and nothing else:

| Change | Effect |
|--------|--------|
| Remove an entry from `memberships` | `REVOKE <group> FROM <role>` (if pgop granted it) |
| Remove a role or privilege from `grants`, `schemas[].grants` | The privilege pgop added is revoked; privileges the grantee already held are kept |
| Remove an `objectGrants` entry, or narrow `objects` | Revoked on the objects no entry selects any more |
| Remove a `defaultPrivileges` entry | `ALTER DEFAULT PRIVILEGES ... REVOKE`; objects created meanwhile keep their grants (revoke them with `objectGrants` or by hand) |
| Remove a `settings` key | `ALTER ROLE/DATABASE ... RESET` |
| Narrow the Cluster's `rolePolicy` | Attributes are removed, memberships and grants pgop made that the policy no longer allows are revoked |

### Deleting a user

Deleting a Role drops its PostgreSQL role. First pgop revokes, in every
database of the Cluster, the role's database and schema privileges, its
privileges on extension objects, the object grants and default privileges the
Databases recorded for it, and the parameter privileges it was granted.
Databases stop granting to a Role that is being deleted. The role's
credentials Secret goes with the Role.

`DROP ROLE` still fails while the role **owns** something (a database,
schemas, tables) or holds privileges pgop did not grant. The Role then
reports `Available=False` with reason `RoleDropBlocked`, lists PostgreSQL's
dependents and retries every 30 seconds. Resolve it in each database the
message names, as the operator (`<cluster>-credentials`):

```sql
REASSIGN OWNED BY shop_app TO shop_owner;   -- hand its objects to the owner
DROP OWNED BY shop_app;                     -- drop what is left, revoke its privileges
```

To tear an application down, first stop its workloads (and anything else
connected to the database): pgop runs a plain `DROP DATABASE`, without
`WITH (FORCE)`, which PostgreSQL refuses while sessions are connected; the
Database keeps retrying until they are gone. Then delete the Database before
(or together with) its owner Role: dropping the database removes everything in it, after which
the owner's `DROP ROLE` succeeds (until then the owner Role reports
`RoleDropBlocked` and retries). Group and login roles that own nothing can
be deleted in any order.

## Troubleshooting

| Condition reason | On | Meaning and fix |
|------------------|----|-----------------|
| `RolePolicyViolation` | Role | A privileged attribute (`createRole`, `replication`, `bypassRLS`) the Cluster does not allow, an existing role pgop must not take over, or a pgop-managed Secret in `passwordSecretRef`. Widen `rolePolicy` or change the Role |
| `MembershipNotAllowed` | Role | A membership the [membership policy](roles.md#membership-policy) refuses: a `pg_*` role not in `allowedPredefinedRoles`, a role no Role manages and not in `allowedExistingRoles`, a superuser |
| `RoleNotManaged` / `DatabaseNotManaged` | Role / Database | The object exists but pgop did not create it: list it in `adoptableRoles` / `adoptableDatabases` |
| `DuplicateRoleName` / `DuplicateDatabaseName` | Role / Database | Another resource already manages that PostgreSQL name |
| `PasswordSecretNotFound` / `PasswordSecretInvalid` | Role | The `passwordSecretRef` Secret or key is missing, or the value is not a plaintext password |
| `SettingNotAllowed` | Role / Database | Not a `user`-context parameter, or a custom namespace the Cluster's `allowedSettingPrefixes` does not list |
| `GranteeNotAllowed` | Database | A grantee that is not `PUBLIC`, a role of the Cluster's Roles, or in `allowedExistingRoles` |
| `SchemaNotManaged` | Database | An existing schema the Database neither created nor owns; set the Database's (or the schema's) `owner` |
| `ObjectGrantSkipped` / `ObjectNotFound` | Database | A named object is not granted on (owner outside the trust domain, extension member, ...) or does not exist yet |
| `DefaultPrivilegeNotAllowed` | Database | `forRole` is not a non-superuser role of the Cluster's Roles |
| `TooManyGrants` | Database / Role | The status ledger is full: grant to a group role instead of many roles or objects |
| `RoleDropBlocked` | Role | The deleted role still owns objects or holds privileges pgop did not grant: see [Deleting a user](#deleting-a-user) |

All reasons are listed in the [API reference](../reference/api.md).
