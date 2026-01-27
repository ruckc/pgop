# pgop - PostgreSQL Operator

`pgop` is a Kubernetes Operator built with the Operator SDK and Controller Runtime for managing PostgreSQL clusters. It provides a declarative API to manage the lifecycle of PostgreSQL instances, users, and databases securely and efficiently.

## Features

-   **PostgreSQL Clusters**: Easily provision and manage PostgreSQL stateful sets.
-   **User Management**: Create and manage database roles/users with auto-generated credentials.
-   **Database Management**: specific databases within the cluster.
-   **Secure by Design**: Enforces Restricted Pod Security Standards (RunAsNonRoot, Seccomp, Drop All Capabilities).
-   **Observability**: Built-in metrics export for Prometheus.

## Getting Started

### Prerequisites

-   **Kubernetes Cluster**: A running Kubernetes cluster (v1.24+).
-   **Kubectl**: CLI tool for interacting with your cluster.

### Installation

You can install the operator directly from the GitHub repository without cloning the code:

1.  **Install the CRDs**:

    ```sh
    kubectl apply -f https://raw.githubusercontent.com/ruckc/pgop/main/config/crd/bases/pgop.ruck.io_clusters.yaml
    kubectl apply -f https://raw.githubusercontent.com/ruckc/pgop/main/config/crd/bases/pgop.ruck.io_roles.yaml
    kubectl apply -f https://raw.githubusercontent.com/ruckc/pgop/main/config/crd/bases/pgop.ruck.io_databases.yaml
    ```
    
    *Alternatively, if you have cloned the repo or want to install everything at once:*

2.  **Deploy the Operator**:

    ```sh
    kubectl apply -k https://github.com/ruckc/pgop/config/default
    ```

    This command will:
    -   Create the `pgop-system` namespace.
    -   Create the necessary ServiceAccounts, Roles, and Bindings.
    -   Deploy the Controller Manager using the `latest` image from `ghcr.io/ruckc/pgop`.

### Uninstallation

To remove the operator and all CRDs:

```sh
kubectl delete -k https://github.com/ruckc/pgop/config/default
```

## Usage

### 1. Create a Cluster

Apply the `Cluster` sample to create a PostgreSQL instance:

```sh
kubectl apply -f config/samples/postgres_v1alpha1_cluster.yaml
```

This creates:
- A StatefulSet with PostgreSQL (defaulting to version 18).
- A Service for connectivity.
- A Secret `example-cluster-credentials` containing the superuser credentials.

### 2. Create a Role (User)

Create a database user associated with the cluster:

```sh
kubectl apply -f config/samples/postgres_v1alpha1_role.yaml
```

### 3. Create a Database

Create a specific database owned by a role:

```sh
kubectl apply -f config/samples/postgres_v1alpha1_database.yaml
```

## Development

### Running Tests

Run unit tests:
```sh
make test
```

Run End-to-End (E2E) tests (requires Kind):
```sh
make test-e2e
```

### Building

Build the binary:
```sh
make build
```

Build the docker image:
```sh
make docker-build IMG=example.com/pgop:latest
```

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
