# Build the client binary

ARG BUILDARCH
ARG GO_TOOLCHAIN
ARG GO_VERSION
ARG BASEIMAGE

FROM --platform=linux/${BUILDARCH} ${GO_TOOLCHAIN}:${GO_VERSION} as builder

# Copy in the go src
WORKDIR /go/src/sigs.k8s.io/apiserver-network-proxy

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# We have a replace directive for konnectivity-client in go.mod
COPY konnectivity-client/ konnectivity-client/

# Copy vendored modules
COPY vendor/ vendor/

# Copy the sources
COPY pkg/    pkg/
COPY cmd/    cmd/
COPY proto/  proto/

# Build
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -mod=vendor -v -a -ldflags '-extldflags "-static"' -o proxy-test-client sigs.k8s.io/apiserver-network-proxy/cmd/test-client

FROM ${BASEIMAGE}

WORKDIR /
COPY --from=builder /go/src/sigs.k8s.io/apiserver-network-proxy/proxy-test-client .
ENTRYPOINT ["/proxy-test-client"]
