# API Reference

Complete API specification for all pgop Custom Resource Definitions: `Cluster`,
`Role`, `Database`, `Backup`, `BackupRun` and `Restore`, all in the group
`pgop.ruck.io`, version `v1alpha1`, and all namespaced. Every reference
(`clusterRef`, `owner`, `databaseRef`, `backupRef`, `backupRunRef`, Secret
references) names an object in the **same namespace**.

The YAML blocks below are schema notation: each value names the field's type,
not a valid value. For manifests you can apply, see the
[User Guide](../user-guide/access-patterns.md) and the `examples/` and
`config/samples/` directories of the repository (all of them are validated
against the CRDs by `make test-manifests`). `kubectl explain
clusters.spec.rolePolicy` (and so on) prints the same descriptions from the
installed CRDs.

## Cluster

**Group/Version:** `pgop.ruck.io/v1alpha1`

### ClusterSpec

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
spec:
  # PostgreSQL container image (default: postgres:18). PostgreSQL 16, 17 and
  # 18 are tested; see User Guide -> Clusters -> Supported Images.
  image: string

  # PostgreSQL major version of the image (>= 1). Auto-detected from the tag
  # (postgres:18, postgis/postgis:16-3.4); set it when the tag has no
  # parseable major version (latest, a digest, a mirror), otherwise the
  # reconcile fails rather than guess the data-directory layout.
  postgresMajorVersion: integer

  # Number of PostgreSQL instances (default: 1, max: 10): pod <cluster>-0 is
  # the primary, the others are asynchronous streaming hot standbys. No
  # automated failover. See User Guide -> Replication.
  replicas: integer

  # PostgreSQL port (default: 5432)
  port: integer

  # Storage configuration
  storage:
    size: string           # e.g., "10Gi" (default "1Gi")
    storageClassName: string  # Optional
    retainPolicy: string   # Retain (default) | Delete

  # Container resources
  resources:
    requests:
      cpu: string
      memory: string
    limits:
      cpu: string
      memory: string

  # Server TLS (optional). Unset = no TLS, sslmode=disable (unchanged behaviour).
  # Set at most one of secretName / issuerRef; neither (tls: {}) = self-managed CA.
  tls:
    secretName: string          # Your kubernetes.io/tls-style Secret (tls.crt, tls.key, ca.crt)
    issuerRef:                  # cert-manager: the operator creates Certificate <cluster>-server
      name: string              # Required: issuer name
      kind: string              # Issuer (default) | ClusterIssuer | external issuer kind
      group: string             # cert-manager.io (default)
    requireTLS: boolean         # Reject non-TLS TCP connections (default: true)
    minProtocolVersion: string  # TLSv1.2 (default) | TLSv1.3

  # PostgreSQL configuration parameters (optional, at most 256). Rendered into
  # ConfigMap <cluster>-config and loaded with -c config_file=...; changes are
  # reloaded, restart-only changes restart the pod once.
  # Names must match ^[a-zA-Z_][a-zA-Z0-9_.]*$; operator-managed names
  # (listen_addresses, port, unix_socket_directories, config_file,
  # data_directory, hba_file, ident_file, external_pid_file, include,
  # include_dir, include_if_exists, ssl, ssl_cert_file, ssl_key_file,
  # ssl_min_protocol_version, archive_mode, archive_command, archive_library,
  # restore_command, wal_level, max_wal_senders, max_replication_slots,
  # hot_standby, primary_conninfo, primary_slot_name) are rejected.
  parameters:
    <name>: string

  # What Roles and Databases on this Cluster may obtain (optional). Only
  # whoever can edit the Cluster can widen it. Absent = all lists empty.
  rolePolicy:
    # Privileged Role attributes Roles may request.
    allowedAttributes:          # createRole | replication | bypassRLS
      - string
    # Predefined roles Roles may be members of. One of: pg_checkpoint,
    # pg_create_subscription, pg_maintain, pg_monitor, pg_read_all_data,
    # pg_read_all_settings, pg_read_all_stats, pg_signal_autovacuum_worker,
    # pg_signal_backend, pg_stat_scan_tables, pg_use_reserved_connections,
    # pg_write_all_data. (pg_execute_server_program, pg_read_server_files and
    # pg_write_server_files can never be allowed.)
    allowedPredefinedRoles:
      - string
    # Roles not managed by a Role of this Cluster (created by a DBA or a
    # bootstrap Job) that Roles may be members of and Databases may grant
    # privileges to (max 256; not postgres,
    # pg_* or pgop_*).
    allowedExistingRoles:
      - string
    # Existing roles / databases that Roles / Databases may take over (max
    # 256 each). Without them only objects pgop created (or recorded in the
    # resource's status) are managed.
    adoptableRoles:
      - string
    adoptableDatabases:
      - string
    # Untrusted extensions Databases may install (max 128; trusted
    # extensions are always allowed).
    allowedExtensions:
      - string
    # Custom parameter namespaces (e.g. myapp for myapp.tenant) whose
    # placeholders Database/Role settings may set (max 32, ^[a-z_][a-z0-9_]*$).
    # Extension namespaces (plperl, pltcl, plv8, plpgsql, postgis,
    # auto_explain, pg_stat_statements, pgaudit, cron, ...) are rejected.
    allowedSettingPrefixes:
      - string
