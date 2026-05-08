# kcp-aware Dependency Controller

[![Build status](https://github.com/opendefensecloud/dependency-controller/actions/workflows/golang.yaml/badge.svg)](https://github.com/opendefensecloud/dependency-controller/actions/workflows/golang.yaml)
[![Go Report Card](https://goreportcard.com/badge/go.opendefense.cloud/dependency-controller)](https://goreportcard.com/report/go.opendefense.cloud/dependency-controller)
[![Go Reference](https://pkg.go.dev/badge/go.opendefense.cloud/dependency-controller.svg)](https://pkg.go.dev/go.opendefense.cloud/dependency-controller)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/opendefensecloud/dependency-controller/badge)](https://scorecard.dev/viewer/?uri=github.com/opendefensecloud/dependency-controller)
[![GitHub Release](https://img.shields.io/github/v/release/opendefensecloud/dependency-controller)](https://github.com/opendefensecloud/dependency-controller/releases/latest)

## Problem Statement

In kcp, APIs can be offered to users via APIExports by a multitude of providers.
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
    B --> E["<b>Webhook:</b><br/>Register rule metadata<br/>(dependent GVR, field paths)<br/>in RuleRegistry"]

    E --> F["Registry holds rule metadata only<br/>— no cache of dependents"]

    F --> G{"Consumer tries to delete<br/>dependency (e.g. VPC)"}
    G --> H["Webhook intercepts DELETE"]
    H --> I["List VMs in consumer workspace<br/>via kcp front-proxy;<br/>in-memory filter:<br/>.spec.vpcRef.name == my-vpc?"]
    I -- Yes --> J["Deny deletion<br/>'still referenced by VirtualMachine/my-vm'"]
    I -- No --> K["Allow deletion"]

    style A fill:#e1f0da,color:#1a3e12
    style C fill:#fff3cd,color:#664d03
    style E fill:#d4edfc,color:#0a3069
    style F fill:#d4edfc,color:#0a3069
    style J fill:#f8d7da,color:#6e1520
    style K fill:#d4edda,color:#0f5132
```

### DependencyRule

Along with their APIExport, providers create `DependencyRule` objects to describe how their
resources depend on others. A single rule attaches to one dependent resource type (via its
APIExport reference in the same workspace as the rule) and lists each dependency together
with the **dependency provider's** APIExport reference (workspace path + name) and the field
path inside the dependent resource where the reference lives:

```yaml
apiVersion: dependencies.opendefense.cloud/v1alpha1
kind: DependencyRule
metadata:
  name: vm-dependencies
spec:
  dependent:
    apiExportName: compute.example.com
    group: compute.example.com
    version: v1alpha1
    kind: VirtualMachine
    resource: virtualmachines
  dependencies:
    - apiExportRef:
        path: root:providers:network
        name: network.example.com
      group: network.example.com
      version: v1alpha1
      resource: vpcs
      fieldRef:
        path: ".spec.vpcRef.name"
    - apiExportRef:
        path: root:providers:network
        name: network.example.com
      group: network.example.com
      version: v1alpha1
      resource: subnets
      fieldRef:
        path: ".spec.subnetRef.name"
```

### Two Binaries

The system runs as two binaries, deployed together via a single Helm chart, that
both watch `DependencyRule` objects via the dep-ctrl APIExport:

**Controller** (`cmd/controller`) -- handles infrastructure setup:

- Installs `ValidatingWebhookConfiguration` in each provider workspace whose
  resources are protected as dependencies
- All provider workspace access goes through the dep-ctrl APIExport's virtual
  workspace, authorized by `permissionClaims` on the APIExport

**Webhook** (`cmd/webhook`) -- handles admission:

- Watches `DependencyRule` objects via the dep-ctrl APIExport's virtual
  workspace and stores parsed metadata (dependent GVR + field paths) in an
  in-memory `RuleRegistry`.
- On each DELETE admission request, finds matching rules in the registry,
  lists dependent resources directly in the consumer workspace via the kcp
  front-proxy, and filters in-memory by the configured field path to block
  deletion of still-referenced resources.

### Rule Registry

The webhook keeps an in-memory `RuleRegistry` populated by reconciling
`DependencyRule` objects through the dep-ctrl APIExport's virtual workspace.
Each entry holds rule metadata only — the dependent's GroupVersionResource
and the field paths that hold dependency references — not the dependent
resources themselves. Dependent listing happens on demand per admission
request (see [Admission Webhook](#admission-webhook) below).

### Admission Webhook

A kcp ValidatingAdmissionWebhook intercepts DELETE requests. When a delete is attempted,
the webhook looks up matching rules in the registry, builds a per-request dynamic client
targeting the consumer workspace via the kcp front-proxy
(`{base}/clusters/{logicalCluster}`), `List`s the dependent type, and filters the results
in-memory by the rule's field path. If any blocker is found, the request is denied with a
clear error message listing the dependents. Finalizers are intentionally avoided as they
conflict with kcp's sync-agent.

### Architecture

The dependency-controller runs in its own workspace with its own APIExport for the
`DependencyRule` type. Provider workspaces bind to it to create rules and to accept
the `permissionClaims` that grant the controller access to manage webhooks
in those workspaces. Consumer workspaces do not need to bind to the dep-ctrl export.

```mermaid
graph LR
    subgraph DC["Dep-Ctrl Workspace"]
        DCExport["APIExport:<br/>DependencyRule<br/><i>+ permissionClaims</i>"]
    end

    subgraph CB["Controller Binary"]
        Ctrl["DependencyRule Reconciler<br/>· Webhook Installer<br/>· Workspace Resolver"]
    end

    subgraph WB["Webhook Binary"]
        WH["DependencyRule Reconciler<br/>· Rule Registry (metadata)<br/>· Deletion Validator"]
    end

    subgraph CP["Compute Provider Workspace"]
        CPBinding["APIBinding: dep-ctrl<br/><i>(claims accepted)</i>"]
        CPExport["APIExport: compute"]
        CPRule["DependencyRule:<br/>VM → VPC"]
    end

    subgraph NP["Network Provider Workspace"]
        NPBinding["APIBinding: dep-ctrl<br/><i>(claims accepted)</i>"]
        NPExport["APIExport: VPCs"]
        NPWebhook["ValidatingWebhook"]
    end

    subgraph ROOT["Root Workspace"]
        ROOTROLE["ClusterRoles<br/>(workspaces/content +<br/>workspace resolution)"]
    end

    subgraph CW["Consumer Workspace"]
        CWBindings["APIBindings:<br/>compute, network"]
        CWResources["VPC, VM"]
    end

    CPBinding -->|binds to| DCExport
    NPBinding -->|binds to| DCExport
    Ctrl -.->|watches rules via VW| DCExport
    Ctrl -.->|installs webhook via VW| NP
    WH -.->|watches rules via VW| DCExport
    NPWebhook -.->|dispatches DELETE to| WH
    WH -.->|on DELETE: lists dependents<br/>via kcp front-proxy| CW
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

**Multicluster watching is one-level only:** both binaries watch
`DependencyRule` objects via the dep-ctrl APIExport's virtual workspace,
which spans every provider workspace bound to it. Dependent resources
(e.g., VMs) are not watched — the webhook lists them on demand from the
consumer workspace via the kcp front-proxy when validating a DELETE.

For detailed architecture documentation, see [docs/architecture.md](docs/architecture.md).
For a step-by-step deployment walkthrough, see [docs/getting-started.md](docs/getting-started.md).
For development setup and project layout, see [docs/development.md](docs/development.md).

### RBAC Model

The system uses static bootstrap RBAC in three kcp locations. No dynamic RBAC is
created at runtime.

#### permissionClaims on the dep-ctrl APIExport

The dep-ctrl APIExport declares a `permissionClaim` for:

- `validatingwebhookconfigurations` (admissionregistration.k8s.io) -- to install webhooks

Provider workspaces that bind to the dep-ctrl APIExport must **accept** this claim
in their `APIBinding` spec. This grants the controller access to manage webhooks
in binding workspaces through the virtual workspace.

#### Bootstrap RBAC (static, applied at deployment)

**Root workspace** -- both components need `workspaces/content` access to enter child
workspaces. The controller additionally needs `workspaces` read access to resolve
workspace paths to logical cluster names.

**Dep-ctrl workspace** -- the controller needs `apiexportendpointslices` read access
for VW URL discovery and full CRUD on `apiexports/content` to manage webhooks in
binding workspaces via the VW.

No shard-wide RBAC is needed. The webhook watches dependent resources through the
dep-ctrl APIExport's virtual workspace, authorized by dynamically managed
permissionClaims. Providers accept these claims in their APIBinding.

See [docs/getting-started.md](docs/getting-started.md) for the full deployment guide
using [kcp-operator](https://github.com/kcp-dev/helm-charts).

## Development

The fastest way to get a working dev environment is the [Nix flake](flake.nix)
together with [direnv](https://direnv.net/): `direnv allow` (or `nix develop`)
drops you into a shell with Go, `golangci-lint`, `helm`, `kind`, and the kcp
toolchain on `$PATH`. After that, `pre-commit install` registers the project's
hooks.

For project layout, the full `make` target reference, integration- and e2e-test
internals, and shard-config tips, see [docs/development.md](docs/development.md).
