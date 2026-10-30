FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS builder

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

FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99 AS controller
WORKDIR /
COPY --from=controller-builder /workspace/bin/dependency-controller .
USER 65532:65532
ENTRYPOINT ["/dependency-controller"]

FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99 AS webhook
WORKDIR /
COPY --from=webhook-builder /workspace/bin/dependency-webhook .
USER 65532:65532
ENTRYPOINT ["/dependency-webhook"]

# Combined image with both binaries (used by e2e tests and single-image deployments).
FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
WORKDIR /
COPY --from=controller-builder /workspace/bin/dependency-controller .
COPY --from=webhook-builder /workspace/bin/dependency-webhook .
USER 65532:65532
