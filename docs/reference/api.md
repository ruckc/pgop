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
    size: string           # e.g., "10Gi"
    storageClassName: string  # Optional

  # Container resources
  resources:
    requests:
      cpu: string
      memory: string
    limits:
      cpu: string
      memory: string
```

### ClusterStatus

```yaml
status:
  ready: boolean           # Cluster is accepting connections
  endpoint: string         # Service endpoint (host:port)
  secretName: string       # Credentials secret name
  conditions:
    - type: string
      status: string       # True/False/Unknown
      reason: string
      message: string
      lastTransitionTime: string
```

---

## Role

**Group/Version:** `pgop.ruck.io/v1alpha1`

### RoleSpec

```yaml
spec:
  # Reference to the cluster (required, same namespace)
  clusterRef:
    name: string           # Cluster name

  # PostgreSQL role options
  login: boolean           # LOGIN/NOLOGIN (default: false)
  superuser: boolean       # SUPERUSER/NOSUPERUSER (default: false)
  createDB: boolean        # CREATEDB/NOCREATEDB (default: false)
  createRole: boolean      # CREATEROLE/NOCREATEROLE (default: false)
  inherit: boolean         # INHERIT/NOINHERIT (default: true)
  replication: boolean     # REPLICATION/NOREPLICATION (default: false)
  bypassRLS: boolean       # BYPASSRLS/NOBYPASSRLS (default: false)
  connectionLimit: integer # CONNECTION LIMIT (default: -1)

  # Role memberships
  memberOf:
    - string               # Role names to be a member of

  # Optional: use existing password
  passwordSecretRef:
    name: string           # Secret name
    key: string            # Key within secret
```

### RoleStatus

```yaml
status:
  ready: boolean           # Role exists in PostgreSQL
  secretName: string       # Auto-generated credentials secret
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

  # Database owner role
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
  size: string             # Required: PVC size (e.g., "10Gi")
  storageClassName: string # Optional: StorageClass name
```
