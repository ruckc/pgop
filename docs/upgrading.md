# Upgrade Notes

pgop's API is `v1alpha1`: fields and behaviour still change between minor
releases, without conversion webhooks or compatibility shims. This page lists
every change that can break an existing installation, a manifest or a client,
by the release that introduced it, and what to do about it. The user-guide
pages have the details; the per-release notes are on the
[GitHub releases](https://github.com/ruckc/pgop/releases) page.

Upgrading across several releases applies all the notes in between. The Helm
chart ships the CRDs: upgrade the chart (or apply `config/crd/bases`) before
or together with the operator image, so the API server knows the new fields.

## Requirements

- **Kubernetes 1.27 or later** (documented minimum since v0.5.0). The CRDs
  use CEL validation rules (`x-kubernetes-validations`), immutable fields
  need transition rules, `storage.retainPolicy: Delete` uses the StatefulSet
  `persistentVolumeClaimRetentionPolicy` (beta and on by default since 1.27),
  and streaming replicas were built against 1.27+. On Kubernetes older than
  1.30 there is no CRD validation ratcheting: an object stored before a rule
  became stricter cannot be updated at all (not even to remove its finalizer)
  until the offending value is fixed. Fix such values **before** upgrading.
- **PostgreSQL 16, 17 and 18** are tested (the end-to-end suite runs on 18;
  pgop's Postgres+pgBackRest images exist for 16, 17 and 18). PostgreSQL 14
  and 15 are not a priority: most features work, but some need newer servers
  (membership `inherit`/`set` options: 16+; `parameterGrants`: 15+;
  `MAINTAIN`: 17+), and they are not covered by the test suite.

## v0.19.0: object grants and default privileges

New, opt-in: `Database.spec.schemas[].objectGrants` and
`schemas[].defaultPrivileges` (see
[Databases: Object Grants](user-guide/databases.md#object-grants)). Nothing
changes for Databases that do not use them.

- **Role deletion** now also revokes the object grants and default
  privileges the Cluster's Databases recorded for the role (never
  `DROP OWNED`). Privileges on tables granted by hand still block
  `DROP ROLE` (`RoleDropBlocked`), as before.
- **Ledger caps:** `status.managedObjectGrants` holds at most 4096 entries
  (one per object and grantee, about 512 KiB) and
  `status.managedDefaultPrivileges` 2048. A change that would exceed a cap
  grants nothing new for that kind (`TooManyGrants`, all or nothing) while
  revokes still run. For large schemas grant to one group role and make the
  login roles its members.
- `"*"` selects at most 5000 objects per kind and schema
  (`TooManyObjects`; nothing is granted or revoked there).

## v0.18.0: extensions

See [Databases: Extensions](user-guide/databases.md#extensions).

- **Untrusted extensions** (installed because the Cluster's
  `rolePolicy.allowedExtensions` lists them) are only installed or updated
  in a schema owned by a superuser that only superusers can write to. Without
  `schema` they go to `public`, which the database owner owns, so a Database
  with an `owner` now reports `ExtensionSchemaNotAllowed`: set `schema` to a
  new schema (pgop creates it, owned by the operator). Extensions that are
  already installed are not touched until they are updated.
- **Any extension** is refused in a schema `PUBLIC` or another role can
  create in, notably `public` on PostgreSQL 14 and older (revoke with
  `publicPrivileges.publicSchemaCreate: false`).
- **`version` is enforced** on installed extensions: a different version is
  updated with `ALTER EXTENSION ... UPDATE TO`, a lower one is refused
  (`ExtensionDowngradeNotAllowed`). It used to be ignored.
- Schemas are created before extensions are installed;
  `status.installedExtensions` only lists the spec's extensions that are
  installed (details in `status.extensions`).
- Extensions installed before v0.18 are not recorded as created by pgop, so
  `dropOnRemoval` never drops them.

## v0.17.0: grant tracking, grantee policy, PUBLIC

See [Databases: What pgop tracks](user-guide/databases.md#what-pgop-tracks).

- **Schema grants are tracked and revoked.** A privilege, grant or whole
  `schemas[]` entry removed from the spec is revoked (the schema is never
  dropped), with `CASCADE` for grant options pgop added. There is no opt-out.
  The first reconcile after the upgrade records the declared grants without
  revoking anything.
- **Only what pgop adds is tracked.** Privileges the grantee already held
  (PostgreSQL defaults, the owner's own, grants made by hand, and schema
  grants made by earlier pgop versions) are not recorded and never revoked.
  The SQL to find such privileges is in
  [Databases: upgrade](user-guide/databases.md#upgrade-breaking-changes).
- **Ledger format: `grantOptions` replaces `withGrantOption`** in
  `status.managedGrants`, `managedSchemaGrants` and
  `Role.status.managedParameterGrants`. Ledgers written by earlier versions
  are still read (`withGrantOption: true` counts as the grant option for all
  recorded privileges, and those entries, including PostgreSQL defaults they
  recorded, are revoked when they leave the spec); new entries use
  `grantOptions`. Tools that read the status must switch fields. The spec
  field `withGrantOption` is unchanged.
- **Grantees are checked** (`GranteeNotAllowed`, and revoked if pgop
  recorded granting them): grantees must be `PUBLIC`, roles managed by a Role
  of the same Cluster, or roles listed in the Cluster's
  `rolePolicy.allowedExistingRoles`; never superusers. The API server rejects
  `postgres`, `none`, `pgop_*`, `pg_*`, a lower-case `public`, and
  `withGrantOption` for `PUBLIC`. Write `PUBLIC` in upper case.
- **Existing schemas are only re-owned and granted on when the Database
  manages them** (`SchemaNotManaged` otherwise): schemas it created, and
  existing ones owned by a non-superuser that is the schema's declared
  `owner`, the database's declared owner, or `pg_database_owner`. In
  particular, a database **adopted without `spec.owner`** keeps its previous
  owner, whose schemas are not taken over: declare `owner` (a Role) on such a
  Database. A schema without a declared `owner` is now created owned by the
  database owner (was: the operator); on a Database without `owner`, such a
  schema removed from `schemas` and added back later reports
  `SchemaNotManaged`.
- **Ledger caps:** `managedGrants` 512 entries, `managedSchemaGrants` 2048,
  `managedParameterGrants` 512. Over the cap nothing more of that kind is
  granted (`TooManyGrants`); revokes still run.
- `schemas` names must be unique (at most 64) and each schema's `grants`
  lists a role at most once (at most 16).
- New: `publicPrivileges` to revoke PostgreSQL's default `PUBLIC` privileges.

## v0.16.0: Role settings, placeholder policy

- **Custom placeholder settings need the Cluster's opt-in.** Database
  `settings` (and the new Role `settings` / `databaseSettings`) only set a
  parameter the server does not know (`myapp.tenant`) when the Cluster lists
  its namespace in `rolePolicy.allowedSettingPrefixes`. Existing placeholder
  settings are refused (`SettingNotAllowed`) **and reset** until the Cluster
  lists their namespace. Add the prefixes to the Cluster before upgrading.
  Extension namespaces (`plperl`, `plpgsql`, `postgis`, `auto_explain`,
  `pg_stat_statements`, `pgaudit`, `cron`, ...) can never be listed. See
  [Databases: which parameters may be set](user-guide/databases.md#which-parameters-may-be-set).

## v0.15.0: role privilege policy

Role and Database writers are no longer superuser-equivalent; widening what
they get needs the Cluster's `spec.rolePolicy`. See
[Clusters: Role Policy](user-guide/clusters.md#role-policy) and
[Roles: upgrade](user-guide/roles.md#upgrade-breaking-changes).

- **`Role.spec.superuser` is removed.** The API server rejects it (strict
  field validation) or drops it. Every managed role is `NOSUPERUSER`; a role
  an earlier pgop made superuser is demoted on its next reconcile.
- **`createRole`, `replication`, `bypassRLS`** need the Cluster's
  `rolePolicy.allowedAttributes`. Otherwise the Role reports
  `RolePolicyViolation` and the role is altered to `NOCREATEROLE
  NOREPLICATION NOBYPASSRLS` (none of them).
- **Memberships are checked**: predefined `pg_*` roles need
  `rolePolicy.allowedPredefinedRoles`; roles no Role of the Cluster manages
  need `rolePolicy.allowedExistingRoles`; superuser roles, `postgres`,
  `pgop_*` and the server-file roles are never allowed. Forbidden
  memberships pgop granted are **revoked** on the next reconcile.
- **No more taking over existing roles and databases.** A Role or Database
  only manages the object its status records (or one it creates); an
  existing role or database is left alone (`RoleNotManaged`,
  `DatabaseNotManaged`) unless a Cluster editor lists it in
  `rolePolicy.adoptableRoles` / `adoptableDatabases`. Objects an earlier pgop
  created and recorded keep working and get a signed ownership marker
  (`COMMENT ON ROLE/DATABASE`). Of two resources with the same PostgreSQL
  name, the later one reports `DuplicateRoleName` / `DuplicateDatabaseName`.
  A Role or Database re-created without its status (from Git after a
  restore, or after the Cluster was re-created) needs the allowlist once.
- **Reserved names:** `roleName` must not start with `pgop_` or `pg_` and
  must not be `postgres`; a Role named `postgres` must set `roleName`, and a
  Database named `postgres`, `template0` or `template1` must set
  `databaseName`.
- **`passwordSecretRef` cannot name a Secret pgop manages** (such as
  `<cluster>-credentials`) or a Cluster's TLS Secret
  (`RolePolicyViolation`).
- **`parameterGrants`** only accept `user`- and `superuser`-context
  parameters (and placeholders); `lo_compat_privileges` joins the denylist
  of `parameterGrants` and `settings`.
- **Extensions:** only trusted extensions, or those in
  `rolePolicy.allowedExtensions`, are installed (`ExtensionNotAllowed`).
  Without `schema`, extensions go to their control file's schema or
  `public`, no longer to the first schema of the database's `search_path`.
  Extension names must match `[A-Za-z0-9_-]` (at most 63 characters).
- **System schemas** (`pg_*`, `information_schema`) are rejected in
  `Database.spec.schemas`.

## v0.14.0: physical backups and restores

- Physical (pgBackRest) backups and restores work end to end. While a
  physical Backup names a Cluster, the official `postgres:<major>` image is
  swapped for pgop's Postgres+pgBackRest image (bare `16`/`17` tags only with
  `physical.acceptImageSwap: true`), adding or removing the Backup restarts
  the pods once, and pgop's image is kept after the Backup is removed. See
  [Backups: Images](user-guide/backups.md#images).
- **Physical restores need the Cluster's confirmation**: the annotation
  `pgop.ruck.io/allow-restore: <restore>` (or `<restore>/<uid>`, required
  once the Cluster had a physical restore). Never keep it in Git. See
  [Restores: Confirmation](user-guide/restores.md#confirmation).
- Physical S3 endpoints must be `https://` (the Backup is `Invalid`
  otherwise).

## v0.13.0: streaming replicas

- `wal_level`, `max_wal_senders`, `max_replication_slots`, `hot_standby`,
  `primary_conninfo` and `primary_slot_name` are reserved in
  `Cluster.spec.parameters` (rejected by the API server). Remove them from
  your Clusters before upgrading.
- `pgop_replicator` is a reserved role name.
- Single-instance Clusters are not restarted by the upgrade; the first
  scale-up from 1 restarts the primary once.

## v0.12.0: database grants and settings

- **Schema privileges are validated.** `schemas[].grants[].privileges` only
  accepts `USAGE`, `CREATE`, `ALL` and `ALL PRIVILEGES` (any case, at most 8
  entries of at most 32 characters); earlier versions passed the values into
  `GRANT` unchecked. Anything else (`SELECT`, ...) makes the Database
  invalid. On Kubernetes older than 1.30 such an object cannot be updated
  until it is fixed: correct it before upgrading. Table privileges are now
  available as `objectGrants` (v0.19.0).
- An empty `search_path` / `temp_tablespaces` value is rejected.

## v0.11.0 / v0.11.1: parameters, password hardening

- `Cluster.spec.parameters`: adding the first parameter (or removing the last
  one) restarts the pod once; Clusters without parameters are untouched.
  `archive_mode`, `archive_command`, `archive_library`, `restore_command`,
  the listen, port and file-location parameters, the include directives, and
  `ssl`, `ssl_cert_file`, `ssl_key_file` and `ssl_min_protocol_version` are
  reserved (other `ssl_*` settings, such as `ssl_ciphers`, may be set).
- `Role.status.passwordHash` is deprecated and cleared; the password
  fingerprint lives in the `pgop.ruck.io/password-fingerprint` annotation of
  the credentials Secret. Passwords are sent as SCRAM-SHA-256 verifiers. A
  password edited into a credentials Secret (or a `passwordSecretRef` value)
  must be the plaintext, valid UTF-8 and not a hash
  (`PasswordSecretInvalid`).

## v0.10.0: passwordSecretRef honored

- **`Role.spec.passwordSecretRef` is now honored** (it used to be accepted and
  ignored). Roles that set it switch to the referenced password immediately
  after the upgrade; applications using the old generated password fail on
  their next connection. A missing Secret or key makes the Role
  `Available=False` (`PasswordSecretNotFound`). List affected Roles before
  upgrading: see [Roles: upgrade notes](user-guide/roles.md#upgrade-notes).

## v0.7.0: structured memberships

- **Removed memberships are revoked.** pgop records the memberships it grants
  (`status.managedMemberships`) and revokes those removed from
  `memberships`/`memberOf`. Set `revokeRemovedMemberships: false` to keep the
  old grant-only behaviour (a transitional opt-out). `memberOf` is
  deprecated: move entries to `memberships` (see
  [Roles: deprecated memberOf](user-guide/roles.md#deprecated-memberof)).

## v0.6.0: roleName / databaseName

- New optional `Role.spec.roleName` and `Database.spec.databaseName`; both are
  immutable once set (or unset). `pg_dump`/`pg_restore` Jobs read the
  database name from `PGDATABASE`.

## v0.5.0: storage retention

- `storage.retainPolicy` (`Retain`, the default, or `Delete`). The minimum
  Kubernetes version is documented as 1.27.

## v0.4.9: false values honored

- `Role.spec.login`, `inherit`, `connectionLimit` and
  `Backup.spec.retention.disabled` are pointers in the Go API (a break for
  programs importing `api/v1alpha1`; use `IsLogin()`, `IsInherit()`,
  `GetConnectionLimit()`). `login: false`, `inherit: false` and
  `connectionLimit: 0` used to be lost (the role stayed `LOGIN`/`INHERIT`):
  Roles affected before the upgrade keep the wrong value stored, so
  **re-apply** their manifests. `NOLOGIN` roles no longer get a generated
  password or credentials Secret (existing Secrets are not deleted).
