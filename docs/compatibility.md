# Compatibility

Which kcp versions this controller is built against, which are covered by tests, and
what happens outside that range.

## Support matrix

| kcp | Kubernetes | Status | Covered by |
|---|---|---|---|
| 0.33.x (≥ 0.33.6) | 1.36 | **Supported** | `make test`, `make test-e2e` |
| ≤ 0.32.x | ≤ 1.36 | **Not supported** | — |

Tested at 0.33.6, the version listed in `KCP_VERSIONS`. Run kcp 0.33.6 or a later
0.33.x patch release: earlier 0.33 patch releases are affected by security advisories
fixed in 0.33.6 (see below).

**Supported** — `make test` runs in CI on every PR, and `make test-e2e` has been run
against it. Report bugs.

**Not supported** — not exercised. It may still work, but nothing here checks it and
no compatibility fix will be accepted for it.

## Why the boundary sits there

The boundary follows the kcp lines that still receive security fixes upstream. kcp
publishes no support policy, and in practice ships fixes only on its newest lines.
The advisories published on 2026-11-09 — among them the critical
[GHSA-2qcf-f968-jcq3](https://github.com/kcp-dev/kcp/security/advisories/GHSA-2qcf-f968-jcq3)
and the high-severity
[GHSA-2h54-wjqv-9qqp](https://github.com/kcp-dev/kcp/security/advisories/GHSA-2h54-wjqv-9qqp)
and [GHSA-rj35-2mmm-h9h9](https://github.com/kcp-dev/kcp/security/advisories/GHSA-rj35-2mmm-h9h9)
— were fixed in 0.33.x only, with no 0.32.x, 0.31.x or 0.30.x release. Supporting a
line that cannot be patched would mean recommending a vulnerable deployment. See
[ADR-001](../architecture/ADR-001-kcp-support-follows-patched-lines.md).

The controller is built against `k8s.io/client-go` v0.36, which is not independently
choosable — it moves with the kcp SDK version in `go.mod`. The kcp 0.33.x SDK line
pins Kubernetes 1.36, so client and server match exactly.

## Running the matrix

`KCP_VERSION` selects the kcp server the tests run against; `KCP_VERSIONS` is the
list the matrix targets iterate over. Both are overridable:

```sh
make test                              # primary version only (KCP_VERSION)
make test KCP_VERSION=0.33.6           # one specific version
make test-matrix                       # every version in KCP_VERSIONS

make test-e2e                          # primary version (KCP_VERSION)
make test-e2e-kcp-matrix               # every version in KCP_VERSIONS
E2E_KCP_VERSION=0.33.6 make test-e2e   # one specific version
E2E_KCP_VERSION= make test-e2e         # whatever kcp-operator defaults to
```

For `make test`, `KCP_VERSION` picks the kcp binary downloaded to `bin/`. For the e2e
suite, `E2E_KCP_VERSION` sets `spec.image.tag` on the kcp-operator `RootShard`, `Shard`
and `FrontProxy` resources, and defaults to `KCP_VERSION`.

That default matters: kcp-operator ships its own pinned kcp version (chart 0.7.13 runs
operator v0.10.3, which defaults to kcp v0.33.6). The two happen to match today, but
they drift independently. Without pinning the image explicitly, a later operator bump
would silently change which kcp version the e2e suite exercises.
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
