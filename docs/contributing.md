# Contributing

Thank you for your interest in contributing to pgop!

## Development Setup

### Prerequisites

- Go 1.21+
- Docker
- kubectl
- Access to a Kubernetes cluster (or kind/minikube)

### Clone and Build

```bash
git clone https://github.com/ruckc/pgop.git
cd pgop
make build
```

### Run Tests

```bash
make test
```

### Run Locally

```bash
# Install CRDs
make install

# Run the operator locally
make run
```

## Making Changes

### Code Style

- Follow standard Go formatting (`go fmt`)
- Use meaningful variable and function names
- Add comments for exported functions
- Keep functions focused and small

### Commit Messages

Use conventional commit format:

```
feat: add support for connection pooling
fix: correct role deletion order
docs: update installation guide
test: add cluster controller tests
```

### Pull Request Process

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-feature`)
3. Make your changes
4. Run tests (`make test`)
5. Commit with a descriptive message
6. Push and open a Pull Request

## Project Structure

```
├── api/v1alpha1/       # CRD type definitions
├── cmd/                # Main entry point
├── config/             # Kustomize manifests
│   ├── crd/           # CRD definitions
│   ├── default/       # Default deployment
│   ├── manager/       # Operator deployment
│   ├── rbac/          # RBAC rules
│   └── samples/       # Example resources
├── docs/               # Documentation
├── internal/
│   ├── controller/    # Reconciliation logic
│   └── postgres/      # PostgreSQL client
└── hack/               # Build scripts
```

## Adding Features

### New CRD Field

1. Update type definition in `api/v1alpha1/`
2. Run `make generate manifests`
3. Update controller logic
4. Add tests
5. Update documentation

### New Controller

1. Create controller in `internal/controller/`
2. Register in `cmd/main.go`
3. Add RBAC markers
4. Add tests
5. Run `make manifests`

## Testing

### Unit Tests

```bash
make test
```

### E2E Tests (coming soon)

```bash
make test-e2e
```

## Reporting Issues

- Use GitHub Issues
- Include Kubernetes version
- Include operator logs
- Include resource YAML (sanitized)

## License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.
