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

The e2e tests (`test/e2e/`) run against a real kind cluster with kcp and
cert-manager deployed via Helm. They build the Docker image, load it into
kind, deploy the Helm chart (which includes both controller and webhook), and
exercise the full system including TLS webhook dispatch through kcp's admission
pipeline.

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
`dependencies.opendefense.cloud` APIExport. The APIExport includes a
`permissionClaim` for `validatingwebhookconfigurations` -- this authorizes the
controller to manage webhooks in provider workspaces through the virtual workspace.

### 2. Apply bootstrap RBAC

Bootstrap RBAC is required in three locations, applied with a privileged identity:

**Root workspace** -- both components need `workspaces/content` access to enter
child workspaces. The controller also needs `workspaces` read access for
workspace path resolution:

```sh
kubectl ws root
kubectl apply -f test/fixtures/root-rbac-bootstrap.yaml
```

**Dep-ctrl workspace** -- the controller needs full CRUD on `apiexports/content`
(to manage claimed resources through the VW) and `apiexportendpointslices` read
access for VW URL discovery:

```sh
kubectl ws root:dep-ctrl
kubectl apply -f test/fixtures/depctrl-rbac-bootstrap.yaml
```

**system:admin** (shard-local) -- the webhook SA gets shard-wide read access to
`apiexports/content` and `apiexportendpointslices`, evaluated by the Bootstrap
Policy Authorizer for every request on the shard. Must be applied via the kcp
server (not front-proxy):

```sh
kubectl --server=https://<kcp-server>:6443/clusters/system:admin \
  apply -f test/fixtures/shard-admin-rbac-bootstrap.yaml
```

Adjust the service account names in the `ClusterRoleBinding` subjects to match
your deployment (defaults: `system:serviceaccount:dependency-system:dependency-controller`
and `system:serviceaccount:dependency-system:dependency-webhook`).

### 3. Deploy with Helm

The chart deploys both the controller and webhook server as separate Deployments:

```sh
helm install dep-ctrl charts/dependency-controller \
  --namespace dependency-system --create-namespace \
  --set kcpBaseHost=https://kcp.example.com:6443 \
  --set controller.kubeconfig.secretName=kcp-controller-kubeconfig \
  --set webhook.kubeconfig.secretName=kcp-webhook-kubeconfig \
  --set webhook.tls.certManager.issuerRef.name=my-issuer
```

The controller automatically discovers the webhook service URL and CA bundle
from the co-deployed webhook resources. It routes all provider workspace
operations through the dep-ctrl APIExport's virtual workspace, resolving
workspace paths to logical cluster names via root Workspace objects.

### 4. Provider setup

Each API provider that wants deletion protection:

1. Binds to the dep-ctrl APIExport in their provider workspace, **accepting the
   permissionClaim** (required for the controller to install webhooks):
   ```yaml
   spec:
     permissionClaims:
       - group: "admissionregistration.k8s.io"
         resource: "validatingwebhookconfigurations"
         selector: { matchAll: true }
         state: Accepted
   ```
2. Creates a `DependencyRule` declaring which of their resources depend on which
   other resources

The controller then automatically installs a `ValidatingWebhookConfiguration`
in each dependency provider's workspace (via the VW). The webhook server picks
up the rule, starts an indexed cache for the dependent resource type (authorized
by the shard-wide system:admin RBAC), and begins serving admission requests.