```

See [Clusters → TLS](../user-guide/clusters.md#tls),
[Clusters → Role policy](../user-guide/clusters.md#role-policy),
[Clusters → Parameters](../user-guide/clusters.md#parameters) and
[Replication](../user-guide/replication.md) for details.

### ClusterStatus

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
status:
  ready: boolean           # Cluster is accepting connections
  endpoint: string         # Read-write Service endpoint (host:port); always the primary
  readOnlyEndpoint: string # Read-only Service endpoint <cluster>-ro (host:port); only with replicas > 1
  readyInstances: integer  # Number of ready PostgreSQL pods (primary and standbys)
  currentPrimary: string   # Name of the pod running the primary (<cluster>-0)
  secretName: string       # Credentials secret name
  tlsSecretHash: string    # Hash of the certificate the server was last confirmed to present
  parametersHash: string   # Hash of the generated config file last reloaded (spec.parameters only)
  pendingRestart: [string] # Parameters waiting for a restart (pg_settings.pending_restart)
  lastRestore:             # Last physical Restore that finished on this Cluster
    name: string
    uid: string
    fingerprint: string    # Hash of the Restore spec
    result: string         # Succeeded, Failed or Interrupted
    completionTime: string
  lastWALDrop:             # Last WAL segment pgBackRest dropped (archive queue full)
    time: string
    segment: string        # WAL file name
    closedBy: string       # First successful BackupRun started after the drop (empty: gap open)
  conditions:
    - type: string
      status: string       # True/False/Unknown
      reason: string
      message: string
      lastTransitionTime: string
```

Condition types:

