# Quick Start

Get a PostgreSQL cluster running in minutes.

Install the operator first with the [Helm chart](installation.md), then create your first PostgreSQL cluster.

## Create a Cluster

Create your first PostgreSQL cluster:

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Cluster
metadata:
  name: my-cluster
  namespace: default
spec:
  image: postgres:18
  replicas: 1
  port: 5432
  storage:
    size: 5Gi
```

Apply the manifest:

```bash
kubectl apply -f cluster.yaml
```

## Check Cluster Status

```bash
kubectl get cluster my-cluster
```

Output:

```console
NAME         READY   INSTANCES   READY INSTANCES   ENDPOINT                                    AGE
my-cluster   true    1           1                 my-cluster.default.svc.cluster.local:5432   1m
```

## Access Credentials

The operator automatically generates credentials for the superuser:

```bash
kubectl get secret my-cluster-credentials -o jsonpath='{.data.password}' | base64 -d
```

The secret contains:

| Key | Description |
|-----|-------------|
| `username` | Superuser username (`pgop_operator`) |
| `password` | Superuser password |
| `host` | Service hostname |
| `port` | PostgreSQL port |
| `database` | Default database (`postgres`) |
| `sslmode` | `disable`, or `verify-full` once [TLS](../user-guide/clusters.md#tls) is active |
| `uri` | Ready-made `postgresql://` URI |

Use it for administration only; applications get their own Role and Database
credentials below.

## Create a Role

Create an application user:

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
  connectionLimit: 10
```

The operator generates a password and stores the credentials in the Secret
`<cluster>-<role>-credentials`:

```bash
kubectl get secret my-cluster-app-user-credentials -o yaml
```

`login: false` would make a group role instead (no password, no Secret), to
grant privileges to and make users members of; see
[Roles](../user-guide/roles.md#role-types).

## Create a Database

Create a database with extensions:

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
  schemas:
    - name: app             # owned by app-user, the database owner
```

The Database's connection Secret `myapp-app-user-credentials` has everything
an application needs, including `database: myapp` and a `uri`.

## Connect to PostgreSQL

Port-forward to access the database:

```bash
kubectl port-forward svc/my-cluster 5432:5432
```

Connect using the credentials:

```bash
PGPASSWORD=$(kubectl get secret my-cluster-credentials -o jsonpath='{.data.password}' | base64 -d) \
psql -h localhost -U pgop_operator -d postgres
```

## Next Steps

- [Learn about Clusters](../user-guide/clusters.md): replicas, TLS, parameters, role policy
- [Users and access patterns](../user-guide/access-patterns.md): owner, application, read-only, reporting and monitoring roles end to end
- [Manage Roles](../user-guide/roles.md) and [Databases](../user-guide/databases.md)
- [Backups](../user-guide/backups.md) and [Restores](../user-guide/restores.md)
- Upgrading? Read the [Upgrade Notes](../upgrading.md)
