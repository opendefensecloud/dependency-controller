# Compatibility

Which kcp versions this controller is built against, which are covered by tests, and
what happens outside that range.

## Support matrix

| kcp | Kubernetes | Status | Covered by |
|---|---|---|---|
| 0.32.x | 1.36 | **Supported** | `make test`, `make test-e2e` |
| 0.31.x | 1.35 | **Supported** | `make test`, `make test-e2e` |
| 0.30.x | 1.34 | **Best effort** | `make test` |
| ≤ 0.29.x | ≤ 1.33 | **Not supported** | — |

Tested at 0.32.3, 0.31.6 and 0.30.3 — the versions listed in `KCP_VERSIONS`. Other
patch releases within a supported line are expected to work but are not exercised.

**Supported** — `make test` runs in CI on every PR, and `make test-e2e` has been run
against it. Report bugs.

**Best effort** — `make test` runs in CI on every PR and passes, but e2e does not
cover it and client-go sits two minors ahead of the server. It may work for your
workload; verify it yourself and expect no guarantee.

**Not supported** — never exercised. It may still work, but nothing here checks it
and no compatibility fix will be accepted for it.

## Why the boundary sits there

The controller is built against `k8s.io/client-go` v0.36, which is not independently
choosable — it moves with the kcp SDK version in `go.mod`: the kcp 0.31.x SDK line
pins Kubernetes 1.35, and 0.32.x pins 1.36.

client-go publishes no version-skew guarantee. Its compatibility matrix marks an
exact client/server match as "exactly the same features / API objects", and marks
*every* other combination identically — "everything they have in common (i.e., most
APIs) will work" — with no distinction between one minor apart and several. The
boundary below is therefore this project's own, drawn from what it tests rather than
from an upstream promise:

- kcp 0.32 (k8s 1.36) — client and server match exactly.
- kcp 0.31 (k8s 1.35) — client one minor ahead; integration and e2e suites pass.
- kcp 0.30 (k8s 1.34) — client two minors ahead. The integration suite passes, which
  is why this is "best effort" rather than "not supported", but e2e does not cover it.

Older kcp releases are not tested at all.

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
