# Build the manager binary
FROM --platform=$BUILDPLATFORM golang:1.24.5-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG GO_BUILD_TAGS
ARG VERSION=dev

WORKDIR /istio-build
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/
COPY internal/ internal/
COPY pkg/ pkg/
COPY cmd/ cmd/

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -tags "${GO_BUILD_TAGS}" -ldflags="-s -w -X github.com/kyma-project/istio/operator/internal/resources.version=${VERSION}" -o manager main.go
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o istio_install cmd/istio-install/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder /istio-build/manager .
COPY --from=builder /istio-build/istio_install .

USER 65532:65532

ENTRYPOINT ["/manager"]