| Type | Meaning |
|------|---------|
| `Available` | `True` (reason `ClusterReady`) while the primary pod is ready; `False` with `ClusterNotReady`, `PausedForRestore` (a physical Restore stopped it) or `RestoreInterrupted`. Standbys never affect it (nor `status.ready`, `TLSReady`, the `sslmode` published to clients, or Role/Database reconciles); their health is reported by `status.readyInstances` and `ReplicationHealthy`. |
| `ReplicationHealthy` | Only present while `spec.replicas` is greater than 1. `True` (reason `Streaming`) when every standby streams from the primary. `False` with reason `StandbyNotStreaming` (a standby is still being cloned, catching up, or disconnected; the message names it), `WaitingForPrimary` (the primary pod is not ready) or `ReplicationError` (the operator could not set up the replication role or slots on the primary). When a standby's replication slot was invalidated, the message says which standby to re-clone and a `ReplicationSlotInvalidated` Warning event is recorded. Never affects `Available`. |
| `ExistingVolume` | Set once when the StatefulSet is created. `True` (reason `PreExistingPVC`) if the data PVC already existed, so PostgreSQL started on retained data; `False` (reason `NewVolume`) otherwise. Never recomputed afterwards. |
| `TLSReady` | Only present while `spec.tls` is set. `True` (reason `TLSActive`) once the primary presents the certificate from its TLS Secret (`spec.tls.secretName`, `<cluster>-server-tls` for `issuerRef`, `<cluster>-server-cert` for the self-managed CA). `False` with reason `InvalidTLSSecret` (Secret missing/incomplete/unusable, or a Secret the operator would manage exists and is not owned by the Cluster; the StatefulSet is left unchanged), `CertManagerUnavailable` (`issuerRef` set but cert-manager is not installed), `CertificatePending` (cert-manager has not issued the certificate yet), `WaitingForServer` (pod not ready or not serving TLS yet) or `CertificateReloading` (a rotated certificate is not loaded yet; the operator ran `pg_reload_conf()`, or restarted the pod because the new CA cannot verify the old certificate). |
| `PhysicalBackup` | Only present once a physical `Backup` named the Cluster. `True` (`Enabled`) while WAL is archived for it; `False` with `Invalid` (a physical Backup names the Cluster but cannot be used; nothing is archived) or `Disabled` (no physical Backup any more; pgop's Postgres+pgBackRest image is kept). See [Backups](../user-guide/backups.md#what-the-operator-sets-up). |
| `WALArchiving` | Only with a physical Backup. `True` (`Archiving`), `False` with `ArchiveFailing` or `WALDropped` (latched until a backup started after the drop succeeds), `Unknown` (`NoWALArchivedYet`, `Unknown`). Mirrored on the Backup. See [Backups: Dropped WAL](../user-guide/backups.md#dropped-wal). |
| `RestoreInterrupted` | `True` while a physical restore failed or was interrupted after its Job started: the Cluster stays stopped (annotation `pgop.ruck.io/restore-interrupted`) until a new Restore succeeds. See [Restores](../user-guide/restores.md#failed-or-interrupted-restores). |
| `ParametersApplied` | Only present while `spec.parameters` is set. `True` (reason `Applied`) once every parameter is in effect. `False` with reason `WaitingForServer` (pod not ready, rollout in progress, or no connection), `WaitingForSync` (the server does not see the current configuration file yet), `Reloading` (`pg_reload_conf()` ran; checking the result), `PendingRestart` (a parameter needs a restart; the operator restarts the pod once), `InvalidParameter` (the server rejects a name or value; nothing is reloaded or restarted) or `OverriddenByAlterSystem` (`ALTER SYSTEM` overrides a parameter). Never affects `Available`. |

---

## Role

**Group/Version:** `pgop.ruck.io/v1alpha1`

### RoleSpec

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
spec:
  # Reference to the cluster (required, same namespace, immutable)
  clusterRef:
    name: string           # Cluster name

  # Optional PostgreSQL role name (default: metadata.name). Immutable.
  # Must match ^[a-z_][a-z0-9_]*$, max 63 chars, no "pg_" or "pgop_"
  # prefix, not "postgres". Required when metadata.name is "postgres".
  roleName: string

  # PostgreSQL role options
  login: boolean           # LOGIN/NOLOGIN (default: true). A NOLOGIN (group) role
                           # gets no password and no credentials Secret.
  createDB: boolean        # CREATEDB/NOCREATEDB (default: false)
  createRole: boolean      # CREATEROLE/NOCREATEROLE (default: false); needs the Cluster's rolePolicy
  inherit: boolean         # INHERIT/NOINHERIT (default: true)
  replication: boolean     # REPLICATION/NOREPLICATION (default: false); needs the Cluster's rolePolicy
  bypassRLS: boolean       # BYPASSRLS/NOBYPASSRLS (default: false); needs the Cluster's rolePolicy
  # Roles are always NOSUPERUSER (spec.superuser was removed).
  connectionLimit: integer # CONNECTION LIMIT (default: -1)

  # Role memberships (PostgreSQL role names, not Role resource names).
  # Rejected by the API server: postgres, pgop_*, pg_execute_server_program,
  # pg_read_server_files, pg_write_server_files. Refused by the operator
  # (reason MembershipNotAllowed; revoked if pgop granted them): superuser
  # roles, pg_* roles not in the Cluster's rolePolicy.allowedPredefinedRoles,
  # roles with attributes the policy does not allow, roles no Role of this
  # Cluster manages (unless in rolePolicy.allowedExistingRoles), and roles
  # that are members of any of these. Applies to memberOf as well.
  memberships:             # max 256, each role at most once
    - role: string         # Role to be a member of (required)
      inherit: boolean     # INHERIT option (optional; PostgreSQL 16+)
      set: boolean         # SET option (optional; PostgreSQL 16+)
      admin: boolean       # ADMIN option (default: false)

  # DEPRECATED: use memberships. Each entry acts like {role: <name>}.
  # A role must not appear in both memberOf and memberships.
  memberOf:
    - string

  # Revoke pgop-granted memberships removed from the spec (default: true).
  # Transitional opt-out; removed together with memberOf.
  revokeRemovedMemberships: boolean

  # Optional: take the password from a Secret you manage (same namespace).
  # Changes to the Secret are applied. Mutually exclusive with passwordRotation.
  # The value must be the plaintext password (UTF-8, no NUL, not a
  # SCRAM-SHA-256$/md5 hash). SECURITY: the operator reads any Secret named
  # here, so whoever can create Roles can read every Secret in the namespace
  # (see "Security" below).
  passwordSecretRef:
    name: string           # Secret name
    key: string            # Key within secret

  # Optional: rotate the operator-generated password on a schedule.
  # Mutually exclusive with passwordSecretRef.
  passwordRotation:
    every: string          # Go duration, e.g. "720h"; minimum "1h"

  # Privileges on configuration parameters (GRANT SET ON PARAMETER).
  # PostgreSQL 15+. pgop-granted entries removed from the spec are revoked.
  parameterGrants:         # max 256, each parameter at most once
    - parameter: string    # Parameter name, e.g. log_statement or myapp.tenant_id.
                           # Not allowed: role, session_authorization,
                           # *_preload_libraries, dynamic_library_path,
                           # jit_provider, session_replication_role, lo_compat_privileges,
                           # pgaudit.*, set_user.*, anon.*, sepgsql.*
      privileges:          # Only SET (the default); ALTER SYSTEM is not offered
        - SET
      withGrantOption: boolean # WITH GRANT OPTION (default: false)

  # Defaults for this role's sessions (ALTER ROLE ... SET name TO value).
  # Same rules as Database spec.settings: user-context parameters, and
  # placeholders in the Cluster's rolePolicy.allowedSettingPrefixes, only
  # (others: SettingNotAllowed), same denylist, list syntax for
  # search_path/temp_tablespaces. pgop-set entries removed are RESET.
  settings:                # max 256
    string: string         # parameter name: value (value max 4096 chars)

  # Defaults for this role's sessions in one database
  # (ALTER ROLE ... IN DATABASE db SET name TO value).
  databaseSettings:        # max 32, each database at most once
    - database: string     # PostgreSQL database name (required, max 63 bytes); a missing
                           # database is pending, not an error
      settings:            # max 64, same rules as settings
        string: string
```

Annotation `pgop.ruck.io/rotate-password: <any new value>` requests an
immediate rotation (or, with `passwordSecretRef`, re-applies the referenced
password). Each value is acted on once.

### RoleStatus

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
status:
  ready: boolean           # Role exists in PostgreSQL
  roleName: string         # Effective PostgreSQL role name
  clusterUID: string       # UID of the Cluster roleName was created/adopted on
  secretName: string       # Credentials Secret <cluster>-<role>-credentials (LOGIN roles only)
  managedMemberships:      # Roles whose membership pgop granted (revoked when removed)
    - string
  managedParameterGrants:  # Parameter privileges pgop granted (revoked when removed)
    - parameter: string    # Lowercased parameter name
      privileges: [string] # Privileges pgop added (only these are revoked)
      grantOptions: [string]   # Privileges whose grant option pgop added
      withGrantOption: boolean # Deprecated (ledgers written before v0.17), read as grantOptions = privileges
  managedSettings:         # Lowercased parameter names pgop set (reset when removed)
    - string
  managedDatabaseSettings: # Per database, the names pgop set (reset when removed)
    - database: string
      settings: [string]
  passwordHash: string     # DEPRECATED: no longer written, cleared on reconcile
  passwordRotatedAt: string # When the operator last generated the password (RFC 3339)
  passwordRotationRequest: string # Last rotate-password annotation value acted on
  conditions:
    - type: string
      status: string
      reason: string
      message: string
      lastTransitionTime: string
```

The `Available` condition is `False` with reason `PasswordSecretNotFound` when
`passwordSecretRef` names a missing Secret or key (or an empty value),
`PasswordSecretInvalid` when the value is not valid UTF-8, contains a NUL byte
or is already a password hash (`SCRAM-SHA-256$...` or `md5` + 32 hex digits),
`UnsupportedServerVersion` when `parameterGrants` is set on a server older
than PostgreSQL 15, `ParameterNotAllowed` when `parameterGrants` names a
denylisted parameter, `SettingNotAllowed` when `settings` or
`databaseSettings` name a parameter that is not `user`-context, or a
placeholder whose namespace the Cluster does not allow (the
others are applied; refused ones pgop set before are reset),
`RolePolicyViolation` when the Role requests a
privileged attribute the Cluster's `rolePolicy` does not allow (the role then
has none of `createRole`, `replication`, `bypassRLS`), names an existing role
pgop must not take over, or names a pgop-managed Secret in `passwordSecretRef`,
`RoleNotManaged` when the PostgreSQL role exists but the Role neither
created it (`status.roleName`) nor may adopt it (the Cluster's
`rolePolicy.adoptableRoles`), or its comment is not the Role's ownership marker,
`DuplicateRoleName` when an older Role of the Cluster has the same
PostgreSQL name, `MembershipNotAllowed` when a membership is refused (the others are applied;
refused ones pgop granted before are revoked), `ReservedName` when the
PostgreSQL name is reserved, `RoleDropBlocked` while a deleted Role cannot be dropped
because objects or privileges pgop does not manage depend on it (the message
lists them), and `ReconcileError` for other failures (errors from `CREATE`/`ALTER ROLE` are
redacted). A rotation emits a `PasswordRotated` Event on the Role.

The role credentials Secret carries a `pgop.ruck.io/password-fingerprint`
annotation (salted SHA-256 of the password last set in PostgreSQL) that the
operator uses to send the password only when it changed. Passwords are sent to
PostgreSQL as client-side computed SCRAM-SHA-256 verifiers, never in
plaintext; server logs of role DDL (`log_statement=ddl`, failing statements)
still contain the verifier and must be treated as secret. A password edited by
hand into the credentials Secret gets the same checks as a
`passwordSecretRef` value (`PasswordSecretInvalid` otherwise).

**Security:** the operator reads the Secret named by `passwordSecretRef` with
its own permissions and copies the key into the role (and database)
credentials Secrets. Anyone who can create or update Roles in a namespace can
therefore read the Secrets in it, except those pgop manages (labeled
`app.kubernetes.io/managed-by: pgop` or owned by a pgop resource, such as
`<cluster>-credentials`) and Cluster TLS Secrets, which are refused with
reason `RolePolicyViolation`. Grant write access to `roles.pgop.ruck.io` only
to subjects that may already read the namespace's other Secrets. See
[Roles: who can read a passwordSecretRef Secret](../user-guide/roles.md#security-who-can-read-a-passwordsecretref-secret).

---

## Database

**Group/Version:** `pgop.ruck.io/v1alpha1`

### DatabaseSpec

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
spec:
  # Reference to the cluster (required, same namespace, immutable)
  clusterRef:
    name: string

  # Optional PostgreSQL database name (default: metadata.name). Immutable.
  # Must match ^[a-z_][a-z0-9_]*$, max 63 chars,
  # not "postgres", "template0" or "template1". Required when metadata.name
  # is one of those.
  databaseName: string

  # Name of the owning Role resource (same namespace). The database is owned
  # by that Role's effective PostgreSQL name.
  owner: string

  # Extensions to install, in order (max 64, each name at most once). Only
  # extensions the server marks as trusted for the requested version, or
  # listed in the Cluster's rolePolicy.allowedExtensions, are installed
  # (reason ExtensionNotAllowed otherwise). Their install and update scripts
  # run as a superuser, so the schemas they run in are checked
  # (ExtensionSchemaNotAllowed): see Databases > Extensions.
  extensions:
    - name: string         # Extension name ([A-Za-z0-9_-], max 63 chars)
      schema: string       # Optional schema (default: control file schema, else public);
                           # not pg_* or information_schema. Created by pgop,
                           # owned by the operator, when missing and not in schemas.
      version: string      # Optional version ([A-Za-z0-9][A-Za-z0-9._+~-]*, max 64);
                           # default: the default version. Changing it updates
                           # (ALTER EXTENSION ... UPDATE TO); downgrades refused.
      cascade: boolean     # CREATE EXTENSION ... CASCADE; every dependency
                           # must pass the policy too
      dropOnRemoval: boolean   # DROP EXTENSION (no CASCADE) once removed from
                               # the list, only if pgop created it. Default false.
      # Privileges on the extension's own objects (pg_depend deptype 'e'),
      # max 16, each role at most once. Tracked in
      # status.managedExtensionGrants and revoked when removed.
      grants:
        - role: string     # Grantee (same rules as grants[].role below)
          schema: [string]     # USAGE, CREATE, ALL: on the extension's own schema
                               # (not public, pg_*, or a schema in schemas)
          tables: [string]     # SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES,
                               # MAINTAIN (PostgreSQL 17+), ALL (all but MAINTAIN);
                               # plain and partitioned tables only. No TRIGGER.
          sequences: [string]  # USAGE, SELECT, UPDATE, ALL
          functions: [string]  # EXECUTE (or ALL): only SQL / PL/pgSQL functions
                               # and procedures that are not SECURITY DEFINER
          # On an extension the server does not trust (allowed only by
          # allowedExtensions), only USAGE (schema), SELECT (tables,
          # sequences) and EXECUTE are granted (ExtensionGrantNotAllowed).

  # Schemas to create (max 64, each name at most once)
  schemas:
    - name: string         # Schema name (not pg_* or information_schema)
      owner: string        # Schema owner
      # Schema privileges (GRANT ... ON SCHEMA), max 16, each role at most
      # once. Tracked in status.managedSchemaGrants: pgop-granted privileges
      # removed from the spec (or whose schema entry is removed) are revoked,
      # with CASCADE when they were granted WITH GRANT OPTION.
      grants:
        - role: string     # Grantee (same rules as grants[].role below)
          privileges:
            - string       # USAGE, CREATE, ALL or ALL PRIVILEGES (any case; max 8)
          withGrantOption: boolean   # not allowed for PUBLIC
      # Privileges on existing objects of the schema (max 32). Only objects
      # owned by a role of the Cluster's Roles (no superuser; also for the
      # database and schema owners) or the operator are granted on, and no
      # extension members; the operator's SECURITY DEFINER/non-SQL functions,
      # views and TRIGGER/MAINTAIN on its tables are skipped too (reason
      # ObjectGrantSkipped). Tracked per object in
      # status.managedObjectGrants: revoked once no entry selects the object.
      objectGrants:
        - role: string     # Grantee (same rules as grants[].role below)
          kind: string     # table, sequence, function, procedure or type
          objects:         # 1-64 names (max 255 chars each), or ["*"] alone:
            - string       # every object of the kind (max 5000, TooManyObjects).
                           # Routines: name(argtypes) for one, name for all overloads.
                           # Missing names: ObjectNotFound (retried).
          privileges:      # table: SELECT INSERT UPDATE DELETE TRUNCATE REFERENCES
            - string       # TRIGGER MAINTAIN(17+) ALL; sequence: USAGE SELECT UPDATE ALL;
                           # function/procedure: EXECUTE ALL; type: USAGE ALL
          withGrantOption: boolean   # not allowed for PUBLIC
      # Default privileges for objects forRole creates in the schema later
      # (ALTER DEFAULT PRIVILEGES FOR ROLE ... IN SCHEMA ... GRANT), max 32,
      # each forRole/role/kind at most once. Tracked in
      # status.managedDefaultPrivileges and revoked when removed.
      defaultPrivileges:
        - forRole: string  # A non-superuser role managed by a Role of this
                           # Cluster (DefaultPrivilegeNotAllowed otherwise);
                           # never postgres, pgop_*, pg_* or PUBLIC
          role: string     # Grantee (same rules as grants[].role below)
          kind: string     # table, sequence, function (also procedures) or type
          privileges:
            - string       # As for objectGrants of the same kind
          withGrantOption: boolean   # not allowed for PUBLIC

  # Database-level privileges (GRANT ... ON DATABASE). pgop-granted
  # privileges removed from the spec are revoked (with CASCADE when they were
  # granted WITH GRANT OPTION).
  grants:                  # max 256, each role at most once
    - role: string         # Grantee: PUBLIC (upper case), a role managed by a
                           # Role of this Cluster, or one listed in the
                           # Cluster's rolePolicy.allowedExistingRoles (must
                           # exist). Never postgres, none, pgop_*, pg_* or a
                           # superuser (reason GranteeNotAllowed). Max 63 chars.
      privileges:
        - string           # CONNECT, CREATE, TEMPORARY, TEMP or ALL
      withGrantOption: boolean   # not allowed for PUBLIC

  # Revoke PostgreSQL's default PUBLIC privileges. false revokes; unset or
  # true leaves (or restores) the default. What pgop revoked is recorded in
  # status.revokedPublicPrivileges and granted back once no longer requested.
  # Must not contradict a PUBLIC entry in grants or schemas[public].grants.
  publicPrivileges:
    connect: boolean             # CONNECT on the database
    temporary: boolean           # TEMPORARY on the database
    publicSchemaUsage: boolean   # USAGE on the schema public
    publicSchemaCreate: boolean  # CREATE on the schema public (PostgreSQL < 15 default)

  # Per-database parameter defaults (ALTER DATABASE ... SET name TO 'value').
  # Keys: parameter names (identifiers, optionally dotted, max 127 chars).
  # Values: max 4096 chars. pgop-set keys removed from the spec are RESET.
  # Only "user"-context parameters, and placeholders (custom parameters the
  # server does not know) whose namespace is in the Cluster's
  # rolePolicy.allowedSettingPrefixes, are applied; superuser-only
  # parameters and role, session_authorization, *_preload_libraries,
  # dynamic_library_path, jit_provider, session_replication_role, lo_compat_privileges, pgaudit.*,
  # set_user.*, anon.*, sepgsql.* are refused (reason SettingNotAllowed).
  # search_path/temp_tablespaces take a postgresql.conf list ("$user", app);
  # an empty list is rejected.
  settings:
    string: string
```

The `Available` condition is `False` with reason `SettingNotAllowed` when a
setting is refused, `ExtensionNotAllowed` when an extension (or a dependency
cascade would install) is refused, `ExtensionSchemaNotAllowed` when an
extension's script would run in a schema other roles can write to,
`ExtensionDependencyMissing`, `ExtensionVersionNotAvailable`,
`ExtensionDowngradeNotAllowed`, `ExtensionSchemaMismatch`,
`ExtensionNotManaged`, `ExtensionDropBlocked` and `ExtensionGrantNotAllowed`
(see [Databases: Extensions](../user-guide/databases.md#extensions)),
`SchemaNotAllowed` for a system schema, `ObjectGrantSkipped` when an object
named in `schemas[].objectGrants` is not granted on, `ObjectNotFound` when it
does not exist (yet), `TooManyObjects` when `"*"` selects more than 5000
objects of a kind in a schema, `DefaultPrivilegeNotAllowed` when a
`defaultPrivileges` `forRole` is not allowed (see
[Databases: Object Grants](../user-guide/databases.md#object-grants)),
`GranteeNotAllowed` when a grantee
in `grants`, `schemas[].grants`, `objectGrants`, `defaultPrivileges` or
`extensions[].grants` is not allowed (grants to it are not
applied, and revoked if pgop granted them), `PublicPrivilegeConflict` when
`publicPrivileges` revokes what a `PUBLIC` grant grants, `TooManyGrants` when
the declared grants plus those pgop still tracks exceed the status ledger,
`RevokeSkipped` (reported once) when a revoke was blocked by dependent
privileges pgop did not enable, `PublicPrivilegeStillHeld` when PUBLIC keeps a
revoked privilege from another grantor, `SchemaNotManaged` for an existing schema the
Database neither created nor owns (in all these cases the other
settings, grants, extensions and schemas are still reconciled), `ReservedName` for a reserved
database name, `DatabaseNotManaged` when the database exists but the Database neither
created it (`status.databaseName`) nor may adopt it (the Cluster's
`rolePolicy.adoptableDatabases`), `DatabaseNotConnectable` when
the database does not allow connections, `DuplicateDatabaseName` when an older Database of
the Cluster has the same PostgreSQL name, and `ReconcileError` for other failures, such as a grantee
role that does not exist yet.

The `ObjectGrantsComplete` condition (only on Databases with object grants)
is `True` (reason `AllObjectsGranted`) when every object the object grants
select is granted on, and `False` (reason `ObjectGrantSkipped`, with counts
and examples) when some are skipped; skipped objects selected with `"*"` do
not make the Database unavailable.

### DatabaseStatus

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
status:
  ready: boolean
  databaseName: string     # Effective PostgreSQL database name
  clusterUID: string       # UID of the Cluster databaseName was created/adopted on
  installedExtensions:
    - string               # Extensions of spec.extensions that are installed
  extensions:              # max 128: spec.extensions, and removed ones pgop still has to drop
    - name: string
      version: string      # Installed version (empty: not installed)
      schema: string       # Schema it is installed in
      created: boolean     # pgop created it (recorded before CREATE EXTENSION)
      oid: integer         # pg_extension.oid of the installation pgop created
      owner: string        # its extowner; both must still match for a drop
      dropOnRemoval: boolean   # dropOnRemoval as last reconciled
      reason: string       # Why it is not as requested (a condition reason)
      message: string
      skippedObjects: integer  # Objects grants asked for that pgop does not grant on
  createdSchemas:
    - string               # Schemas the Database manages (created, or owned by the declared/database owner)
  # Ledgers record only what pgop added (privileges the grantee did not hold,
  # grant options it did not have); only these are revoked when removed.
  managedGrants:           # max 512
    - role: string         # Role name or PUBLIC
      privileges: [string] # Normalized: CONNECT, CREATE, TEMPORARY
      grantOptions: [string]   # Privileges whose grant option pgop added
      withGrantOption: boolean # Deprecated (ledgers written before v0.17), read as grantOptions = privileges; no longer written
  managedSchemaGrants:     # max 2048
    - schema: string
      role: string         # Role name or PUBLIC
      privileges: [string] # Normalized: CREATE, USAGE
      grantOptions: [string]
  managedExtensionGrants:  # max 1024
    - extension: string
      role: string         # Role name or PUBLIC
      kind: string         # schema, tables, sequences or functions
      schema: string       # The extension's schema (kind schema)
      privileges: [string] # Added on at least one object; revoked from every object of the kind
  managedObjectGrants:     # max 4096 (and about 512 KiB), one entry per object and grantee
    - schema: string
      kind: string         # table, sequence, function, procedure or type
      object: string       # As PostgreSQL renders it (app."Orders", app.f(integer)), max 1024
      oid: integer         # The object's OID: followed across renames; gone once dropped
      role: string         # Role name or PUBLIC
      privileges: [string]
      grantOptions: [string]
  managedDefaultPrivileges: # max 2048
    - schema: string
      forRole: string
      kind: string         # table, sequence, function or type
      role: string         # Role name or PUBLIC
      privileges: [string]
      grantOptions: [string]
  objectGrants:            # Per schema and kind selected by objectGrants
    - schema: string
      kind: string
      granted: integer     # Selected objects pgop grants on
      skipped: integer     # Selected objects (or privileges on them) pgop skips
      skippedExamples: [string]  # Up to 5: "<object> <why>"
  revokedPublicPrivileges: # Default PUBLIC privileges pgop revoked (granted back when no longer requested)
    - string               # connect, temporary, publicSchemaUsage, publicSchemaCreate
  managedSettings:         # Lowercased parameter names pgop set (reset when removed)
    - string
  conditions:
    - type: string
      status: string
      reason: string
      message: string
      lastTransitionTime: string
```

---

## Backup

**Group/Version:** `pgop.ruck.io/v1alpha1`

A `Backup` is a backup **policy**: the operator turns it into CronJobs. See
[Backups](../user-guide/backups.md).

### BackupSpec

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
spec:
  # Required: physical (pgBackRest, a whole Cluster) or logical (pg_dump, one Database)
  type: string

  # physical: the Cluster to back up (required for physical). One physical
  # Backup per Cluster; a second one is Invalid.
  clusterRef:
    name: string

  # logical: the Database resource to back up (required for logical)
  databaseRef:
    name: string

  # logical: cron schedule of the CronJobs <backup>-schema and <backup>-data
  # (one schedule for both; default "0 2 * * *")
  schedule: string

  # physical: schedules and images
  physical:
    fullSchedule: string         # CronJob <backup>-full (default "0 2 * * 0")
    incrementalSchedule: string  # CronJob <backup>-incremental (default "0 2 * * 1-6")
    image: string                # pgBackRest Job image (default ghcr.io/ruckc/pgop-pgbackrest:<version>);
                                 # must run the same pgBackRest version as the Cluster's image
    postgresImageIncludesPgbackrest: boolean  # the Cluster's spec.image already has pgBackRest
                                 # (same version, /usr/bin/pgbackrest, uid 999): no image swap
    acceptImageSwap: boolean     # allow replacing a bare postgres:16 / :17 tag by pgop's
                                 # trixie image (check collations first)
    archivePushQueueMax: quantity    # WAL allowed to queue in pg_wal while archiving fails
                                 # (default: a quarter of the Cluster's storage size, min 64Mi);
                                 # beyond it WAL is dropped (WALArchiving=False/WALDropped)

  # physical: pgBackRest retention (logical dumps are never expired by pgop)
  retention:
    disabled: boolean            # default true: nothing is expired (use with write-only credentials)
    keepLast: integer            # keep N full backups (repo1-retention-full=N); needs disabled: false
    keepDays: integer            # keep full backups newer than N days; needs disabled: false

  # physical: how long a completed BackupRun record is kept (Go duration,
  # default "168h"); copied into spec.ttl of the BackupRuns pgop creates, so
  # a change only affects later runs. No effect on logical backups (they
  # create no BackupRuns).
  backupRunTTL: string

  # Required: where backups are stored
  destination:
    type: string                 # s3 (azure and gcs are accepted by the schema but not implemented)
    s3:
      bucket: string             # Required
      region: string             # Required
      prefix: string             # physical: the repository path (default
                                 # /<namespace>/<cluster>/<backup>); logical: dumps go to
                                 # <prefix>/schema/ and <prefix>/data/
      endpoint: string           # S3-compatible endpoint. physical: must be https://
      credentialsSecretRef:      # Secret with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY;
        name: string             # omitted: ambient credentials (IRSA, instance profile)
      caSecretRef:               # physical: PEM CA bundle for a private endpoint
        name: string
        key: string
    azure:                       # not implemented yet
      container: string
      storageAccount: string
      credentialsSecretRef:
        name: string
    gcs:                         # not implemented yet
      bucket: string
      prefix: string
      credentialsSecretRef:
        name: string

  # physical: pgBackRest repository encryption
  encryption:
    enabled: boolean             # Required in the block
    keySecretRef:                # Secret key holding the passphrase (required when enabled)
      name: string
      key: string
```

### BackupStatus

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
status:
  lastFullBackupTime: string         # physical: completion of the last full backup Job
  lastIncrementalBackupTime: string  # physical: completion of the last incremental backup Job
  conditions:
    - type: string
      status: string
      reason: string
      message: string
      lastTransitionTime: string
```

| Condition | Meaning |
|-----------|---------|
| `Available` | `True` (reason `Scheduled`) when the CronJobs are in place. `False` with `Invalid` when the spec cannot be used (an `http://` endpoint for a physical backup, an unsupported Cluster image, a second physical Backup of the Cluster; nothing is scheduled and the Cluster is not changed) or `ReconcileError` (for example a missing Database or Cluster). |
| `WALArchiving` | Physical only: mirrored from the Cluster (see above). |

---

## BackupRun

**Group/Version:** `pgop.ruck.io/v1alpha1`

A `BackupRun` records one execution of a Backup and is what a `Restore`
restores from. pgop creates one, named after the Job and owned by the Backup,
for every Job of a physical Backup's CronJobs (also Jobs created by hand with
`kubectl create job --from=cronjob/...`). **Logical backup Jobs do not record
BackupRuns**: create one by hand to restore a dump (see
[Restores](../user-guide/restores.md#logical-restore-pg_restore)).

### BackupRunSpec

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
spec:
  backupRef:                 # Required: the Backup (destination and credentials)
    name: string
  type: string               # Required: full, incremental (physical) or schema, data (logical)
  ttl: string                # Go duration (default 168h). pgop sets it from the Backup's
                             # backupRunTTL when it creates a physical run
```

### BackupRunStatus

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
status:
  phase: string              # Pending, Running, Succeeded or Failed (from the Job)
  startTime: string
  completionTime: string
  location: string           # physical: s3://<bucket>/<repository path>/backup/main/<label>;
                             # logical: the dump, s3://<bucket>/<prefix>/<schema|data>/<time>.dump
  sizeBytes: integer         # Reserved: not filled in yet
  jobName: string            # The Job executing the backup
  conditions:
    - type: string           # Available: Pending, Succeeded or Failed
      status: string
      reason: string
      message: string
      lastTransitionTime: string
```

A BackupRun is deleted `spec.ttl` (default 7 days) after
`status.completionTime`; the backup itself stays in the repository until
pgBackRest expires it. Only `spec.ttl` counts: pgop copies the Backup's
`backupRunTTL` into it when it creates a physical run, so changing
`backupRunTTL` later does not affect existing runs. A BackupRun created by
hand (for a logical restore) has no Job, never gets a `completionTime`, stays
`Pending` and is never deleted by pgop; delete it yourself after the restore.

**Security:** `status.location` decides what a Restore downloads, and a
logical restore runs the dump's SQL as the operator's superuser. Permission
to `patch`/`update` `backupruns/status` (together with creating Restores),
or write access to the backup bucket, is superuser-equivalent. See
[Restores](../user-guide/restores.md#logical-restore-pg_restore).

---

## Restore

**Group/Version:** `pgop.ruck.io/v1alpha1`

A `Restore` is a **one-shot** restore of a BackupRun. Its `spec` is immutable
(the API server rejects changes): create a new Restore to restore again. See
[Restores](../user-guide/restores.md).

A **logical** Restore runs `pg_restore --no-owner --clean --if-exists` as the
operator's superuser: the dump's SQL runs as a superuser (creating Restores
for dumps you do not trust is superuser-equivalent), a schema dump drops and
re-creates its tables (data included), a data dump appends rows, and `GRANT`s
to roles the target Cluster lacks fail, which makes the Job, and the Restore,
end `Failed` although most of the dump was restored: check the Job log. A
logical Restore created before its BackupRun has `status.location` fails at
once and must be re-created.

### RestoreSpec

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
spec:
  type: string               # Required: logical (pg_restore) or physical (pgBackRest)
  backupRunRef:              # Required: the BackupRun to restore; its Backup provides
    name: string             # the destination and credentials
  clusterRef:                # Required: the target Cluster. physical: must be the
    name: string             # Backup's clusterRef; logical: may be another Cluster
  databaseRef:               # logical: the target Database resource (required)
    name: string
  targetTime: string         # physical: RFC 3339 point-in-time target (second precision);
                             # unset restores the BackupRun's backup to consistency
```

A physical Restore does nothing until the Cluster confirms it with the
annotation `pgop.ruck.io/allow-restore: <restore name>` (or `<name>/<uid>`,
required once the Cluster had a physical restore); see
[Annotations](#annotations).

### RestoreStatus

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
status:
  phase: string              # Pending, Running, Succeeded or Failed
  startTime: string
  completionTime: string
  jobName: string            # <restore>-restore
  conditions:
    - type: string           # Available
      status: string
      reason: string
      message: string
      lastTransitionTime: string
```

`Available` reasons: `Running`, `Succeeded`, `Failed`, `ReconcileError`, and,
for physical restores, the steps `AwaitingConfirmation` (the Cluster has not
confirmed the Restore), `WaitingForRestore` (another Restore holds the
Cluster), `WaitingForOldJob`, `StoppingCluster`, `CreatingJob` and
`Restoring`.

---

## Annotations

Annotations users set (the other `pgop.ruck.io/*` annotations are written by
the operator and should not be edited):

| Annotation | On | Effect |
|------------|----|--------|
| `pgop.ruck.io/rotate-password: <any new value>` | Role | Rotate the generated password now (with `passwordSecretRef`: set the referenced password in PostgreSQL again). Each value is acted on once. See [Roles: rotating on demand](../user-guide/roles.md#rotating-on-demand). |
| `pgop.ruck.io/allow-restore: <restore>` or `<restore>/<uid>` | Cluster | Confirms a physical Restore of this Cluster; removed by the operator when the Restore finishes. Apply it with `kubectl annotate`, **never commit it to Git**. See [Restores: confirmation](../user-guide/restores.md#confirmation). |
| `pgop.ruck.io/restore-interrupted` | Cluster | Set by the operator after a failed or interrupted physical restore. Removing it by hand starts PostgreSQL on the data directory as it is (only when you know the restore did not change it). |
| `pgop.ruck.io/allow-primary-init: "true"` | Cluster | Lets the primary run `initdb` on an empty volume although the Cluster had data before (start over with an empty database). Remove it again afterwards. See [Replication](../user-guide/replication.md#primary-volume-lost). |
| any change, e.g. `pgop.ruck.io/reconcile: <timestamp>` | Database | Triggers a reconcile, for example to apply `objectGrants` with `"*"` to tables a migration just created. |

---

## Common Types

### ClusterReference

Used in Role and Database specs to reference a Cluster in the same namespace:

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
clusterRef:
  name: string             # Required: Cluster name (same namespace)
```

### SecretKeySelector

Used to reference a key within a Secret:

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
passwordSecretRef:
  name: string             # Secret name
  key: string              # Key within the secret
```

### StorageSpec

Storage configuration for Clusters:

<!-- pgop-validate: skip (schema notation, not a manifest) -->
```yaml
storage:
  size: string             # Optional: PVC size (e.g., "10Gi"), default "1Gi"
  storageClassName: string # Optional: StorageClass name
  retainPolicy: string     # Optional: Retain (default) | Delete
```

`retainPolicy` controls what happens to the data PVCs (`data-<cluster>-<n>`)
when the Cluster is deleted. `Retain` keeps them; `Delete` removes them with
the Cluster. It maps onto the StatefulSet
`persistentVolumeClaimRetentionPolicy.whenDeleted` (`whenScaled` is always
`Retain`, so a scale-down never deletes the primary's volume) and requires
Kubernetes 1.27+. The field is mutable; changing it updates the StatefulSet in
place. When `spec.replicas` is lowered, the operator itself deletes the data
PVCs of the removed standbys (never `data-<cluster>-0`) once their pods are
gone, so a standby added later is cloned afresh.
