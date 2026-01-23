# Databases

A Database resource represents a PostgreSQL database within a cluster.

## Overview

The Database controller:

1. Connects to the referenced PostgreSQL cluster
2. Creates the database with the specified owner
3. Installs requested extensions
4. Creates schemas with ownership
5. Applies schema grants
6. Drops the database on deletion

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
  extensions:
    - name: uuid-ossp
    - name: pg_trgm
    - name: postgis
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
            - SELECT
```

## Spec Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `clusterRef.name` | string | **required** | Name of the Cluster resource (same namespace) |
| `owner` | string | - | Role that owns the database |
| `extensions` | []ExtensionSpec | - | Extensions to install |
| `schemas` | []SchemaSpec | - | Schemas to create |

### ExtensionSpec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Extension name |
| `schema` | string | - | Schema to install extension in |

### SchemaSpec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Schema name |
| `owner` | string | - | Role that owns the schema |
| `grants` | []GrantSpec | - | Privileges to grant |

### GrantSpec

| Field | Type | Description |
|-------|------|-------------|
| `role` | string | Role to grant privileges to |
| `privileges` | []string | Privileges (USAGE, CREATE, SELECT, etc.) |

## Status

| Field | Description |
|-------|-------------|
| `ready` | Whether the database is ready |
| `installedExtensions` | List of installed extensions |
| `createdSchemas` | List of created schemas |
| `conditions` | Detailed status conditions |

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

## Schema with Grants

Create a schema with read-only access for reporting:

```yaml
schemas:
  - name: app
    owner: app-user
  - name: app
    grants:
      - role: readonly_user
        privileges:
          - USAGE
          - SELECT
```

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
          privileges: [USAGE, SELECT]
        - role: order-service
          privileges: [USAGE, SELECT]
```

