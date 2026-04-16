# KCP-aware Dependency Controller

## Problem Statement

In KCP, APIs can be offered to users via APIExports by a multitude of providers.
For IaaS services however there is a critical shortcoming:
IaaS APIs typically depend on each other -- for example, a VM is provisioned in a VPC.
The VM is dependent on the VPC. If the VPC is deleted, it pulls the rug from under the VM.

The dependency-controller blocks the deletion of resources that still have active dependents.

## How It Works

### Lifecycle Overview

```mermaid
flowchart TD
    A["Provider creates<br/><b>DependencyRule</b><br/>(e.g. VM → VPC)"] --> B["Both binaries discover rule<br/>via dep-ctrl APIExport"]

    B --> C["<b>Controller:</b><br/>Install ValidatingWebhook<br/>in dependency provider workspace"]
    B --> D["<b>Controller:</b><br/>Create RBAC in provider workspace<br/>(apiexports/content)"]
    B --> E["<b>Webhook:</b><br/>Start indexed cache watching<br/>dependent type via APIExport VW"]

    E --> F["Informer indexes dependents<br/>by field paths<br/>(e.g. .spec.vpcRef.name)"]

    F --> G{"Consumer tries to delete<br/>dependency (e.g. VPC)"}
    G --> H["Webhook intercepts DELETE"]
    H --> I["Query indexed cache:<br/>any VMs where .spec.vpcRef.name = my-vpc?"]
    I -- Yes --> J["Deny deletion<br/>'still referenced by VirtualMachine/my-vm'"]
    I -- No --> K["Allow deletion"]

    style A fill:#e1f0da,color:#1a3e12
    style C fill:#fff3cd,color:#664d03
    style D fill:#fff3cd,color:#664d03
    style E fill:#d4edfc,color:#0a3069
    style F fill:#d4edfc,color:#0a3069
    style J fill:#f8d7da,color:#6e1520
    style K fill:#d4edda,color:#0f5132
```

### DependencyRule

Along with their APIExport, providers create `DependencyRule` objects to describe how their
resources depend on others. A single rule attaches to one dependent resource type (via its
APIExport reference) and lists all of its dependencies with field paths that describe where
the reference lives:

```yaml
apiVersion: dependencies.opendefense.cloud/v1alpha1
kind: DependencyRule
metadata:
  name: vm-dependencies
spec:
  dependent:
    apiExportRef:
      path: root:compute-provider
      name: compute.example.com
    group: compute.example.com
    version: v1alpha1
    kind: VirtualMachine
    resource: virtualmachines
  dependencies:
    - group: network.example.com
      version: v1alpha1
      resource: vpcs
      fieldRef:
        path: ".spec.vpcRef.name"
    - group: network.example.com
      version: v1alpha1
      resource: subnets
      fieldRef:
        path: ".spec.subnetRef.name"
```

### Two Binaries

The system runs as two independently deployable binaries that both watch
`DependencyRule` objects via the dep-ctrl APIExport:

**Controller** (`cmd/controller`) -- handles infrastructure setup:
- Installs `ValidatingWebhookConfiguration` in each provider workspace whose
  resources are protected as dependencies
- Creates `ClusterRole` and `ClusterRoleBinding` in each dependent's provider
  workspace granting the webhook `apiexports/content` access on the dependent
  resource's APIExport (see [Webhook RBAC](#webhook-rbac))

**Webhook** (`cmd/webhook`) -- handles admission:
- Maintains a dedicated indexed cache per rule, watching the dependent resource
  type via the provider's APIExport virtual workspace
- Serves admission requests, querying indexed caches to block deletion of
  resources that are still referenced

### Indexed Cache

For each DependencyRule, the webhook server starts a multicluster manager that watches the
dependent resource type (e.g., VirtualMachines) via the referenced APIExport's virtual
workspace. Field indices are registered on the dependent informer for each dependency
target's field path (e.g., `.spec.vpcRef.name`), enabling O(1) lookups by referenced
resource name.

### Admission Webhook

A KCP ValidatingAdmissionWebhook intercepts DELETE requests. When a delete is attempted,
the webhook queries the indexed caches to find dependent resources that reference the
resource being deleted. If any are found, the request is denied with a clear error message
listing the dependents. Finalizers are intentionally avoided as they conflict with KCP's
sync-agent.

### Architecture

The dependency-controller runs in its own workspace with its own APIExport for the
`DependencyRule` type. Providers bind to it to create rules. Consumer workspaces do
not need to bind to the dep-ctrl export.

