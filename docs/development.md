# Development

## Prerequisites

- Go 1.26+
- A kcp binary (downloaded automatically by `make kcp`)

## Project Structure

```
api/v1alpha1/           Go types for DependencyRule and Dependency
cmd/controller/         Controller entrypoint
config/
  crds/                 Generated CRDs (intermediate, from controller-gen)
  kcp/                  Generated APIResourceSchemas + APIExport (from apigen)
docs/                   Documentation
internal/
  controller/
    dependencyrule_controller.go   Multicluster DependencyRule reconciler
    dependent_controller.go        Dependent resource reconciler (creates Dependencies)
    webhook_installer.go           Manages ValidatingWebhookConfigurations
  webhook/
    deletion_validator.go          Admission webhook handler
    fieldpath.go                   Dot-notation field path resolver
test/
  e2e/                  End-to-end tests (run against real kcp)
  fixtures/             YAML fixtures for test provider schemas
```

## Make Targets

### Code Generation

```sh
make generate    # Generate deepcopy methods (controller-gen object)
make manifests   # Generate CRDs -> APIResourceSchemas + APIExport
```

`make manifests` runs two stages:

1. `controller-gen crd` generates standard Kubernetes CRDs into `config/crds/`
2. `apigen` (from `github.com/kcp-dev/sdk`) converts CRDs into kcp
   `APIResourceSchema` and `APIExport` manifests in `config/kcp/`

The `apigen` tool preserves schema names across regenerations when the spec
hasn't changed, avoiding unnecessary churn.

### Build

```sh
make build         # Build binary to bin/dependency-controller
make docker-build  # Build Docker image
```

### Test

```sh
make test          # All tests (unit + e2e)
make test-unit     # Unit tests only
make test-e2e      # E2E tests against a local kcp instance
```

### Lint & Format

```sh
make fmt    # Add license headers, format code, run lint --fix
make lint   # Run golangci-lint
make vet    # Run go vet
```

## E2E Tests

The e2e tests spin up a real kcp instance and create a 5-workspace topology:

1. **dep-ctrl** -- hosts the dependency-controller's APIExport
2. **network-provider** -- exports VPCs
3. **compute-provider** -- exports VirtualMachines, creates a DependencyRule
4. **consumer1** -- binds to all three, where test resources are created
5. **consumer2** -- binds to all three, used to verify no cross-workspace leakage

The test also starts an HTTPS webhook server with a self-signed CA, wires the
`WebhookInstaller` into the DependencyRule reconciler, and verifies the full
lifecycle:

- Dependency auto-creation when a VM references a VPC
- Webhook blocks VPC deletion while Dependencies exist
- Dependency cleanup when the VM is deleted
- Webhook allows VPC deletion after cleanup
- Webhook removal when the DependencyRule is deleted

### Fixtures

Test fixtures are loaded from YAML files rather than constructed inline:

- `config/kcp/` -- the generated dep-ctrl APIResourceSchemas and APIExport
  (same files used for real deployment)
- `test/fixtures/` -- test provider schemas (VPC, VirtualMachine) and their
  APIExports

## Deploying to kcp

### 1. Create the dep-ctrl workspace and apply schemas

```sh
kubectl ws create dep-ctrl --enter
kubectl apply -k config/kcp/
```

This creates the `APIResourceSchema` objects and the
`dependencies.opendefense.cloud` APIExport.

### 2. Run the controller

```sh
# Point kubeconfig at the dep-ctrl workspace
make run
```

Or with explicit flags:

```sh
bin/dependency-controller \
  --api-export-name=dependencies.opendefense.cloud \
  --kcp-base-host=https://kcp.example.com:6443
```

### 3. Provider setup

Each API provider that wants deletion protection:

1. Binds to the dep-ctrl APIExport in their provider workspace
2. Creates a `DependencyRule` declaring which of their resources depend on which
   other resources

The controller then automatically:

- Starts watching the dependent resource type via the provider's APIExport
- Installs a `ValidatingWebhookConfiguration` in each dependency provider's
  workspace
- Creates `Dependency` marker objects in consumer workspaces as resources are
  created
