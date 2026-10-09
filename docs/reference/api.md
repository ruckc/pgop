# API Reference

Complete API specification for all pgop Custom Resource Definitions.

## Cluster

**Group/Version:** `pgop.ruck.io/v1alpha1`

### ClusterSpec

```yaml
spec:
  # PostgreSQL container image (default: postgres:18)
  image: string

  # Number of replicas (default: 1, max: 1)
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
  # restore_command) are rejected.
  parameters:
    <name>: string
```

See [Clusters → TLS](../user-guide/clusters.md#tls) and
[Clusters → Parameters](../user-guide/clusters.md#parameters) for details.

### ClusterStatus

```yaml
status:
  ready: boolean           # Cluster is accepting connections
  endpoint: string         # Service endpoint (host:port)
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
| `Available` | `True` (reason `ClusterReady`) once the StatefulSet is ready. |
| `ExistingVolume` | Set once when the StatefulSet is created. `True` (reason `PreExistingPVC`) if the data PVC already existed, so PostgreSQL started on retained data; `False` (reason `NewVolume`) otherwise. Never recomputed afterwards. |
| `TLSReady` | Only present while `spec.tls` is set. `True` (reason `TLSActive`) once the server presents the certificate from its TLS Secret (`spec.tls.secretName`, `<cluster>-server-tls` for `issuerRef`, `<cluster>-server-cert` for the self-managed CA). `False` with reason `InvalidTLSSecret` (Secret missing/incomplete/unusable, or a Secret the operator would manage exists and is not owned by the Cluster; the StatefulSet is left unchanged), `CertManagerUnavailable` (`issuerRef` set but cert-manager is not installed), `CertificatePending` (cert-manager has not issued the certificate yet), `WaitingForServer` (pod not ready or not serving TLS yet) or `CertificateReloading` (a rotated certificate is not loaded yet; the operator ran `pg_reload_conf()`, or restarted the pod because the new CA cannot verify the old certificate). |
| `ParametersApplied` | Only present while `spec.parameters` is set. `True` (reason `Applied`) once every parameter is in effect. `False` with reason `WaitingForServer` (pod not ready, rollout in progress, or no connection), `WaitingForSync` (the server does not see the current configuration file yet), `Reloading` (`pg_reload_conf()` ran; checking the result), `PendingRestart` (a parameter needs a restart; the operator restarts the pod once), `InvalidParameter` (the server rejects a name or value; nothing is reloaded or restarted) or `OverriddenByAlterSystem` (`ALTER SYSTEM` overrides a parameter). Never affects `Available`. |

---

## Role

**Group/Version:** `pgop.ruck.io/v1alpha1`

### RoleSpec

```yaml
spec:
  # Reference to the cluster (required, same namespace)
  clusterRef:
    name: string           # Cluster name

  # Optional PostgreSQL role name (default: metadata.name). Immutable.
  # Must match ^[a-z_][a-z0-9_]*$, max 63 chars, no "pg_" prefix,
  # not "postgres" or "pgop_operator".
  roleName: string

  # PostgreSQL role options
  login: boolean           # LOGIN/NOLOGIN (default: false)
  superuser: boolean       # SUPERUSER/NOSUPERUSER (default: false)
  createDB: boolean        # CREATEDB/NOCREATEDB (default: false)
  createRole: boolean      # CREATEROLE/NOCREATEROLE (default: false)
  inherit: boolean         # INHERIT/NOINHERIT (default: true)
  replication: boolean     # REPLICATION/NOREPLICATION (default: false)
  bypassRLS: boolean       # BYPASSRLS/NOBYPASSRLS (default: false)
  connectionLimit: integer # CONNECTION LIMIT (default: -1)

  # Role memberships (PostgreSQL role names, not Role resource names)
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
    - parameter: string    # Parameter name, e.g. log_statement or myapp.tenant_id
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
than PostgreSQL 15, and `ReconcileError` for other failures (errors from `CREATE`/`ALTER ROLE` are
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
therefore read every Secret in it, including `<cluster>-credentials`. Grant
write access to `roles.pgop.ruck.io` only to subjects that may already read
the namespace's Secrets. See
[Roles: who can read a passwordSecretRef Secret](../user-guide/roles.md#security-who-can-read-a-passwordsecretref-secret).

---

## Database

**Group/Version:** `pgop.ruck.io/v1alpha1`

### DatabaseSpec

```yaml
spec:
  # Reference to the cluster (required, same namespace)
  clusterRef:
    name: string

  # Optional PostgreSQL database name (default: metadata.name). Immutable.
  # Must match ^[a-z_][a-z0-9_]*$, max 63 chars,
  # not "postgres", "template0" or "template1".
  databaseName: string

  # Name of the owning Role resource (same namespace). The database is owned
  # by that Role's effective PostgreSQL name.
  owner: string

  # Extensions to install
  extensions:
    - name: string         # Extension name
      schema: string       # Optional schema

  # Schemas to create
  schemas:
    - name: string         # Schema name
      owner: string        # Schema owner
      grants:
        - role: string     # PostgreSQL role to grant to
          privileges:
            - string       # USAGE, CREATE or ALL
          withGrantOption: boolean

  # Database-level privileges (GRANT ... ON DATABASE). pgop-granted
  # privileges removed from the spec are revoked.
  grants:                  # max 256, each role at most once
    - role: string         # PostgreSQL role name (must exist)
      privileges:
        - string           # CONNECT, CREATE, TEMPORARY, TEMP or ALL
      withGrantOption: boolean

  # Per-database parameter defaults (ALTER DATABASE ... SET name TO 'value').
  # Keys: parameter names (identifiers, optionally dotted, max 127 chars).
  # Values: max 4096 chars. pgop-set keys removed from the spec are RESET.
  settings:
    string: string
```

### DatabaseStatus

```yaml
status:
  ready: boolean
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

`retainPolicy` controls what happens to the data PVC (`data-<cluster>-0`) when
the Cluster is deleted. `Retain` keeps it; `Delete` removes it with the Cluster.
It maps onto the StatefulSet `persistentVolumeClaimRetentionPolicy.whenDeleted`
(`whenScaled` is always `Retain`) and requires Kubernetes 1.27+. The field is
mutable; changing it updates the StatefulSet in place.
