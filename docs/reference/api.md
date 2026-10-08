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
```

See [Clusters → TLS](../user-guide/clusters.md#tls) for details.

### ClusterStatus

```yaml
status:
  ready: boolean           # Cluster is accepting connections
  endpoint: string         # Service endpoint (host:port)
  secretName: string       # Credentials secret name
  tlsSecretHash: string    # Hash of the certificate the server was last confirmed to present
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

  # Optional: use existing password
  passwordSecretRef:
    name: string           # Secret name
    key: string            # Key within secret
```

### RoleStatus

```yaml
status:
  ready: boolean           # Role exists in PostgreSQL
  roleName: string         # Effective PostgreSQL role name
  secretName: string       # Auto-generated credentials secret
  managedMemberships:      # Roles whose membership pgop granted (revoked when removed)
    - string
  conditions:
    - type: string
      status: string
      reason: string
      message: string
      lastTransitionTime: string
```

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
        - role: string     # Role to grant to
          privileges:
            - string       # USAGE, CREATE, SELECT, INSERT, etc.
```

### DatabaseStatus

```yaml
status:
  ready: boolean
  installedExtensions:
    - string               # List of installed extension names
  createdSchemas:
    - string               # List of created schema names
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
