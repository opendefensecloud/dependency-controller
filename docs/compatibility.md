# Compatibility

Which kcp versions this controller is built against, which are covered by tests, and
what happens outside that range.

## Support matrix

| kcp | Kubernetes | Status | Covered by |
|---|---|---|---|
| 0.32.x | 1.36 | **Supported** | `make test`, `make test-e2e` |
| 0.31.x | 1.35 | **Supported** | `make test`, `make test-e2e` |
| 0.30.x | 1.34 | **Best effort** | `make test` only |
| ≤ 0.29.x | ≤ 1.33 | **Not supported** | — |

**Supported** — exercised by the test matrix and gated in CI. Report bugs.

**Best effort** — the test suite passes, but the combination sits outside the
Kubernetes client/server version skew policy (see below). It may work for your
workload; verify it yourself and expect no guarantee.

**Not supported** — never exercised. It may still work, but nothing here checks it
and no compatibility fix will be accepted for it.

## Why the boundary sits there

The controller is built against `k8s.io/client-go` v0.36, which is derived from the
kcp SDK version in `go.mod`: the kcp 0.31.x SDK line pins Kubernetes 1.35, and 0.32.x
pins 1.36. The client library version is therefore not independently choosable — it
moves with the kcp SDK.

Kubernetes supports a client that is at most **one minor ahead** of the API server.
That gives:

- kcp 0.32 (k8s 1.36) — client and server aligned.
- kcp 0.31 (k8s 1.35) — client one minor ahead, inside the policy.
- kcp 0.30 (k8s 1.34) — client two minors ahead, **outside** the policy. The
  integration suite passes, which is why this is "best effort" rather than
  "not supported", but the skew guarantee does not apply.

Older kcp releases fall further outside and are not tested at all.

## Running the matrix

`KCP_VERSION` selects the kcp server the tests run against; `KCP_VERSIONS` is the
list the matrix targets iterate over. Both are overridable:

```sh
make test                              # primary version only (KCP_VERSION)
make test KCP_VERSION=0.30.3           # one specific version
make test-matrix                       # every version in KCP_VERSIONS

make test-e2e                          # primary version (KCP_VERSION)
make test-e2e-kcp-matrix               # every version in KCP_VERSIONS
E2E_KCP_VERSION=0.30.3 make test-e2e   # one specific version
E2E_KCP_VERSION= make test-e2e         # whatever kcp-operator defaults to
```

For `make test`, `KCP_VERSION` picks the kcp binary downloaded to `bin/`. For the e2e
suite, `E2E_KCP_VERSION` sets `spec.image.tag` on the kcp-operator `RootShard`, `Shard`
and `FrontProxy` resources, and defaults to `KCP_VERSION`.

That default matters: kcp-operator ships its own pinned kcp version (chart 0.7.3 runs
operator v0.7.2, which defaults to kcp v0.31.1). Without pinning the image explicitly,
the e2e suite would silently exercise a kcp version this project makes no claim about.
Set `E2E_KCP_VERSION=` (empty) to fall back to the operator's default deliberately.

`make test-e2e-kcp-matrix` is independent of `make test-e2e-matrix`, which iterates
over shard topologies (`single-shard`, `multi-shard`) rather than kcp versions.

## What the tests actually cover

The integration suite (`make test`) starts a real kcp server and exercises the paths
this controller depends on: workspace creation under `root`, `APIExport` and
`APIResourceSchema` application, `APIBinding` into consumer workspaces, and the
`multicluster-provider` virtual-workspace client.

It does not cover everything a deployment touches — leader election, metrics and
health endpoints, and the kcp-operator deployment path are only reached by
`make test-e2e`. A green `make test` on a given kcp version therefore says the API
surface this controller uses is compatible, not that the full deployment is.
