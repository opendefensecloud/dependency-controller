FROM --platform=$BUILDPLATFORM golang:1.26.3@sha256:313faae491b410a35402c05d35e7518ae99103d957308e940e1ae2cfa0aac29b AS builder

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

FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240 AS controller
WORKDIR /
COPY --from=controller-builder /workspace/bin/dependency-controller .
USER 65532:65532
ENTRYPOINT ["/dependency-controller"]

FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240 AS webhook
WORKDIR /
COPY --from=webhook-builder /workspace/bin/dependency-webhook .
USER 65532:65532
ENTRYPOINT ["/dependency-webhook"]

# Combined image with both binaries (used by e2e tests and single-image deployments).
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
WORKDIR /
COPY --from=controller-builder /workspace/bin/dependency-controller .
COPY --from=webhook-builder /workspace/bin/dependency-webhook .
USER 65532:65532
