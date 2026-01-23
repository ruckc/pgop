# Installation

This guide covers different methods to install pgop in your Kubernetes cluster.

## Prerequisites

- Kubernetes cluster (v1.24+)
- `kubectl` configured to access your cluster
- Cluster admin permissions

## Install with Kustomize

### Install CRDs

```bash
kubectl apply -k https://github.com/ruckc/pgop/config/crd
```

### Deploy the Operator

```bash
kubectl apply -k https://github.com/ruckc/pgop/config/default
```

## Install from Source

### Clone the Repository

```bash
git clone https://github.com/ruckc/pgop.git
cd pgop
```

### Install CRDs

```bash
make install
```

### Run the Operator Locally (Development)

```bash
make run
```

### Deploy to Cluster

```bash
make deploy IMG=<your-registry>/pgop:latest
```

## Verify Installation

Check that the operator is running:

```bash
kubectl get pods -n pgop-system
```

Expected output:

```
NAME                                      READY   STATUS    RESTARTS   AGE
pgop-controller-manager-xxxxx-xxxxx       1/1     Running   0          1m
```

Check that CRDs are installed:

```bash
kubectl get crds | grep pgop.ruck.io
```

Expected output:

```
clusters.pgop.ruck.io      2026-01-01T00:00:00Z
databases.pgop.ruck.io     2026-01-01T00:00:00Z
roles.pgop.ruck.io         2026-01-01T00:00:00Z
```

## Uninstall

```bash
# Remove CRDs (this will delete all managed resources!)
make uninstall

# Remove operator
kubectl delete -k config/default
```

!!! warning
    Uninstalling CRDs will delete all Cluster, Role, and Database resources and their associated PostgreSQL data.
