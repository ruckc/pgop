# API Reference

Complete API specification for all pgop Custom Resource Definitions.

## Cluster

**Group/Version:** `pgop.ruck.io/v1alpha1`

### ClusterSpec

```yaml
spec:
  # PostgreSQL container image (default: postgres:18)
  image: string

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
    # bootstrap Job) that Roles may be members of (max 256; not postgres,
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
```

See [Clusters → TLS](../user-guide/clusters.md#tls),
[Clusters → Role policy](../user-guide/clusters.md#role-policy),
[Clusters → Parameters](../user-guide/clusters.md#parameters) and
[Replication](../user-guide/replication.md) for details.

### ClusterStatus

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
| `Available` | `True` (reason `ClusterReady`) while the primary pod is ready. Standbys never affect it (nor `status.ready`, `TLSReady`, the `sslmode` published to clients, or Role/Database reconciles); their health is reported by `status.readyInstances` and `ReplicationHealthy`. |
| `ReplicationHealthy` | Only present while `spec.replicas` is greater than 1. `True` (reason `Streaming`) when every standby streams from the primary. `False` with reason `StandbyNotStreaming` (a standby is still being cloned, catching up, or disconnected; the message names it), `WaitingForPrimary` (the primary pod is not ready) or `ReplicationError` (the operator could not set up the replication role or slots on the primary). When a standby's replication slot was invalidated, the message says which standby to re-clone and a `ReplicationSlotInvalidated` Warning event is recorded. Never affects `Available`. |
| `ExistingVolume` | Set once when the StatefulSet is created. `True` (reason `PreExistingPVC`) if the data PVC already existed, so PostgreSQL started on retained data; `False` (reason `NewVolume`) otherwise. Never recomputed afterwards. |
| `TLSReady` | Only present while `spec.tls` is set. `True` (reason `TLSActive`) once the primary presents the certificate from its TLS Secret (`spec.tls.secretName`, `<cluster>-server-tls` for `issuerRef`, `<cluster>-server-cert` for the self-managed CA). `False` with reason `InvalidTLSSecret` (Secret missing/incomplete/unusable, or a Secret the operator would manage exists and is not owned by the Cluster; the StatefulSet is left unchanged), `CertManagerUnavailable` (`issuerRef` set but cert-manager is not installed), `CertificatePending` (cert-manager has not issued the certificate yet), `WaitingForServer` (pod not ready or not serving TLS yet) or `CertificateReloading` (a rotated certificate is not loaded yet; the operator ran `pg_reload_conf()`, or restarted the pod because the new CA cannot verify the old certificate). |
| `ParametersApplied` | Only present while `spec.parameters` is set. `True` (reason `Applied`) once every parameter is in effect. `False` with reason `WaitingForServer` (pod not ready, rollout in progress, or no connection), `WaitingForSync` (the server does not see the current configuration file yet), `Reloading` (`pg_reload_conf()` ran; checking the result), `PendingRestart` (a parameter needs a restart; the operator restarts the pod once), `InvalidParameter` (the server rejects a name or value; nothing is reloaded or restarted) or `OverriddenByAlterSystem` (`ALTER SYSTEM` overrides a parameter). Never affects `Available`. |

---

## Role

**Group/Version:** `pgop.ruck.io/v1alpha1`

### RoleSpec

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
  login: boolean           # LOGIN/NOLOGIN (default: true)
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
```

Annotation `pgop.ruck.io/rotate-password: <any new value>` requests an
immediate rotation (or, with `passwordSecretRef`, re-applies the referenced
password). Each value is acted on once.

### RoleStatus

```yaml
status:
  ready: boolean           # Role exists in PostgreSQL
  roleName: string         # Effective PostgreSQL role name
  clusterUID: string       # UID of the Cluster roleName was created/adopted on
  secretName: string       # Auto-generated credentials secret
  managedMemberships:      # Roles whose membership pgop granted (revoked when removed)
    - string
  managedParameterGrants:  # Parameter privileges pgop granted (revoked when removed)
    - parameter: string    # Lowercased parameter name
      privileges: [string]
      withGrantOption: boolean
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
denylisted parameter, `RolePolicyViolation` when the Role requests a
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

  # Extensions to install (max 64). Only extensions the server marks as
  # trusted, or listed in the Cluster's rolePolicy.allowedExtensions, are
  # installed (reason ExtensionNotAllowed otherwise). Never dropped.
  extensions:
    - name: string         # Extension name ([A-Za-z0-9_-], max 63 chars)
      schema: string       # Optional schema (default: control file schema, else public)
      version: string      # Optional version (default: the default version)

  # Schemas to create
  schemas:
    - name: string         # Schema name (not pg_* or information_schema)
      owner: string        # Schema owner
      grants:
        - role: string     # PostgreSQL role to grant to
          privileges:
            - string       # USAGE, CREATE, ALL or ALL PRIVILEGES (any case; max 8)
          withGrantOption: boolean

  # Database-level privileges (GRANT ... ON DATABASE). pgop-granted
  # privileges removed from the spec are revoked (with CASCADE when they were
  # granted WITH GRANT OPTION).
  grants:                  # max 256, each role at most once
    - role: string         # PostgreSQL role name (must exist)
      privileges:
        - string           # CONNECT, CREATE, TEMPORARY, TEMP or ALL
      withGrantOption: boolean

  # Per-database parameter defaults (ALTER DATABASE ... SET name TO 'value').
  # Keys: parameter names (identifiers, optionally dotted, max 127 chars).
  # Values: max 4096 chars. pgop-set keys removed from the spec are RESET.
  # Only "user"-context and custom parameters are applied; superuser-only
  # parameters and role, session_authorization, *_preload_libraries,
  # dynamic_library_path, jit_provider, session_replication_role, lo_compat_privileges, pgaudit.*,
  # set_user.*, anon.*, sepgsql.* are refused (reason SettingNotAllowed).
  # search_path/temp_tablespaces take a postgresql.conf list ("$user", app);
  # an empty list is rejected.
  settings:
    string: string
```

The `Available` condition is `False` with reason `SettingNotAllowed` when a
setting is refused, `ExtensionNotAllowed` when an extension is refused,
`SchemaNotAllowed` for a system schema (the other settings, grants,
extensions and schemas are still reconciled), `ReservedName` for a reserved
database name, `DatabaseNotManaged` when the database exists but the Database neither
created it (`status.databaseName`) nor may adopt it (the Cluster's
`rolePolicy.adoptableDatabases`), `DatabaseNotConnectable` when
the database does not allow connections, `DuplicateDatabaseName` when an older Database of
the Cluster has the same PostgreSQL name, and `ReconcileError` for other failures, such as a grantee
role that does not exist yet.

### DatabaseStatus

```yaml
status:
  ready: boolean
  databaseName: string     # Effective PostgreSQL database name
  clusterUID: string       # UID of the Cluster databaseName was created/adopted on
  installedExtensions:
    - string               # List of installed extension names
  createdSchemas:
    - string               # List of created schema names
  managedGrants:           # Database privileges pgop granted (revoked when removed)
    - role: string
      privileges: [string] # Normalized: CONNECT, CREATE, TEMPORARY
      withGrantOption: boolean
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

## Common Types

### ClusterReference

Used in Role and Database specs to reference a Cluster in the same namespace:

```yaml
clusterRef:
  name: string             # Required: Cluster name (same namespace)
```

### SecretKeySelector

Used to reference a key within a Secret:

```yaml
passwordSecretRef:
  name: string             # Secret name
  key: string              # Key within the secret
```

### StorageSpec

Storage configuration for Clusters:

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
