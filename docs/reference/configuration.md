# Configuration

Configuration options for the pgop operator.

## Operator Flags

The operator binary supports the following command-line flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `:8443` | Address for metrics endpoint |
| `--health-probe-bind-address` | `:8081` | Address for health probes |
| `--leader-elect` | `false` | Enable leader election for HA |
| `--metrics-secure` | `true` | Serve metrics over HTTPS |
| `--enable-http2` | `false` | Enable HTTP/2 for webhooks |

## Environment Variables

| Variable | Description |
|----------|-------------|
| `KUBERNETES_SERVICE_HOST` | Set automatically by Kubernetes |
| `KUBERNETES_SERVICE_PORT` | Set automatically by Kubernetes |

## RBAC Permissions

The operator requires the following permissions:

### Cluster-scoped

```yaml
- apiGroups: ["pgop.ruck.io"]
  resources: ["clusters", "roles", "databases"]
  verbs: ["*"]
- apiGroups: ["pgop.ruck.io"]
  resources: ["clusters/status", "roles/status", "databases/status"]
  verbs: ["get", "patch", "update"]
```

### Namespaced

```yaml
- apiGroups: [""]
  resources: ["secrets", "services"]
  verbs: ["*"]
- apiGroups: ["apps"]
  resources: ["statefulsets"]
  verbs: ["*"]
- apiGroups: [""]
  resources: ["persistentvolumeclaims"]
  verbs: ["get", "list", "watch"]
```

## Resource Limits

Recommended resource limits for the operator:

```yaml
resources:
  limits:
    cpu: 500m
    memory: 128Mi
  requests:
    cpu: 10m
    memory: 64Mi
```

## Health Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| `/healthz` | 8081 | Liveness probe |
| `/readyz` | 8081 | Readiness probe |

## Metrics

Metrics are exposed at `:8443/metrics` in Prometheus format.

Key metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `controller_runtime_reconcile_total` | Counter | Total reconciliations |
| `controller_runtime_reconcile_errors_total` | Counter | Reconciliation errors |
| `controller_runtime_reconcile_time_seconds` | Histogram | Reconciliation duration |

## Customizing Deployment

### Using Kustomize

Create an overlay to customize the deployment:

```yaml
# kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: pgop-system
resources:
  - https://github.com/ruckc/pgop/config/default
patches:
  - patch: |-
      - op: replace
        path: /spec/template/spec/containers/0/resources/limits/memory
        value: 256Mi
    target:
      kind: Deployment
      name: pgop-controller-manager
```

### High Availability

Enable leader election for running multiple replicas:

```yaml
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: manager
          args:
            - "--leader-elect"
```
