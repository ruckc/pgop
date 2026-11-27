# Build the manager binary
# golang:1.27.1
FROM golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
ARG GO_BUILD_FLAGS=""
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build ${GO_BUILD_FLAGS} -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
# debian:stable-slim
FROM debian:stable-slim@sha256:a64d9dbf633e2a2503afac76be521dc25d9b5765150b30d31a3914d4ae969cab
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
