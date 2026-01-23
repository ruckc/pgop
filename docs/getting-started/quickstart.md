# Quick Start

Get a PostgreSQL cluster running in minutes.

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

```
NAME         READY   ENDPOINT                        AGE
my-cluster   true    my-cluster.default.svc:5432     1m
```

## Access Credentials

The operator automatically generates credentials for the superuser:

```bash
kubectl get secret my-cluster-credentials -o jsonpath='{.data.password}' | base64 -d
```

The secret contains:

| Key | Description |
|-----|-------------|
| `username` | Superuser username (pgop_operator) |
| `password` | Superuser password |
| `host` | Service hostname |
| `port` | PostgreSQL port |
| `database` | Default database (postgres) |

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

The operator auto-generates credentials and stores them in a secret:

```bash
kubectl get secret app-user-credentials -o yaml
```

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
    - name: app
      owner: app-user
```

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

- [Learn about Clusters](../user-guide/clusters.md)
- [Manage Roles](../user-guide/roles.md)
- [Create Databases](../user-guide/databases.md)
