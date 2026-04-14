FROM golang:1.26-alpine AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/dependency-controller ./cmd/controller/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/dependency-webhook ./cmd/webhook/

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/dependency-controller /dependency-controller
COPY --from=builder /out/dependency-webhook /dependency-webhook

USER 65532:65532