```mermaid
graph LR
    subgraph DC["Dep-Ctrl Workspace"]
        DCExport["APIExport:<br/>DependencyRule"]
    end

    subgraph CB["Controller Binary"]
        Ctrl["DependencyRule Reconciler<br/>· Webhook Installer<br/>· RBAC Manager"]
    end

    subgraph WB["Webhook Binary"]
        WH["Rule Cache Manager<br/>· Indexed Caches (per rule)<br/>· Deletion Validator"]
    end

    subgraph CP["Compute Provider WS"]
        CPBinding["APIBinding: dep-ctrl"]
        CPExport["APIExport: compute"]
        CPRule["DependencyRule:<br/>VM → VPC"]
    end

    subgraph NP["Network Provider WS"]
        NPExport["APIExport: VPCs"]
        NPWebhook["ValidatingWebhook"]
    end

    subgraph ROOT["Root Workspace"]
        ROOTROLE["ClusterRole<br/>(workspaces/content access)"]
    end

    subgraph CW["Consumer WS"]
        CWBindings["APIBindings:<br/>compute, network"]
        CWResources["VPC, VM"]
    end

    CPBinding -->|binds to| DCExport
    Ctrl -.->|watches rules via| DCExport
    Ctrl -.->|installs webhook in| NP
    Ctrl -.->|manages apiexports/content<br/>RBAC in| CP
    WH -.->|watches rules via| DCExport
    WH -.->|watches VMs via| CPExport
    NPWebhook -.->|dispatches DELETE to| WH
    CWBindings -->|binds to| CPExport
    CWBindings -->|binds to| NPExport

    style DC fill:#dbeafe,color:#1e3a5f
    style CB fill:#dbeafe,color:#1e3a5f
    style WB fill:#fce4ec,color:#6e1520
    style CP fill:#e1f0da,color:#1a3e12
    style NP fill:#e1f0da,color:#1a3e12
    style ROOT fill:#f3e8ff,color:#4a1d7a
    style CW fill:#fef3c7,color:#664d03
```

**Two levels of multicluster watching:**

1. **DependencyRule reconciler** (both binaries) watches rules via the dep-ctrl's own
   APIExport virtual workspace, discovering provider workspaces that bind to the dep-ctrl
   export.

2. **Indexed cache** (webhook only, dynamic per-rule) watches the dependent resource type
   (e.g., VMs) via the referenced APIExport's virtual workspace. Field indices enable the
   webhook to quickly find dependents referencing a given resource.

**RBAC:** The controller dynamically manages per-workspace RBAC granting the webhook
`apiexports/content` access. A static `workspaces/content` grant in the root workspace
is required as a prerequisite (see [Webhook RBAC](#webhook-rbac)).

For detailed architecture documentation, see [docs/architecture.md](docs/architecture.md).

### Webhook RBAC

The webhook server needs to access APIExport virtual workspaces to watch dependent
resources. In kcp, virtual workspace access is authorized via the `apiexports/content`
subresource in the workspace where the APIExport is defined. The RBAC setup has two layers:

#### Static prerequisite: `workspaces/content` in root

The webhook service account needs `workspaces/content` access in the root workspace to
enter child workspaces through APIExport virtual workspace endpoints. This must be
provisioned at deployment time (e.g., via bootstrap RBAC or Helm chart):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: dependency-controller-webhook
rules:
  - apiGroups: ["core.kcp.io"]
    resources: ["workspaces/content"]
    verbs: ["access"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: dependency-controller-webhook
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: dependency-controller-webhook
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: User
    name: "system:serviceaccount:<namespace>:<webhook-sa-name>"
```

This grant is static and does not change as rules are added or removed.

#### Dynamic: `apiexports/content` per provider workspace

When the controller processes a `DependencyRule`, it reads the rule's
`spec.dependent.apiExportRef` to determine which provider workspace hosts the
dependent resource's APIExport. It then creates a `ClusterRole` and
`ClusterRoleBinding` in that workspace granting the webhook:

- `get`/`list`/`watch` on `apiexports/content` scoped to the specific APIExport name
- `get`/`list`/`watch` on `apiexportendpointslices` (needed for VW URL discovery)

For example, if a `DependencyRule` declares that `VirtualMachine` (from
`compute.example.com` in `root:compute-provider`) depends on VPCs, the controller
creates RBAC in `root:compute-provider` granting access to `apiexports/content`
for `compute.example.com`. This allows the webhook to read VirtualMachines across
all consumer workspaces through the compute APIExport's virtual workspace.

When multiple rules reference the same provider workspace, their APIExport names
are merged into a single ClusterRole. When the last rule referencing a workspace is
deleted, the ClusterRole and ClusterRoleBinding are removed.

The webhook does **not** receive RBAC in the dependency target's workspace
(e.g., `root:network-provider`). It only needs access to the dependent's APIExport
virtual workspace to index the dependent resources.

## Development

### Prerequisites

- Go 1.26+
- [kcp](https://github.com/kcp-dev/kcp) binary (for integration tests)

### Build

```sh
make build
```

### Run Tests

```sh
# Unit and integration tests (requires kcp binary)
make test

# E2E tests (requires kind, helm, docker)
make test-e2e
```

### Generate Code

```sh
make generate
```
