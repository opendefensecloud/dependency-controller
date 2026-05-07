# Architecture

The dependency-controller prevents deletion of resources that are still referenced
by other resources in a multi-tenant [kcp](https://kcp.io) environment.

## Problem

Different API providers export resource types (VPCs, VirtualMachines, ManagedDBs, ...)
from separate kcp workspaces. Consumer workspaces bind to multiple providers and
create resources that reference each other -- a VirtualMachine references a VPC by
name, a ManagedDB references a FirewallRule, etc.

Without coordination, deleting a VPC that is still referenced by a VirtualMachine
leaves the VM in a broken state.

The system solves this with two cooperating binaries:

- **Controller** -- watches `DependencyRule` objects and installs admission webhooks
  in provider workspaces
- **Webhook** -- maintains a metadata registry of dependency rules and serves
  admission requests that block deletion of still-referenced resources by querying
  consumer workspaces directly

## Workspace Topology

```mermaid
graph LR
    subgraph DC["Dep-Ctrl Workspace"]
        DCExport["APIExport:<br/>DependencyRule<br/><i>+ VWC permissionClaim</i>"]
    end

    subgraph CP["Compute Provider WS"]
        CPBinding["APIBinding: dep-ctrl<br/><i>(VWC claim accepted)</i>"]
        CPExport["APIExport: compute"]
        CPRule["DependencyRule:<br/>VM → VPC"]
    end

    subgraph NP["Network Provider WS"]
        NPBinding["APIBinding: dep-ctrl<br/><i>(VWC claim accepted)</i>"]
        NPExport["APIExport: VPCs"]
        NPWebhook["ValidatingWebhook"]
    end

    subgraph ROOT["Root Workspace"]
        ROOTROLE["ClusterRoles:<br/><i>controller: workspaces/content<br/>+ workspaces read</i>"]
    end

    subgraph SYSADMIN["system:admin (per shard)"]
        SACR["ClusterRoles:<br/><i>webhook: wildcard read</i><br/>(applied directly to each shard)"]
    end

    subgraph CW["Consumer WS"]
        CWBindings["APIBindings:<br/>compute, network"]
        CWResources["VPC, VM"]
    end

    CPBinding -->|binds to| DCExport
    NPBinding -->|binds to| DCExport
    CWBindings -->|binds to| CPExport
    CWBindings -->|binds to| NPExport

    style DC fill:#dbeafe,color:#1e3a5f
    style CP fill:#e1f0da,color:#1a3e12
    style NP fill:#e1f0da,color:#1a3e12
    style ROOT fill:#f3e8ff,color:#4a1d7a
    style SYSADMIN fill:#f3e8ff,color:#4a1d7a
    style CW fill:#fef3c7,color:#664d03
```

**Dep-ctrl workspace** -- hosts the `DependencyRule` APIExport
(`dependencies.opendefense.cloud`) with a `permissionClaim` for
`validatingwebhookconfigurations`. Both the controller and webhook connect to
this workspace's virtual workspace to discover rules. The controller also uses
the virtual workspace to manage webhooks in binding workspaces (authorized by
the permissionClaim).

**Provider workspaces** -- each provider (compute, network, ...) exports its own
resource types and binds to the dep-ctrl APIExport to create `DependencyRule`
objects. The APIBinding must **accept** the dep-ctrl's VWC permissionClaim, which
grants the controller access to manage `ValidatingWebhookConfigurations` in
those workspaces through the virtual workspace.

**Consumer workspaces** -- bind to provider exports and create the actual
resources (VPCs, VMs). Consumers don't interact with the dependency system
directly. The webhook queries dependent resources in consumer workspaces via
the front-proxy using broad read RBAC.

**Root workspace** -- hosts static `ClusterRoles` for the **controller**:
`workspaces/content` access (to enter child workspaces) and `workspaces` read
(to resolve workspace paths to logical cluster names). This is a deployment
prerequisite.

**`system:admin` workspace, per shard** -- hosts a `ClusterRole` +
`ClusterRoleBinding` granting the **webhook** wildcard read access (`get`,
`list` on all resources). The webhook queries dependent resources directly
in consumer workspaces, and consumer workspaces can live on any shard. kcp's
`BootstrapPolicyAuthorizer` reads RBAC from the local shard's `system:admin`
only — bindings do not propagate across shards — so this binding must be
applied to **every shard** that hosts consumer workspaces. `system:admin` is
not reachable through the front-proxy; the binding is applied via direct
(port-forwarded) shard access using a `system:masters` identity issued from
a kcp-operator `Kubeconfig` CR with `rootShardRef` / `shardRef`.

## Component Overview

```mermaid
flowchart TD
    subgraph Controller["Controller Binary (cmd/controller)"]
        DR["DependencyRule Reconciler<br/><i>+ Workspace Resolver</i>"]
        DR -->|delegates to| WI["Webhook Installer"]
    end

    subgraph Webhook["Webhook Server Binary (cmd/webhook)"]
        RCM["Rule Registry Manager"]
        RCM -->|"populates"| RR["Rule Registry<br/>(metadata only)"]
        DV["Deletion Validator"]
        DV -->|"queries rules"| RR
        DV -->|"queries dependents via<br/>front-proxy per request"| CW["Consumer Workspaces"]
    end

    WI -->|"installs via dep-ctrl VW"| PW["Provider Workspaces"]
    PW -->|"dispatches DELETE to"| DV

    style Controller fill:#dbeafe,color:#1e3a5f
    style Webhook fill:#fce4ec,color:#6e1520
    style PW fill:#fef3c7,color:#664d03
```

---

## Controller

**Entry point:** [`cmd/controller/main.go`](../cmd/controller/main.go)

The controller watches `DependencyRule` objects and installs
`ValidatingWebhookConfiguration` objects in the right provider workspaces.

All operations in provider workspaces are routed through the dep-ctrl APIExport's
virtual workspace, authorized by `permissionClaims`. The controller never connects
directly to provider workspaces.

### Initialization and Workspace Resolution

On first reconcile, the controller lazily initializes two components
([`ensureInitialized`](../internal/controller/dependencyrule_controller.go)):

1. **VW URL discovery** -- reads the `APIExportEndpointSlice` for the dep-ctrl
   APIExport to find the virtual workspace base URL
2. **Workspace resolver** -- resolves workspace paths (e.g., `root:network-provider`)
   to logical cluster names (e.g., `qh6707jkfsen31z9`) by reading `Workspace`
   objects from the root workspace (`ws.Spec.Cluster`)

The VW only accepts logical cluster names in its `/clusters/<name>` path, not
workspace paths. The resolver caches mappings and is consulted before every
webhook operation.

### How a DependencyRule becomes a webhook

When a provider creates a `DependencyRule` ([`api/v1alpha1/types.go`](../api/v1alpha1/types.go)),
the controller's reconciler
([`internal/controller/dependencyrule_controller.go:Reconcile`](../internal/controller/dependencyrule_controller.go))
picks it up via the dep-ctrl APIExport's virtual workspace and installs webhooks.

The [`WebhookInstaller`](../internal/controller/webhook_installer.go) creates or
updates a `ValidatingWebhookConfiguration` named `dependency-controller` in each
provider workspace whose resources are referenced as dependencies.

The rule's `spec.dependencies[].apiExportRef.path` determines which workspace to
target. The reconciler resolves the path to a logical cluster name and sets the
installer's `BaseConfig` to the dep-ctrl VW URL, so the installer connects via
`<vw-url>/clusters/<logical-cluster-name>`. The `permissionClaims` on the dep-ctrl
APIExport authorize creating `ValidatingWebhookConfigurations` in the binding
workspace.

The installer groups all dependency targets by workspace and merges them
into a single webhook per workspace
([`reconcileWorkspaceWebhook`](../internal/controller/webhook_installer.go)).

For example, if two DependencyRules both protect resources from the network
provider, the installer creates one webhook in the network provider's workspace
with two `rules` entries (one per protected GVR). This merging is tracked via
`ruleTargets map[string][]ruleTarget` -- keyed by DependencyRule, so each rule's
contributions can be independently added or removed. On any change,
[`desiredRulesForWorkspace`](../internal/controller/webhook_installer.go)
recomputes the full desired state from scratch to avoid incremental bookkeeping bugs.

When a DependencyRule is deleted
([`handleDeletion`](../internal/controller/dependencyrule_controller.go)),
the installer removes that rule's contributions. If no rules remain for a
workspace, the webhook is deleted entirely.

---

## Webhook

**Entry point:** [`cmd/webhook/main.go`](../cmd/webhook/main.go)

The webhook server watches the same `DependencyRule` objects as the controller,
but its job is different: it maintains a metadata registry of active rules and
uses per-request direct queries to check for active dependents when a deletion
is attempted.

### Startup and Registry Population

On startup, the webhook creates an `mcmanager` backed by the dep-ctrl APIExport
provider, then registers the
[`RuleCacheManager`](../internal/webhook/rule_cache_manager.go) as a controller
watching `DependencyRule` objects.

Before the webhook can serve requests safely, it must populate its registry with
all existing rules. This happens in a
[`manager.RunnableFunc`](../cmd/webhook/main.go) that runs after the manager
starts:

1. [`PopulateRegistry`](../internal/webhook/rule_cache_manager.go) resolves
   the dep-ctrl APIExport's virtual workspace URL from its
   `APIExportEndpointSlice`
2. Lists all existing `DependencyRule` objects across all bound workspaces
3. Registers each rule's metadata (GVK, GVR, field paths) in the registry
4. Closes the `initialized` channel

Until `initialized` is closed:
- The readyz probe ([`ReadyzCheck`](../internal/webhook/deletion_validator.go))
  returns unhealthy
- The `DeletionValidator` denies all DELETE requests with "not yet initialized"

### Per-Request Direct Queries

Unlike a cache-based approach, the webhook does not maintain persistent informers
for dependent resources. Instead, on each DELETE admission request, it constructs
a temporary dynamic client scoped to the consumer workspace via the kcp front-proxy
and lists dependent resources directly.

The webhook derives the front-proxy base URL from its kubeconfig at startup by
stripping the `/clusters/...` workspace path suffix. For each admission request,
it builds a workspace-scoped URL: `{frontProxyBase}/clusters/{logicalClusterName}`.

This approach is routing-transparent across shards -- no webhook configuration
change is required when shards are added, as the front-proxy handles routing
based on the logical cluster name. Each new shard does, however, need the
`system:admin` RBAC binding (see [Workspace Topology](#workspace-topology))
applied so the webhook is authorized to list dependents on it.

### Admission Request Flow

When kcp dispatches a DELETE request to the webhook, the
[`DeletionValidator.Handle`](../internal/webhook/deletion_validator.go)
method processes it:

```
DELETE vpcs/my-vpc (from consumer workspace)
   |
   v
1. Non-DELETE? --> Allow
   |
2. Not initialized? --> Deny ("retry later")
   |
3. Parse object from request (OldObject for DELETE)
   |
4. Has skip-protection annotation? --> Allow
   |
5. Extract logical cluster name from kcp.io/cluster annotation
   |
6. Registry.FindByTargetGVR(vpcs GVR)
   |  returns []RuleEntry with matched IndexedFields
   |
7. Create dynamic client for {frontProxy}/clusters/{clusterName}
   |
8. For each matching rule:
   |  a. List dependent resources in namespace
   |  b. Filter by field path (fieldpath.Resolve == deleted resource name)
   |  c. Each match is a blocker: "VirtualMachine/my-vm"
   |
9. Blockers found? --> Deny ("still referenced by VirtualMachine/my-vm")
   |
10. No blockers --> Allow
```

The validator is rule-agnostic -- it doesn't need to know the structure of each
`DependencyRule`, only how to query the dependent resources via the GVR and field
paths stored in `RuleEntry`.

### Force-Deleting Protected Resources

If the dependency lifecycle has broken down (stale rules, crashed webhook),
operators can bypass protection:

```sh
kubectl annotate vpc my-vpc dependencies.opendefense.cloud/skip-protection=true
kubectl delete vpc my-vpc
```

The webhook checks for this annotation
([`AnnotationSkipProtection`](../internal/webhook/deletion_validator.go))
early in the handler and allows deletion regardless of active dependents.

---

## Key Data Structures

### DependencyRule (`api/v1alpha1/types.go`)

```yaml
apiVersion: dependencies.opendefense.cloud/v1alpha1
kind: DependencyRule
metadata:
  name: vm-dependencies
spec:
  dependent:
    apiExportName: "compute.test.io"
    group: compute.test.io
    version: v1
    kind: VirtualMachine
    resource: virtualmachines
  dependencies:
    - apiExportRef:
        path: "root:network-provider"
        name: "network.test.io"
      group: network.test.io
      version: v1
      resource: vpcs
      fieldRef:
        path: ".spec.vpcRef.name"
```

`spec.dependent` -- the resource type that holds references (the one queried on deletion).
`spec.dependent.apiExportName` -- the APIExport in the same workspace that provides this type.
`spec.dependent.resource` -- the plural resource name, used to construct the GVR for dynamic client queries.
`spec.dependencies[]` -- the resource types being referenced (the ones being protected).
`spec.dependencies[].fieldRef.path` -- where in the dependent resource the reference lives.

### RuleRegistry (`internal/webhook/rule_registry.go`)

Thread-safe metadata store shared between the `RuleCacheManager` (writer) and
`DeletionValidator` (reader).

- `rules map[string]*RuleState` -- keyed by `clusterName/ruleName`
- `byTarget map[GVR][]string` -- reverse index from protected GVR to rule keys,
  rebuilt on every `Register`/`Unregister`

Key operations:
- `Register(key, state) *RuleState` -- adds/replaces a rule, returns old state
- `Unregister(key)` -- removes the rule
- `FindByTargetGVR(gvr) []RuleEntry` -- O(1) lookup used by the admission handler

### Field Path Resolution (`internal/fieldpath/fieldpath.go`)

[`fieldpath.Resolve`](../internal/fieldpath/fieldpath.go) extracts a string value
from an unstructured object given a dot-notation path (e.g., `.spec.vpcRef.name`).
Used by the webhook to filter dependent resources by matching field values against
the deleted resource name.

---

## Known Limitations

### Dependency-provider workspaces must be direct children of root

The path named in `DependencyRule.spec.dependencies[].apiExportRef.path`
must currently be a direct child of `root` (e.g., `root:network-provider`).
Nested paths such as `root:org:network-provider` will fail to resolve.

The cause is in
[`workspaceResolver.ensureResolved`](../internal/controller/dependencyrule_controller.go):
the resolver splits the supplied path on `:`, takes only the last segment as
the `Workspace` name, and looks that name up in the **root** workspace. A
nested workspace will not be found there.

This restriction applies **only** to the workspace hosting the protected
APIExport. It does **not** apply to:

- Consumer workspaces — the webhook identifies them by the `kcp.io/cluster`
  annotation on the admission request (a logical cluster name, not a path)
  and queries them directly through the front-proxy.
- The dependent-provider workspace (where the `DependencyRule` itself
  lives) — the controller and webhook discover rules through the dep-ctrl
  APIExport's multicluster watch, which delivers the logical cluster name
  with each event.
- The dep-ctrl workspace — its location is pinned at deploy time via the
  controller's kubeconfig.

Lifting this limitation requires the resolver to walk the path segment by
segment (or call kcp's path-aware Workspace lookup) instead of bottoming
out at root.

### Circular dependencies

The system does not detect cycles. If rule A says VM depends on VPC and rule B
says VPC depends on VM, neither can be deleted normally. Use `skip-protection`
to break the cycle.

### Per-request query latency

Each DELETE admission request triggers a live API call to list dependent resources
in the consumer workspace. This adds latency compared to a cache-based approach,
but DELETE operations are infrequent and the namespace-scoped listings are
typically small.

### Rule updates

In-place `DependencyRule` spec edits are picked up automatically: the
`RuleCacheManager` reconciles on each update event and overwrites the
existing registry entry, and `WebhookInstaller.reconcileWorkspaceWebhook`
recomputes the desired webhook config from scratch on every change rather
than maintaining incremental state. This is covered by the
*"should propagate in-place DependencyRule updates to the webhook"* e2e
scenario, which patches `fieldRef.path` on a live rule and verifies the
webhook re-evaluates without a recreate cycle.
