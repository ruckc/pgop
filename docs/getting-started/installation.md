# Installation

This guide covers the supported ways to install pgop in your Kubernetes cluster.

## Prerequisites

- Kubernetes cluster (v1.24+)
- `kubectl` configured to access your cluster
- Cluster admin permissions

## Install with Helm

```bash
helm upgrade -i pgop oci://ghcr.io/ruckc/charts/pgop \
  --namespace pgop-system \
  --create-namespace
```

This installs the CRDs and deploys the operator into the `pgop-system` namespace.

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
helm uninstall pgop --namespace pgop-system
```

If you also want to remove the CRDs after uninstalling the release, run:

```bash
kubectl delete crd \
  backupruns.pgop.ruck.io \
  backups.pgop.ruck.io \
  clusters.pgop.ruck.io \
  databases.pgop.ruck.io \
  restores.pgop.ruck.io \
  roles.pgop.ruck.io
```

!!! warning
    Uninstalling CRDs will delete all Cluster, Role, and Database resources and their associated PostgreSQL data.
