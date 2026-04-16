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
  dependency-controller/    Helm chart for the controller
  dependency-webhook/       Helm chart for the webhook server
config/
  crds/                     Generated CRDs (intermediate, from controller-gen)
  kcp/                      Generated APIResourceSchemas + APIExport (from apigen)
docs/                       Documentation
internal/
  controller/
    dependencyrule_controller.go   Multicluster DependencyRule reconciler
    webhook_installer.go           Manages ValidatingWebhookConfigurations
    rbac_manager.go                Manages ClusterRole/Binding in system:master
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
make helm-package     # Package Helm charts
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

The e2e tests (`test/e2e/`) run against a real kind cluster with kcp and
cert-manager deployed via Helm. They build the Docker image, load it into
kind, deploy the Helm charts, and exercise the full system including TLS
webhook dispatch through kcp's admission pipeline.

Tool paths can be configured via environment variables (`KIND`, `KUBECTL`,
`HELM`, `DOCKER`) with fallback to PATH lookup.

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

### 2. Apply bootstrap RBAC in root workspace

The webhook service account needs `workspaces/content` access in the root workspace.
Apply this once with a privileged identity:

```sh
kubectl ws root
kubectl apply -f test/fixtures/root-rbac-bootstrap.yaml
```

Adjust the service account name in the `ClusterRoleBinding` subject to match your
deployment's webhook service account (default: `system:serviceaccount:dependency-system:dependency-webhook`).

### 3. Run the controller

```sh
bin/dependency-controller \
  --api-export-name=dependencies.opendefense.cloud \
  --kcp-base-host=https://kcp.example.com:6443 \
  --webhook-url=https://dependency-webhook.ns.svc:443/validate \
  --webhook-ca-bundle-path=/path/to/ca.pem \
  --webhook-service-account-name=dependency-webhook \
  --webhook-service-account-namespace=dependency-system
```

The controller will dynamically create `apiexports/content` RBAC in each provider
workspace as `DependencyRule` objects are created.

### 4. Run the webhook server

```sh
bin/dependency-webhook \
  --api-export-name=dependencies.opendefense.cloud \
  --kcp-base-host=https://kcp.example.com:6443 \
  --tls-cert-dir=/etc/webhook-tls \
  --webhook-port=9443
```

### 5. Provider setup

Each API provider that wants deletion protection:

1. Binds to the dep-ctrl APIExport in their provider workspace
2. Creates a `DependencyRule` declaring which of their resources depend on which
   other resources

The controller then automatically installs a `ValidatingWebhookConfiguration`
in each dependency provider's workspace and creates `apiexports/content` RBAC in
the dependent's provider workspace. The webhook server picks up the rule, starts
an indexed cache for the dependent resource type, and begins serving admission
requests.
