# Development

## Prerequisites

- Go 1.26+
- A kcp binary (downloaded automatically by `make kcp`)

## Project Structure

```
api/v1alpha1/               Go types for DependencyRule
cmd/
  controller/               Controller entrypoint
  webhook/                  Webhook server entrypoint
charts/
  dependency-controller/    Helm chart (deploys both controller and webhook)
config/
  crds/                     Generated CRDs (intermediate, from controller-gen)
  kcp/                      Generated APIResourceSchemas + APIExport (from apigen)
docs/                       Documentation
internal/
  controller/
    dependencyrule_controller.go   Reconciler + workspace resolver (VW routing)
    webhook_installer.go           Manages ValidatingWebhookConfigurations
  fieldpath/
    fieldpath.go                   Dot-notation field path resolver
  webhook/
    rule_cache_manager.go          Per-rule indexed cache lifecycle manager
    rule_registry.go               Thread-safe registry of rule caches
    deletion_validator.go          Admission webhook handler
test/
  e2e/                  End-to-end tests (kind + kcp + helm)
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
make build            # Build both binaries to bin/
make docker-build     # Build Docker image
make helm-package     # Package Helm chart
```

### Run

```sh
make run-controller   # Run the controller from source
make run-webhook      # Run the webhook server from source
```

### Test

```sh
make test             # Unit + integration tests (requires kcp binary, excludes e2e)
make test-e2e         # E2E tests (requires kind, helm, docker)
make clean-e2e        # Remove kind cluster from e2e tests
```

### Lint & Format

```sh
make fmt    # Add license headers, format code, run lint --fix
make lint   # Run golangci-lint
make vet    # Run go vet
```

## Integration Tests

The integration tests (`internal/controller/integration_test.go`) use
[kcp envtest](https://github.com/kcp-dev/multicluster-provider/tree/main/envtest)
to spin up a real kcp instance in-process and create a 5-workspace topology:

1. **dep-ctrl** -- hosts the DependencyRule APIExport
2. **network-provider** -- exports VPCs
3. **compute-provider** -- exports VirtualMachines, creates a DependencyRule
4. **consumer1** -- binds to all three, where test resources are created
5. **consumer2** -- binds to all three, used to verify no cross-workspace leakage

The test starts both the controller reconciler (with `WebhookInstaller`) and the
webhook's `RuleCacheManager` on the same multicluster manager, plus an HTTPS
webhook server with a self-signed CA. It verifies the full lifecycle:

- Webhook blocks VPC deletion while VMs reference it
- Consumer2's VPC is unaffected (cross-workspace isolation)
- Webhook allows VPC deletion after the VM is deleted
- Webhook removal when the DependencyRule is deleted

## E2E Tests

The e2e tests (`test/e2e/`) run against a real kind cluster with a multi-shard
kcp instance deployed via the
[kcp-operator](https://github.com/kcp-dev/helm-charts). The test suite:

1. Creates a kind cluster with a NodePort for the kcp front-proxy
2. Installs cert-manager and the kcp-operator Helm chart
3. Deploys two etcd instances and creates a `RootShard`, `Shard` (shard1), and
   `FrontProxy` via kcp-operator CRs
4. Generates admin and component kubeconfigs via kcp-operator `Kubeconfig` CRs
   (using `rootShardRef` so certs are trusted by both front-proxy and shards)
5. Builds the Docker image, loads it into kind, and deploys via Helm
6. Exercises the full system including TLS webhook dispatch through kcp's
   admission pipeline

The test creates a 5-workspace topology:
- **dep-ctrl** -- hosts the DependencyRule APIExport
- **network-provider** -- exports VPCs
- **compute-provider** -- exports VirtualMachines, creates a DependencyRule
- **consumer1** -- on the root shard, binds to all providers
- **consumer2** -- pinned to shard1 via location selector, verifies multi-shard

Shard-wide bootstrap RBAC is applied to both shards via port-forward to each
shard's Service, targeting `system:admin` with a `system:masters` kubeconfig.

Tool paths can be configured via environment variables (`KIND`, `KUBECTL`,
`HELM`, `DOCKER`) with fallback to PATH lookup.

### Fixtures

Test fixtures are loaded from YAML files rather than constructed inline:

- `config/kcp/` -- the generated dep-ctrl APIResourceSchemas and APIExport
  (same files used for real deployment)
- `test/fixtures/` -- test provider schemas (VPC, VirtualMachine) and their
  APIExports

## Deploying to kcp

See [docs/getting-started.md](getting-started.md) for the full step-by-step
deployment guide using kcp-operator. The guide covers kcp-operator setup,
multi-shard configuration, bootstrap RBAC, kubeconfig generation via
kcp-operator Kubeconfig CRs, and Helm deployment.

### Quick reference

1. Deploy kcp via kcp-operator (RootShard, FrontProxy, optional additional Shards)
2. Create the dep-ctrl workspace and apply `config/kcp/` schemas
3. Apply bootstrap RBAC in three locations: root, dep-ctrl, and system:admin (per shard)
4. Generate component kubeconfigs via kcp-operator Kubeconfig CRs (use `rootShardRef`)
5. Deploy with Helm
6. Providers bind to the dep-ctrl APIExport (accepting permissionClaims) and create DependencyRules
