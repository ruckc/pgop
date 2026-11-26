# Build the manager binary
# golang:1.27.1
FROM golang:1.27.4@sha256:e65b9442d278803007162df7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS builder
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
FROM debian:stable-slim@sha256:97df7a62b82338f3c67d3a6f6c847ba44759f9c1b825cf58d3239d95a5478700 AS runtime
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
