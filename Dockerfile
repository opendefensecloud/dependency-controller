FROM --platform=$BUILDPLATFORM golang:1.27.1@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa AS builder

WORKDIR /workspace
RUN go env -w GOMODCACHE=/root/.cache/go-build

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS
ARG TARGETARCH

RUN mkdir bin

FROM builder AS controller-builder
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o bin/dependency-controller ./cmd/controller/

FROM builder AS webhook-builder
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o bin/dependency-webhook ./cmd/webhook/

FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478 AS controller
WORKDIR /
COPY --from=controller-builder /workspace/bin/dependency-controller .
USER 65532:65532
ENTRYPOINT ["/dependency-controller"]

FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478 AS webhook
WORKDIR /
COPY --from=webhook-builder /workspace/bin/dependency-webhook .
USER 65532:65532
ENTRYPOINT ["/dependency-webhook"]

# Combined image with both binaries (used by e2e tests and single-image deployments).
FROM gcr.io/distroless/static:nonroot@sha256:d29e660cc75a5b6b1334e03c5c81ccf9bc0884a002c6000dbf0fb96034814478
WORKDIR /
COPY --from=controller-builder /workspace/bin/dependency-controller .
COPY --from=webhook-builder /workspace/bin/dependency-webhook .
USER 65532:65532
