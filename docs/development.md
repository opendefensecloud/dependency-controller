# Development

## Prerequisites

The recommended setup is the [dev shell](#dev-shell) below — it provides the
full toolchain. If you're not using Nix, you need:

- Go 1.26+
- A kcp binary (downloaded automatically by `make kcp`)
- `golangci-lint`, `helm`, `kind`, `docker`, and `pre-commit` on `$PATH`

## Dev shell

The repo ships a [Nix flake](../flake.nix) wired up via [direnv](https://direnv.net/)
(see [.envrc](../.envrc)). With both installed, the dev shell auto-loads on `cd`
and provides Go 1.26.2, `golangci-lint`, `gopls`, `helm`, `kind`, `task`, the kcp
toolchain, and the rest of the dependencies.

```sh
direnv allow            # one-time, on first entry
# or, without direnv:
nix develop
```

The shell is defined by [`opendefensecloud/dev-kit`](https://github.com/opendefensecloud/dev-kit)
via the `dev-kit` flake input — adding tools project-wide is a PR there, not here.

## Pre-commit hooks

```sh
pre-commit install      # registers the hooks listed in .pre-commit-config.yaml
```

The configured hooks cover trailing whitespace, YAML/JSON syntax, `yamllint`,
`shellcheck`, `gofmt`, `go vet`, `go mod tidy`, `golangci-lint` (manual stage),
and `helm lint`.

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
make test               # Unit + integration tests (requires kcp binary, excludes e2e)
make test-e2e           # E2E tests (requires kind, helm, docker) — uses the active shard config
make test-e2e-matrix    # Run e2e tests against both shard configs sequentially
make clean-e2e          # Remove kind cluster from e2e tests
```

`make test-e2e` honors the `E2E_SHARD_CONFIG` environment variable
(`single-shard` or `multi-shard`, default `multi-shard`); see the [E2E Tests](#e2e-tests)
section for what each config exercises. `make test-e2e-matrix` runs both back-to-back
with a kind cleanup between runs.

Tool paths can be overridden via `KIND`, `KUBECTL`, `HELM`, `DOCKER` env vars
(fallback: PATH lookup). Set `E2E_SKIP_CLEANUP=1` to keep the kind cluster
running after the suite for inspection.

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
3. Deploys two etcd instances and creates a `RootShard`, `Shard` (`shard1`), and
   `FrontProxy` via kcp-operator CRs
4. Generates admin and component kubeconfigs via kcp-operator `Kubeconfig` CRs
   (using `rootShardRef` so certs are trusted by both front-proxy and shards)
5. Bootstraps RBAC (see below)
6. Builds the Docker image, loads it into kind, and deploys via Helm
7. Exercises the full system including TLS webhook dispatch through kcp's
   admission pipeline

### Workspace topology and shard placement

The five test workspaces (`dep-ctrl`, `network-provider`, `compute-provider`,
`consumer1`, `consumer2`) are pinned deterministically to either `root` or
`shard1` via `spec.location.selector`. Both shards carry an `e2e-target=<name>`
label. After workspace readiness, `verifyShardPlacements` reads each
workspace's `internal.tenancy.kcp.io/shard` annotation and asserts that
selectors weren't ignored.

Selection is driven by the `E2E_SHARD_CONFIG` env var:

| Config           | Purpose                                  | Placements                                                                                                  |
|------------------|------------------------------------------|-------------------------------------------------------------------------------------------------------------|
| `single-shard`   | sanity / single-shard fast paths         | all five workspaces on `root`                                                                                |
| `multi-shard` (default) | exercises every cross-shard path  | `compute-provider` and `consumer2` on `shard1`; `dep-ctrl`, `network-provider`, `consumer1` on `root` |

Together, the two configs exercise: same-shard fast paths (single-shard);
controller-VW cross-shard webhook installation, provider ↔ consumer
cross-shard binding, and webhook ↔ consumer cross-shard query (multi-shard).
Run `make test-e2e-matrix` to execute both sequentially.

### Bootstrap RBAC

Three RBAC bundles are applied during setup:

1. **`system:admin` per shard** (`test/fixtures/system-admin-rbac-bootstrap.yaml`):
   ClusterRole + ClusterRoleBinding granting the webhook SA `*/*` get/list.
   Applied via direct (port-forwarded) connection to each shard, using a
   `system:masters` kubeconfig generated from a kcp-operator `Kubeconfig` CR
   with `rootShardRef` / `shardRef`. kcp's `BootstrapPolicyAuthorizer` reads
   this binding from the local shard only — bindings do not propagate across
   shards — so the fixture is applied once per shard.
2. **Root workspace** (`test/fixtures/root-rbac-bootstrap.yaml`): controller-only
   rules (workspaces/content access + tenancy.kcp.io/workspaces read), applied
   via the front-proxy.
3. **Dep-ctrl workspace** (`test/fixtures/depctrl-rbac-bootstrap.yaml`):
   apiexportendpointslices read + dep-ctrl APIExport content access for both
   the controller and webhook SAs.

The webhook installation in provider workspaces is authorized by the
`validatingwebhookconfigurations` permissionClaim on the dep-ctrl APIExport,
not by RBAC.

### Test scenarios

`test/e2e/dependency_test.go` is an `Ordered` `Describe` covering:

- Initial install (DependencyRule → ValidatingWebhookConfiguration appears)
- Block / unblock cycles with a single dependent and with multiple dependents
- Cross-shard isolation (`consumer2` with no VMs)
- Cross-shard protection (`consumer2` with a VM, on `shard1` under multi-shard)
- `skip-protection` annotation
- DependencyRule deletion → webhook removal
- DependencyRule recreation → protection restored
- **In-place rule update**: patches `fieldRef.path` on a live rule and
  verifies the webhook re-evaluates without a recreate cycle (proves
  `WebhookInstaller.reconcileWorkspaceWebhook`'s update branch and the
  webhook's `RuleCacheManager` re-indexing both work end-to-end).

### Fixtures

Test fixtures are loaded from YAML files rather than constructed inline:

- `config/kcp/` -- the generated dep-ctrl APIResourceSchemas and APIExport
  (same files used for real deployment)
- `test/fixtures/` -- test provider schemas (VPC, VirtualMachine), APIExports,
  RBAC bundles, and per-test VPC/VM resources

## Deploying to kcp

See [docs/getting-started.md](getting-started.md) for the full step-by-step
deployment guide using kcp-operator. The guide covers kcp-operator setup,
multi-shard configuration, bootstrap RBAC, kubeconfig generation via
kcp-operator Kubeconfig CRs, and Helm deployment.

### Quick reference

1. Deploy kcp via kcp-operator (RootShard, FrontProxy, optional additional Shards)
2. Create the dep-ctrl workspace and apply `config/kcp/` schemas
3. Apply bootstrap RBAC: per-shard `system:admin` (webhook get/list), root
   workspace (controller), dep-ctrl workspace (APIExport access for both)
4. Generate component kubeconfigs via kcp-operator Kubeconfig CRs (use `rootShardRef`)
5. Deploy with Helm
6. Providers bind to the dep-ctrl APIExport (accepting permissionClaims) and create DependencyRules
