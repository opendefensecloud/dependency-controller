# Getting Started

This guide walks through deploying the dependency-controller from scratch. It
assumes you have a running kcp instance and a Kubernetes cluster where the
controller and webhook will run (they connect to kcp remotely via kubeconfigs).

By the end, you'll have:
- The `DependencyRule` API available in kcp
- The controller and webhook running in your Kubernetes cluster
- A working example where deleting a VPC is blocked while a VirtualMachine
  references it

## Prerequisites

- A running [kcp](https://github.com/kcp-dev/kcp) instance
- A Kubernetes cluster (the "management cluster") with:
  - [cert-manager](https://cert-manager.io/) installed (for webhook TLS)
  - Network connectivity to kcp
- `kubectl` with the [kcp plugin](https://github.com/kcp-dev/kcp/tree/main/cli)
- `helm` v3

### Quick setup with kind and kcp

If you don't have a cluster yet:

```sh
# Create a kind cluster
kind create cluster --name dep-ctrl

# Install cert-manager
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.17.2/cert-manager.yaml
kubectl -n cert-manager wait deployment cert-manager-webhook --for=condition=Available --timeout=120s

# Create a self-signed ClusterIssuer (for dev/testing)
kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned
spec:
  selfSigned: {}
EOF
```

For kcp, follow the [kcp installation guide](https://docs.kcp.io/kcp/main/setup/install/)
or deploy it via its [Helm chart](https://github.com/kcp-dev/helm-charts).

## Overview

The deployment has four phases:

1. **kcp setup** -- create the dep-ctrl workspace, apply schemas and the
   APIExport
2. **Bootstrap RBAC** -- grant the controller and webhook identities the
   minimum permissions they need in kcp
3. **Helm install** -- deploy the controller and webhook into your Kubernetes
   cluster
4. **Provider onboarding** -- providers bind to the dep-ctrl APIExport and
   create DependencyRules

```
Management Cluster (kind)            kcp
+-------------------------------+    +----------------------------------+
| dependency-controller pod     |    | root workspace                   |
|   reads Workspace objects     |--->|   ClusterRoles for both SAs      |
|   manages webhooks+RBAC via VW|    |                                  |
|                               |    | root:dep-ctrl workspace          |
| dependency-webhook pod        |    |   APIExport: DependencyRule      |
|   watches rules via VW        |--->|   ClusterRoles for both SAs      |
|   serves admission requests   |    |                                  |
+-------------------------------+    | root:network-provider            |
                                     |   APIExport: VPCs                |
                                     |   ValidatingWebhook (installed   |
                                     |     by controller via VW)        |
                                     |                                  |
                                     | root:compute-provider            |
                                     |   APIExport: VMs                 |
                                     |   DependencyRule: VM -> VPC      |
                                     |   ClusterRole (installed by      |
                                     |     controller for webhook)      |
                                     +----------------------------------+
```

## Step 1: Create the dep-ctrl workspace and apply schemas

The dependency-controller manages its own API type (`DependencyRule`) via a kcp
APIExport. You need to create a workspace for it and apply the schema and export
definitions.

```sh
# Create the dep-ctrl workspace
kubectl ws root
kubectl ws create dep-ctrl --enter

# Apply the APIResourceSchema and APIExport
kubectl apply -f config/kcp/apiresourceschema-dependencyrules.dependencies.opendefense.cloud.yaml
kubectl apply -f config/kcp/apiexport-dependencies.opendefense.cloud.yaml
```

The APIExport
([`config/kcp/apiexport-dependencies.opendefense.cloud.yaml`](../config/kcp/apiexport-dependencies.opendefense.cloud.yaml))
declares `permissionClaims` for three resource types:

```yaml
spec:
  permissionClaims:
    - group: "admissionregistration.k8s.io"
      resource: "validatingwebhookconfigurations"
      verbs: ["get", "list", "watch", "create", "update", "delete"]
    - group: "rbac.authorization.k8s.io"
      resource: "clusterroles"
      verbs: ["get", "list", "watch", "create", "update", "delete"]
    - group: "rbac.authorization.k8s.io"
      resource: "clusterrolebindings"
      verbs: ["get", "list", "watch", "create", "update", "delete"]
```

**Why?** The controller needs to manage resources in provider workspaces that
bind to this APIExport. In kcp, you can't directly access another workspace's
resources -- instead, the APIExport's
[virtual workspace](https://docs.kcp.io/kcp/main/concepts/apis/virtual-workspaces/)
acts as a proxy. `permissionClaims` tell kcp which resource types the APIExport
provider is allowed to manage in binding workspaces via that proxy. Provider
workspaces must explicitly accept these claims when creating their APIBinding
(covered in [Step 5](#step-5-onboard-a-provider)).

## Step 2: Bootstrap RBAC in kcp

Both components run with dedicated service account identities. They need
specific permissions in two kcp workspaces, applied once using a privileged
identity (e.g., `system:masters` via a bootstrap certificate).

### Root workspace RBAC

Both components need `workspaces/content` access to enter child workspaces
(this is how kcp authorizes traversing the workspace hierarchy). The controller
additionally needs to read `Workspace` objects to resolve workspace paths
(like `root:network-provider`) to the logical cluster names that the virtual
workspace requires.

```sh
kubectl ws root
kubectl apply -f test/fixtures/root-rbac-bootstrap.yaml
```

The file
([`test/fixtures/root-rbac-bootstrap.yaml`](../test/fixtures/root-rbac-bootstrap.yaml))
creates:

| Resource | Name | Purpose |
|---|---|---|
| ClusterRole | `dependency-controller` | `workspaces/content` access + `workspaces` read (get/list/watch) |
| ClusterRoleBinding | `dependency-controller` | Binds to controller SA |
| ClusterRole | `dependency-controller-webhook` | `workspaces/content` access only |
| ClusterRoleBinding | `dependency-controller-webhook` | Binds to webhook SA |

**Why `workspaces/content`?** Without it, kcp rejects any request to a child
workspace's API, including virtual workspace endpoints. Both components access
the dep-ctrl workspace where their kubeconfig points.

**Why `workspaces` read for the controller?** The virtual workspace accepts
logical cluster names (opaque IDs like `qh6707jkfsen31z9`), not workspace paths
(`root:network-provider`). The controller reads `Workspace` objects from root
to map paths to cluster names via `workspace.spec.cluster`.

### Dep-ctrl workspace RBAC

Both components need access to the dep-ctrl APIExport's virtual workspace.
This is authorized by the `apiexports/content` subresource in the workspace
where the APIExport is defined.

```sh
kubectl ws root:dep-ctrl
kubectl apply -f test/fixtures/depctrl-rbac-bootstrap.yaml
```

The file
([`test/fixtures/depctrl-rbac-bootstrap.yaml`](../test/fixtures/depctrl-rbac-bootstrap.yaml))
creates:

| Resource | Name | Permissions |
|---|---|---|
| ClusterRole | `dependency-controller` | `apiexportendpointslices` read + `apiexports/content` full CRUD |
| ClusterRoleBinding | `dependency-controller` | Binds to controller SA |
| ClusterRole | `dependency-controller-webhook` | `apiexportendpointslices` read + `apiexports/content` read-only |
| ClusterRoleBinding | `dependency-controller-webhook` | Binds to webhook SA |

**Why `apiexportendpointslices`?** Both components discover the virtual
workspace URL by reading the `APIExportEndpointSlice` for the
`dependencies.opendefense.cloud` APIExport. This URL is the entry point for
all operations through the virtual workspace.

**Why `apiexports/content` with different permission levels?**
- The **controller** needs full CRUD because it writes resources (webhooks, RBAC)
  in binding workspaces through the virtual workspace. The verb on
  `apiexports/content` controls what operations are allowed through the VW --
  read-only access would block the controller from creating anything.
- The **webhook** only needs read access -- it watches `DependencyRule` objects
  through the VW but never writes.

### Adjusting service account names

The bootstrap RBAC files bind to these default identities:
- Controller: `system:serviceaccount:dependency-system:dependency-controller`
- Webhook: `system:serviceaccount:dependency-system:dependency-webhook`

If your deployment uses different service account names or namespaces, edit the
`subjects` in the `ClusterRoleBinding` resources before applying. The names must
match what you configure in the Helm values (Step 3).

## Step 3: Create kubeconfigs for the components

Each component needs a kubeconfig that authenticates as its service account
identity and targets the dep-ctrl workspace. How you create these depends on
your kcp setup:

- **Client certificates**: create certificates with the appropriate CN
  (e.g., `system:serviceaccount:dependency-system:dependency-controller`)
  signed by kcp's CA
- **Service account tokens**: if kcp supports token-based auth, create tokens
  bound to the service accounts

The kubeconfig server URL should point to kcp with the dep-ctrl workspace path:

```
https://<kcp-host>:6443/clusters/root:dep-ctrl
```

Store the kubeconfigs as Kubernetes secrets in the management cluster:

```sh
kubectl -n dependency-system create secret generic kcp-controller-kubeconfig \
  --from-file=kubeconfig=/path/to/controller.kubeconfig

kubectl -n dependency-system create secret generic kcp-webhook-kubeconfig \
  --from-file=kubeconfig=/path/to/webhook.kubeconfig
```

## Step 4: Deploy with Helm

The Helm chart deploys both the controller and webhook as separate Deployments
in a single release. The controller automatically discovers the webhook's
service URL and TLS CA from the co-deployed resources.

```sh
helm install dep-ctrl charts/dependency-controller \
  --namespace dependency-system --create-namespace \
  --set kcpBaseHost=https://<kcp-host>:6443 \
  --set controller.kubeconfig.secretName=kcp-controller-kubeconfig \
  --set webhook.kubeconfig.secretName=kcp-webhook-kubeconfig \
  --set webhook.tls.certManager.issuerRef.name=selfsigned
```

Key values:

| Value | Purpose |
|---|---|
| `kcpBaseHost` | Root kcp URL (no workspace path). Used by the controller to resolve workspace paths and by the webhook to discover APIExport VW URLs. |
| `controller.kubeconfig.secretName` | Secret containing the controller's kubeconfig (from Step 3). |
| `webhook.kubeconfig.secretName` | Secret containing the webhook's kubeconfig (from Step 3). |
| `webhook.tls.certManager.issuerRef.name` | cert-manager issuer for the webhook's TLS certificate. |

See [`charts/dependency-controller/values.yaml`](../charts/dependency-controller/values.yaml)
for all available options.

Verify both components are running:

```sh
kubectl -n dependency-system get pods
# NAME                                              READY   STATUS
# dep-ctrl-dependency-controller-...                1/1     Running
# dep-ctrl-dependency-controller-webhook-...        1/1     Running
```

The webhook pod's readiness probe only passes once it has populated its rule
registry (listed all existing DependencyRules). On first deploy with no rules,
this is near-instant.

## Step 5: Onboard a provider

This example uses two providers -- a network provider (exports VPCs) and a
compute provider (exports VirtualMachines). The compute provider will create a
DependencyRule saying "VMs depend on VPCs", which blocks VPC deletion while
any VM references it.

### 5a. Create provider workspaces and APIExports

```sh
# Network provider
kubectl ws root
kubectl ws create network-provider --enter
kubectl apply -f test/fixtures/apiresourceschema-vpcs.yaml
kubectl apply -f test/fixtures/apiexport-network.test.io.yaml

# Compute provider
kubectl ws root
kubectl ws create compute-provider --enter
kubectl apply -f test/fixtures/apiresourceschema-virtualmachines.yaml
kubectl apply -f test/fixtures/apiexport-compute.test.io.yaml
```

### 5b. Bind providers to the dep-ctrl APIExport

Each provider must bind to the dep-ctrl APIExport **and accept the
permissionClaims**. This is what authorizes the controller to manage webhooks
and RBAC in that provider's workspace through the virtual workspace.

Apply the binding in each provider workspace:

```sh
# In compute-provider
kubectl ws root:compute-provider
kubectl apply -f - <<'EOF'
apiVersion: apis.kcp.io/v1alpha2
kind: APIBinding
metadata:
  name: dependencies.opendefense.cloud
spec:
  reference:
    export:
      path: root:dep-ctrl
      name: dependencies.opendefense.cloud
  permissionClaims:
    - group: "admissionregistration.k8s.io"
      resource: "validatingwebhookconfigurations"
      verbs: ["get", "list", "watch", "create", "update", "delete"]
      selector:
        matchAll: true
      state: Accepted
    - group: "rbac.authorization.k8s.io"
      resource: "clusterroles"
      verbs: ["get", "list", "watch", "create", "update", "delete"]
      selector:
        matchAll: true
      state: Accepted
    - group: "rbac.authorization.k8s.io"
      resource: "clusterrolebindings"
      verbs: ["get", "list", "watch", "create", "update", "delete"]
      selector:
        matchAll: true
      state: Accepted
EOF
```

Repeat in the network-provider workspace (same YAML, apply with
`kubectl ws root:network-provider`).

A reference fixture is available at
[`test/fixtures/apibinding-dependencies.opendefense.cloud.yaml`](../test/fixtures/apibinding-dependencies.opendefense.cloud.yaml)
(replace `${DEP_CTRL_PATH}` with `root:dep-ctrl`).

**Why accept permissionClaims?** Without acceptance, the controller has no
access to the provider workspace through the virtual workspace. The claims
grant the controller permission to:
- Create `ValidatingWebhookConfigurations` (to install deletion protection)
- Create `ClusterRoles` and `ClusterRoleBindings` (to grant the webhook read
  access to the provider's APIExport virtual workspace)

### 5c. Create a DependencyRule

The compute provider declares that VirtualMachines depend on VPCs:

```sh
kubectl ws root:compute-provider
kubectl apply -f - <<'EOF'
apiVersion: dependencies.opendefense.cloud/v1alpha1
kind: DependencyRule
metadata:
  name: vm-dependencies
spec:
  dependent:
    apiExportRef:
      path: root:compute-provider
      name: compute.test.io
    group: compute.test.io
    version: v1
    kind: VirtualMachine
    resource: virtualmachines
  dependencies:
    - apiExportRef:
        path: root:network-provider
        name: network.test.io
      group: network.test.io
      version: v1
      resource: vpcs
      fieldRef:
        path: ".spec.vpcRef.name"
EOF
```

A reference fixture is available at
[`test/fixtures/dependencyrule-vm-dependencies.yaml`](../test/fixtures/dependencyrule-vm-dependencies.yaml).

Once applied, the controller will:
1. Install a `ValidatingWebhookConfiguration` in `root:network-provider`
   (protecting VPC deletions)
2. Create RBAC in `root:compute-provider` (granting the webhook read access to
   VirtualMachines via the compute APIExport's virtual workspace)

The webhook will:
1. Start an indexed cache watching VirtualMachines via the compute APIExport VW
2. Begin serving admission requests for VPC deletions

### 5d. Create a consumer workspace and test resources

```sh
kubectl ws root
kubectl ws create consumer1 --enter

# Bind to both providers
kubectl apply -f - <<'EOF'
apiVersion: apis.kcp.io/v1alpha2
kind: APIBinding
metadata:
  name: network.test.io
spec:
  reference:
    export:
      path: root:network-provider
      name: network.test.io
---
apiVersion: apis.kcp.io/v1alpha2
kind: APIBinding
metadata:
  name: compute.test.io
spec:
  reference:
    export:
      path: root:compute-provider
      name: compute.test.io
EOF

# Wait for bindings
kubectl get apibindings

# Create a VPC
kubectl apply -f - <<'EOF'
apiVersion: network.test.io/v1
kind: VPC
metadata:
  name: my-vpc
  namespace: default
spec:
  cidr: "10.0.0.0/16"
EOF

# Create a VM that references the VPC
kubectl apply -f - <<'EOF'
apiVersion: compute.test.io/v1
kind: VirtualMachine
metadata:
  name: my-vm
  namespace: default
spec:
  cpu: 4
  vpcRef:
    name: my-vpc
EOF
```

### 5e. Verify deletion protection

```sh
# This should be denied:
kubectl delete vpc my-vpc -n default
# Error: admission webhook "dependency-controller.dependencies.opendefense.cloud"
# denied the request: still referenced by VirtualMachine/my-vm

# Delete the VM first, then the VPC:
kubectl delete virtualmachine my-vm -n default
kubectl delete vpc my-vpc -n default
# VPC deletion succeeds
```

## How the pieces fit together

Here's the flow that makes Step 5e work:

1. The **controller** watches DependencyRules via the dep-ctrl APIExport's
   virtual workspace
2. When it sees the `vm-dependencies` rule, it resolves `root:network-provider`
   to a logical cluster name by reading the `Workspace` object from root
3. It connects to the dep-ctrl VW at
   `<vw-url>/clusters/<logical-cluster-name>` and creates a
   `ValidatingWebhookConfiguration` in the network-provider workspace
   (authorized by the accepted `validatingwebhookconfigurations` permissionClaim)
4. It also creates a `ClusterRole` + `ClusterRoleBinding` in compute-provider
   (via the VW) granting the webhook `apiexports/content` on `compute.test.io`
5. The **webhook** also watches the same DependencyRule and starts an indexed
   cache watching VirtualMachines via compute.test.io's VW (authorized by the
   RBAC from step 4)
6. When a consumer deletes a VPC, kcp dispatches the DELETE to the webhook
   (via the installed `ValidatingWebhookConfiguration`)
7. The webhook queries its indexed cache: "any VMs where `.spec.vpcRef.name`
   equals `my-vpc`?" -- finds `my-vm` and denies the deletion

## Troubleshooting

### Webhook pod not becoming ready

The webhook's readiness probe fails until it has listed all existing
DependencyRules. Check the webhook logs:

```sh
kubectl -n dependency-system logs -l app.kubernetes.io/component=webhook
```

Common issues:
- **Kubeconfig invalid** -- the webhook can't reach kcp
- **Missing dep-ctrl RBAC** -- the webhook SA needs `apiexports/content` read
  access in the dep-ctrl workspace (Step 2)
- **Missing root RBAC** -- the webhook SA needs `workspaces/content` in root
  (Step 2)

### Webhook not blocking deletions

Check that all pieces are in place:

```sh
# Is the ValidatingWebhookConfiguration installed?
kubectl ws root:network-provider
kubectl get validatingwebhookconfiguration dependency-controller

# Is the RBAC in place for the webhook?
kubectl ws root:compute-provider
kubectl get clusterrole dependency-controller-webhook
kubectl get clusterrolebinding dependency-controller-webhook

# Are the DependencyRule bindings bound?
kubectl ws root:compute-provider
kubectl get apibinding dependencies.opendefense.cloud -o jsonpath='{.status.phase}'
# Should be: Bound
```

### Controller can't create webhooks or RBAC

Check that the provider workspace's APIBinding has accepted the
permissionClaims (Step 5b). Without acceptance, the VW rejects write
operations:

```sh
kubectl ws root:network-provider
kubectl get apibinding dependencies.opendefense.cloud -o yaml
# Look for permissionClaims with state: Accepted
```

### Force-deleting a protected resource

If the webhook is down or caches are stale, annotate the resource to bypass
protection:

```sh
kubectl annotate vpc my-vpc dependencies.opendefense.cloud/skip-protection=true
kubectl delete vpc my-vpc
```
