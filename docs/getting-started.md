# Getting Started

This guide walks through deploying the dependency-controller on a Kubernetes
cluster with kcp managed by the
[kcp-operator](https://github.com/kcp-dev/helm-charts). By the end you will
have:

- A multi-shard kcp instance (root shard + one additional shard)
- The `DependencyRule` API available in kcp
- The controller and webhook running in your Kubernetes cluster
- A working example where deleting a VPC is blocked while a VirtualMachine
  references it

## Prerequisites

- A Kubernetes cluster (the "management cluster") -- [kind](https://kind.sigs.k8s.io/) works well for dev
- [cert-manager](https://cert-manager.io/) installed in the cluster
- `kubectl` with the [kcp plugin](https://github.com/kcp-dev/kcp/tree/main/cli)
- `helm` v3

### Quick setup with kind

```sh
# Create a kind cluster (expose a NodePort for the kcp front-proxy)
cat <<'EOF' | kind create cluster --name dep-ctrl --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 31443
        hostPort: 31443
        protocol: TCP
EOF

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

## Overview

The deployment has six phases:

1. **Deploy kcp** -- install kcp-operator, etcd, and create the kcp shards and
   front-proxy
2. **kcp workspace setup** -- create the dep-ctrl workspace, apply schemas and
   the APIExport
3. **Bootstrap RBAC** -- grant the controller and webhook identities the minimum
   permissions they need in kcp
4. **Create kubeconfigs** -- generate client certificates for the controller and
   webhook via kcp-operator Kubeconfig CRs
5. **Helm install** -- deploy the controller and webhook into your Kubernetes
   cluster
6. **Provider onboarding** -- providers bind to the dep-ctrl APIExport and
   create DependencyRules

```text
Management Cluster (kind)            kcp (via kcp-operator)
+-------------------------------+    +----------------------------------+
| kcp-operator                  |    | root shard                       |
| etcd (one per shard)          |    |   system:admin (per-shard)       |
| root-kcp (root shard pod)     |    |     ClusterRoleBinding for       |
| shard1-kcp (shard1 pod)       |    |       webhook SA (wildcard read) |
| kcp-front-proxy               |    |   root workspace                 |
|                               |    |     ClusterRole for controller   |
| dependency-controller pod     |    |   root:dep-ctrl workspace        |
|   reads Workspace objects     |--->|     APIExport: DependencyRule    |
|   manages webhooks via VW     |    |     ClusterRoles for both SAs    |
|                               |    |   root:compute-provider          |
| dependency-webhook pod        |    |     APIExport: VMs               |
|   watches rules via VW        |    |     DependencyRule: VM -> VPC    |
|   serves admission requests   |--->|   root:network-provider          |
+-------------------------------+    |     APIExport: VPCs              |
                                     |     ValidatingWebhook (installed |
                                     |       by controller via VW)      |
                                     |                                  |
                                     | shard1                           |
                                     |   system:admin (per-shard)       |
                                     |     same webhook binding as root |
                                     |   (consumer workspaces may live  |
                                     |    on any shard)                 |
                                     +----------------------------------+
```

## Step 1: Deploy kcp via kcp-operator

### 1a. Install the kcp-operator

```sh
helm repo add kcp https://kcp-dev.github.io/helm-charts
helm repo update kcp

helm install kcp-operator kcp/kcp-operator \
  --namespace kcp-system --create-namespace \
  --wait --timeout 300s
```

### 1b. Deploy etcd

Each kcp shard needs its own etcd instance. For production, use a proper etcd
cluster; for dev/testing a single-replica StatefulSet per shard is sufficient.
You need a `Service` (client port 2379) and a `StatefulSet` per shard.

For a working example, see the `applyEtcd()` function in
[`test/e2e/suite_test.go`](../test/e2e/suite_test.go) which creates minimal
single-node etcd instances.

```sh
# Create etcd for the root shard (Service + StatefulSet named "etcd-root")
# Create etcd for the secondary shard ("etcd-shard1", optional for multi-shard)

# Wait for etcd to be ready
kubectl -n kcp-system wait statefulset etcd-root \
  --for=jsonpath='{.status.readyReplicas}'=1 --timeout=120s
```

### 1c. Create a cert-manager Issuer

kcp-operator uses cert-manager for PKI (server certs, client CAs, etc.):

```sh
kubectl -n kcp-system apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: selfsigned
spec:
  selfSigned: {}
EOF
```

### 1d. Create the RootShard, FrontProxy, and (optional) additional Shards

```sh
# Root shard
kubectl apply -f - <<'EOF'
apiVersion: operator.kcp.io/v1alpha1
kind: RootShard
metadata:
  name: root
  namespace: kcp-system
spec:
  external:
    hostname: kcp-front-proxy.kcp-system.svc.cluster.local
    port: 6443
  certificates:
    issuerRef:
      group: cert-manager.io
      kind: Issuer
      name: selfsigned
  cache:
    embedded:
      enabled: true
  etcd:
    endpoints:
      - http://etcd-root.kcp-system.svc.cluster.local:2379
  auth:
    serviceAccount:
      enabled: true
EOF

# Front-proxy (routes requests to the correct shard)
kubectl apply -f - <<'EOF'
apiVersion: operator.kcp.io/v1alpha1
kind: FrontProxy
metadata:
  name: kcp
  namespace: kcp-system
spec:
  rootShard:
    ref:
      name: root
  auth:
    serviceAccount:
      enabled: true
  serviceTemplate:
    spec:
      type: NodePort
EOF

# Optional: secondary shard for multi-shard setups
kubectl apply -f - <<'EOF'
apiVersion: operator.kcp.io/v1alpha1
kind: Shard
metadata:
  name: shard1
  namespace: kcp-system
spec:
  rootShard:
    ref:
      name: root
  etcd:
    endpoints:
      - http://etcd-shard1.kcp-system.svc.cluster.local:2379
  auth:
    serviceAccount:
      enabled: true
EOF

# Wait for all components
kubectl -n kcp-system wait rootshard root \
  --for=jsonpath='{.status.phase}'=Running --timeout=180s
kubectl -n kcp-system wait frontproxy kcp \
  --for=jsonpath='{.status.phase}'=Running --timeout=120s
```

### 1e. Create an admin kubeconfig

Use a kcp-operator `Kubeconfig` CR to generate a kubeconfig with admin access:

```sh
kubectl apply -f - <<'EOF'
apiVersion: operator.kcp.io/v1alpha1
kind: Kubeconfig
metadata:
  name: kcp-admin
  namespace: kcp-system
spec:
  username: kcp-admin
  groups:
    - "system:kcp:admin"
  validity: 8766h
  secretRef:
    name: kcp-admin-kubeconfig
  target:
    frontProxyRef:
      name: kcp
EOF

# Wait for the kubeconfig secret
kubectl -n kcp-system wait secret kcp-admin-kubeconfig --for=jsonpath='{.data.kubeconfig}' --timeout=120s

# Extract it (rewrite the server URL if accessing from outside the cluster)
kubectl -n kcp-system get secret kcp-admin-kubeconfig \
  -o jsonpath='{.data.kubeconfig}' | base64 -d > /tmp/kcp-admin.kubeconfig
```

If you're using kind with a NodePort, rewrite the server URL in the kubeconfig
to `https://localhost:<nodePort>`. The kcp-operator generates two contexts:
`base` (bare URL) and `default` (URL + `/clusters/root`). Use `base` if you
want to specify workspace paths manually via `--server`.

## Step 2: Create the dep-ctrl workspace and apply schemas

```sh
export KUBECONFIG=/tmp/kcp-admin.kubeconfig

# Create the dep-ctrl workspace
kubectl ws root
kubectl ws create dep-ctrl --enter

# Apply the APIResourceSchema and APIExport
kubectl apply -f config/kcp/apiresourceschema-dependencyrules.dependencies.opendefense.cloud.yaml
kubectl apply -f config/kcp/apiexport-dependencies.opendefense.cloud.yaml
```

The APIExport
([`config/kcp/apiexport-dependencies.opendefense.cloud.yaml`](../config/kcp/apiexport-dependencies.opendefense.cloud.yaml))
declares a `permissionClaim` for webhook configurations:

```yaml
spec:
  permissionClaims:
    - group: "admissionregistration.k8s.io"
      resource: "validatingwebhookconfigurations"
      verbs: ["get", "list", "watch", "create", "update", "delete"]
```

**Why?** The controller needs to install `ValidatingWebhookConfigurations` in
provider workspaces that bind to this APIExport. In kcp, you can't directly
access another workspace's resources -- instead, the APIExport's
[virtual workspace](https://docs.kcp.io/kcp/main/concepts/apis/virtual-workspaces/)
acts as a proxy. `permissionClaims` tell kcp which resource types the APIExport
provider is allowed to manage in binding workspaces via that proxy. Provider
workspaces must explicitly accept these claims when creating their APIBinding
(covered in [Step 6](#step-6-onboard-a-provider)).

## Step 3: Bootstrap RBAC in kcp

Both components run with dedicated service account identities. They need
specific permissions in several kcp locations, applied once using the admin
kubeconfig.

### Root workspace RBAC

The **controller** needs `workspaces/content` access to enter child workspaces
(this is how kcp authorizes traversing the workspace hierarchy) and
`workspaces` read access to resolve paths like `root:network-provider` to the
logical cluster names the virtual workspace requires.

```sh
kubectl ws root
kubectl apply -f test/fixtures/root-rbac-bootstrap.yaml
```

(The webhook does **not** get RBAC here — its broad read is granted in
`system:admin` per shard, see below.)

### Dep-ctrl workspace RBAC

Both components need access to the dep-ctrl APIExport's virtual workspace and
`apiexportendpointslices` for VW URL discovery:

```sh
kubectl ws root:dep-ctrl
kubectl apply -f test/fixtures/depctrl-rbac-bootstrap.yaml
```

The file
([`test/fixtures/depctrl-rbac-bootstrap.yaml`](../test/fixtures/depctrl-rbac-bootstrap.yaml))
grants both the controller and webhook SAs:

- `apiexportendpointslices` read access (for VW URL discovery at startup)
- `apiexports/content` full CRUD (for managing resources through the VW)

### Webhook RBAC in `system:admin` (per shard)

The webhook queries dependent resources directly in each consumer workspace,
and consumer workspaces can live on any shard. kcp's
`BootstrapPolicyAuthorizer` reads RBAC from the local shard's `system:admin`
logical cluster only — bindings do **not** propagate across shards — so the
webhook needs a `ClusterRole` + `ClusterRoleBinding` in `system:admin` of
**every shard** that hosts consumer workspaces.

`system:admin` is intentionally not reachable through the front-proxy. To
apply RBAC there you need:

1. A kubeconfig issued via a kcp-operator `Kubeconfig` CR with
   `target.rootShardRef` (or `target.shardRef` for secondary shards) and
   `groups: ["system:masters"]` — `system:masters` is honored by the shard
   directly but not by the front-proxy.
2. Direct shard access. The shard's ClusterIP service is normally not exposed;
   `kubectl port-forward svc/<shard-svc>` to localhost is the simplest path.
   Make sure the shard's server certificate has `localhost` / `127.0.0.1` in
   its SANs (kcp-operator's `RootShard` / `Shard` `certificateTemplates`).

Example (root shard):

```sh
# 1. Generate a system:masters kubeconfig for the root shard
kubectl apply -f - <<'EOF'
apiVersion: operator.kcp.io/v1alpha1
kind: Kubeconfig
metadata:
  name: root-system-masters
  namespace: kcp-system
spec:
  username: bootstrap-system-masters
  groups:
    - "system:masters"
  validity: 8766h
  secretRef:
    name: root-system-masters-kubeconfig
  target:
    rootShardRef:
      name: root
EOF

kubectl -n kcp-system wait secret root-system-masters-kubeconfig \
  --for=jsonpath='{.data.kubeconfig}' --timeout=120s

kubectl -n kcp-system get secret root-system-masters-kubeconfig \
  -o jsonpath='{.data.kubeconfig}' | base64 -d > /tmp/root-masters.kubeconfig
# Rewrite the server URL in /tmp/root-masters.kubeconfig to https://localhost:6443
# (or any free port you pick for the port-forward).

# 2. Port-forward the root shard service
kubectl -n kcp-system port-forward svc/root-kcp 6443:6443 &
PF_PID=$!

# 3. Apply the binding in /clusters/system:admin
kubectl --kubeconfig /tmp/root-masters.kubeconfig \
  --server https://localhost:6443/clusters/system:admin \
  apply --validate=false -f test/fixtures/system-admin-rbac-bootstrap.yaml

# 4. Stop the port-forward
kill $PF_PID
```

Repeat for every additional shard, swapping `rootShardRef` for
`shardRef: {name: <shard-name>}` in the `Kubeconfig` CR and pointing the
port-forward at that shard's service (e.g., `svc/shard1-shard-kcp`).

The fixture
([`test/fixtures/system-admin-rbac-bootstrap.yaml`](../test/fixtures/system-admin-rbac-bootstrap.yaml))
grants the webhook SA `*/*` `get`/`list`. Within a shard, this binding
applies to every workspace on that shard — no per-consumer RBAC is needed.

### Adjusting service account names

The bootstrap RBAC files bind to these default identities:
- Controller: `system:serviceaccount:dependency-system:dependency-controller`
- Webhook: `system:serviceaccount:dependency-system:dependency-webhook`

If your deployment uses different service account names or namespaces, edit the
`subjects` in the `ClusterRoleBinding` resources before applying. The names must
match what you configure in the Helm values (Step 5).

## Step 4: Create kubeconfigs for the components

Each component needs a kubeconfig that authenticates as its service account
identity. Use kcp-operator `Kubeconfig` CRs to generate client certificates:

```sh
# Controller kubeconfig
kubectl apply -f - <<'EOF'
apiVersion: operator.kcp.io/v1alpha1
kind: Kubeconfig
metadata:
  name: controller-kubeconfig
  namespace: kcp-system
spec:
  username: "system:serviceaccount:dependency-system:dependency-controller"
  groups:
    - "system:authenticated"
    - "system:serviceaccounts"
    - "system:serviceaccounts:dependency-system"
  validity: 8766h
  secretRef:
    name: controller-kubeconfig
  target:
    rootShardRef:
      name: root
EOF

# Webhook kubeconfig
kubectl apply -f - <<'EOF'
apiVersion: operator.kcp.io/v1alpha1
kind: Kubeconfig
metadata:
  name: webhook-kubeconfig
  namespace: kcp-system
spec:
  username: "system:serviceaccount:dependency-system:dependency-webhook"
  groups:
    - "system:authenticated"
    - "system:serviceaccounts"
    - "system:serviceaccounts:dependency-system"
  validity: 8766h
  secretRef:
    name: webhook-kubeconfig
  target:
    rootShardRef:
      name: root
EOF
```

**Important notes about kcp-operator Kubeconfig CRs:**

- **`rootShardRef` (not `frontProxyRef`)**: This generates client certificates
  signed by `root-client-ca`, which is trusted by both the front-proxy (via
  `kcp-merged-client-ca`) and all shards directly. This is required because the
  multicluster-provider connects to APIExport virtual workspace URLs that point
  directly at shards, not through the front-proxy.

- **`system:authenticated` group**: kcp's front-proxy uses request header
  impersonation. Unlike direct Kubernetes authentication, impersonated
  identities do not automatically get the `system:authenticated` group -- it
  must be listed explicitly.

- **Two contexts**: kcp-operator generates kubeconfigs with two contexts:
  `base` (bare shard URL) and `default` (shard URL + `/clusters/root`). The
  component kubeconfigs should be rewritten to point at the front-proxy with
  the dep-ctrl workspace path (`/clusters/root:dep-ctrl`) before mounting
  them as secrets.

Extract and rewrite the kubeconfigs:

```sh
# Extract the controller kubeconfig
kubectl -n kcp-system get secret controller-kubeconfig \
  -o jsonpath='{.data.kubeconfig}' | base64 -d > /tmp/controller.kubeconfig

# Rewrite the server URL from the shard URL to the front-proxy + workspace path
# (replace the shard URL with https://<front-proxy-host>:<port>/clusters/root:dep-ctrl)

# Store as Kubernetes secrets
kubectl -n dependency-system create secret generic kcp-controller-kubeconfig \
  --from-file=kubeconfig=/tmp/controller.kubeconfig

kubectl -n dependency-system create secret generic kcp-webhook-kubeconfig \
  --from-file=kubeconfig=/tmp/webhook.kubeconfig
```

## Step 5: Deploy with Helm

The Helm chart deploys both the controller and webhook as separate Deployments
in a single release. The controller automatically discovers the webhook's
service URL and TLS CA from the co-deployed resources.

```sh
helm install dep-ctrl charts/dependency-controller \
  --namespace dependency-system --create-namespace \
  --set controller.kubeconfig.secretName=kcp-controller-kubeconfig \
  --set webhook.kubeconfig.secretName=kcp-webhook-kubeconfig \
  --set webhook.tls.certManager.issuerRef.name=selfsigned
```

Key values:

| Value | Purpose |
|---|---|
| `controller.kubeconfig.secretName` | Secret containing the controller's kubeconfig (from Step 4). |
| `webhook.kubeconfig.secretName` | Secret containing the webhook's kubeconfig (from Step 4). |
| `webhook.tls.certManager.issuerRef.name` | cert-manager issuer for the webhook's TLS certificate. |

Both components automatically derive the kcp front-proxy base URL from their
kubeconfig by stripping the `/clusters/...` workspace path suffix. No
additional base URL flag is needed (though `kcpBaseHost` can be set to
override this if your kubeconfig host doesn't follow the standard pattern).

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

## Step 6: Onboard a provider

This example uses two providers -- a network provider (exports VPCs) and a
compute provider (exports VirtualMachines). The compute provider will create a
DependencyRule saying "VMs depend on VPCs", which blocks VPC deletion while
any VM references it.

### 6a. Create provider workspaces and APIExports

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

### 6b. Bind providers to the dep-ctrl APIExport

**Every provider workspace referenced in a DependencyRule must bind to the
dep-ctrl APIExport and accept the permissionClaims.** This applies to both
sides of the dependency relationship:

- The **dependency target provider** (network-provider, which exports VPCs) --
  the controller needs to install a `ValidatingWebhookConfiguration` there to
  intercept VPC deletions
- The **dependent provider** (compute-provider, which exports VMs) -- the
  controller needs to reach this workspace via the VW to discover the
  DependencyRule

The controller reaches both workspaces through the dep-ctrl APIExport's virtual
workspace. In kcp, a virtual workspace can only access workspaces that bind to
its APIExport -- and `permissionClaims` control which resource types the VW is
allowed to manage in those workspaces. Without the binding and accepted claims,
the controller has no access.

Apply the binding in **both** provider workspaces:

```sh
# In network-provider (where the webhook will be installed)
kubectl ws root:network-provider
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
EOF

# In compute-provider (where the DependencyRule is created)
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
EOF
```

A reference fixture is available at
[`test/fixtures/apibinding-dependencies.opendefense.cloud.yaml`](../test/fixtures/apibinding-dependencies.opendefense.cloud.yaml)
(replace `${DEP_CTRL_PATH}` with `root:dep-ctrl`).

The accepted permissionClaim grants the controller permission to create
`ValidatingWebhookConfigurations` in the workspace (for deletion protection).

### 6c. Create a DependencyRule

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
    apiExportName: compute.test.io
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

Once applied, the controller will install a `ValidatingWebhookConfiguration`
in `root:network-provider` (protecting VPC deletions). The webhook registers
the rule's metadata in its registry and begins serving admission requests
for VPC deletions.

### 6d. Create a consumer workspace and test resources

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

### 6e. Verify deletion protection

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

Here's the flow that makes Step 6e work:

1. The **controller** watches DependencyRules via the dep-ctrl APIExport's
   virtual workspace
2. When it sees the `vm-dependencies` rule, it resolves `root:network-provider`
   to a logical cluster name by reading the `Workspace` object from root
3. It connects to the dep-ctrl VW at
   `<vw-url>/clusters/<logical-cluster-name>` and creates a
   `ValidatingWebhookConfiguration` in the network-provider workspace
   (authorized by the accepted `validatingwebhookconfigurations` permissionClaim)
4. The **webhook** also watches the same DependencyRule and registers the rule's
   metadata (dependent GVR, field paths, target GVR) in its in-memory registry
5. When a consumer deletes a VPC, kcp dispatches the DELETE to the webhook
   (via the installed `ValidatingWebhookConfiguration`)
6. The webhook extracts the logical cluster name from the `kcp.io/cluster`
   annotation on the object, constructs a temporary dynamic client scoped to
   `{frontProxy}/clusters/{clusterName}`, and lists VirtualMachines in that
   namespace
7. It filters by field path: "any VMs where `.spec.vpcRef.name` equals
   `my-vpc`?" -- finds `my-vm` and denies the deletion

## Multi-shard considerations

The dependency-controller is multi-shard aware. The runtime data path needs
no per-shard configuration — but the **bootstrap RBAC does**:

- **Webhook installation**: The controller installs webhooks via the dep-ctrl
  APIExport's virtual workspace, which automatically routes to the correct
  shard for each provider workspace. No changes per shard.

- **Per-request queries**: The webhook queries dependent resources via the
  kcp front-proxy, which transparently routes requests to the correct shard
  based on the logical cluster name. No webhook-side configuration changes
  per shard.

- **`system:admin` RBAC must be applied per shard.** kcp's
  `BootstrapPolicyAuthorizer` reads RBAC from the local shard's `system:admin`
  logical cluster only; bindings do not propagate across shards. Each new
  shard that hosts consumer workspaces needs the webhook's wildcard read
  binding applied via the procedure in [Step 3](#webhook-rbac-in-systemadmin-per-shard).

- **Root and dep-ctrl workspace RBAC are applied once.** Those workspaces
  live on the root shard and the bindings there are not duplicated.

- **Kubeconfigs**: Component kubeconfigs must use certificates signed by
  `root-client-ca` (via `rootShardRef` in the Kubeconfig CR). This CA is
  trusted by both the front-proxy and all shards, which is required because
  VW URLs point directly at shards.

## Troubleshooting

### Webhook pod not becoming ready

The webhook's readiness probe fails until it has listed all existing
DependencyRules. Check the webhook logs:

```sh
kubectl -n dependency-system logs -l app.kubernetes.io/component=webhook
```

Common issues:

- **Kubeconfig invalid** -- the webhook can't reach kcp
- **Missing dep-ctrl workspace RBAC** -- ensure
  [Step 3](#dep-ctrl-workspace-rbac) was applied
- **Missing `system:admin` RBAC on a shard** -- if the webhook can list on some
  consumer workspaces but not others, the failing ones are likely on a shard
  where the `system:admin` binding from
  [Step 3](#webhook-rbac-in-systemadmin-per-shard) was never applied.
  Verify with: `kubectl auth can-i list <some-resource>
  --as=system:serviceaccount:dependency-system:dependency-webhook`
  against the consumer workspace path.
- **Certificate not trusted by shards** -- ensure kubeconfigs use `rootShardRef`
  (not `frontProxyRef`) so certs are signed by `root-client-ca`

### Webhook not blocking deletions

Check that all pieces are in place:

```sh
# Is the ValidatingWebhookConfiguration installed?
kubectl ws root:network-provider
kubectl get validatingwebhookconfiguration dependency-controller

# Are the DependencyRule bindings bound?
kubectl ws root:compute-provider
kubectl get apibinding dependencies.opendefense.cloud -o jsonpath='{.status.phase}'
# Should be: Bound
```

### Controller can't create webhooks

Check that the provider workspace's APIBinding has accepted the
`validatingwebhookconfigurations` permissionClaim (Step 6b). Without
acceptance, the VW rejects write operations:

```sh
kubectl ws root:network-provider
kubectl get apibinding dependencies.opendefense.cloud -o yaml
# Look for permissionClaims with state: Accepted
```

### Force-deleting a protected resource

If the webhook is down or rules are stale, annotate the resource to bypass
protection:

```sh
kubectl annotate vpc my-vpc dependencies.opendefense.cloud/skip-protection=true
kubectl delete vpc my-vpc
```
