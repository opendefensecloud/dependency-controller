# ADR-001: kcp support follows the lines kcp still patches

- **Status:** Accepted
- **Date:** 2026-11-11
- **Issue:** #195

## Context

The controller supported kcp 0.32.x and 0.31.x, with 0.30.x as best effort
(see `docs/compatibility.md` before this change). Those tiers were drawn from what
the test suites exercise and from client-go skew, not from upstream support.

kcp publishes no support policy. Its security releases show the actual pattern:
fixes ship on the newest one or two lines only.

- GHSA-c8w2-fgvx-vhv4 (critical, 2026-06): fixed in 0.32.2 and 0.31.4; 0.30.x left
  vulnerable.
- Advisories published 2026-11-09, nine in total, including the critical
  GHSA-2qcf-f968-jcq3 and the high-severity GHSA-2h54-wjqv-9qqp,
  GHSA-rj35-2mmm-h9h9 and GHSA-3g7x-x96g-62gq: fixed in 0.33.x only (0.33.2 up to
  0.33.6). No 0.32.x, 0.31.x or 0.30.x release followed; the last 0.32 release is
  0.32.5 from 2026-09-22.

The kcp maintainers recommended upgrading to 0.33.3. The advisory data shows that is
not sufficient: GHSA-2qcf-f968-jcq3 affects everything up to 0.33.4 and is fixed
only in 0.33.6.

Testing a line says the controller's API surface works against it. It says nothing
about whether running that line is safe, and a "Supported" label reads as an
endorsement of the deployment.

## Decision

The project supports only kcp lines that still receive security fixes upstream.
A line drops out of support once kcp ships a security fix without backporting it to
that line.

Applied now: kcp 0.33.x is the only supported line, tested at 0.33.6, and 0.33.6 is
the documented minimum. 0.32.x and older are not supported. There is no best-effort
tier for unpatched lines.

The kcp SDK in `go.mod`, `apigen` in `tools.lock`, `KCP_VERSION` / `KCP_VERSIONS` in
the Makefile, the CI test matrix, and the e2e kcp-operator chart move together when
the supported line changes.

## Consequences

- Deployments on kcp ≤ 0.32 must upgrade to 0.33.6 or a later 0.33.x patch release
  to stay supported. A new kcp minor line (0.34 onwards) is not covered until it is
  added to the test matrix and `docs/compatibility.md`.
- The CI matrix shrinks to one kcp version, so CI is faster but no longer detects
  regressions against older lines. That is intended: such regressions would not be
  fixed.
- Support can now end at short notice, driven by upstream advisories rather than
  by this project's release cadence. Each kcp security release needs a check of
  which lines it patched, and `docs/compatibility.md` updated to match.
- The e2e suite moves to kcp-operator chart 0.7.13 (operator v0.10.3), the operator
  release published alongside the 0.33 fixes.
