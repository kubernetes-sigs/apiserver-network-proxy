# Build the proxy-server binary

ARG GO_TOOLCHAIN
ARG GO_VERSION
ARG BASEIMAGE

FROM ${GO_TOOLCHAIN}:${GO_VERSION} as builder

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
ARG ARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${ARCH} go build -mod=vendor -v -a -ldflags '-extldflags "-static"' -o proxy-server sigs.k8s.io/apiserver-network-proxy/cmd/server

FROM ${BASEIMAGE}

WORKDIR /
COPY --from=builder /go/src/sigs.k8s.io/apiserver-network-proxy/proxy-server .
ENTRYPOINT ["/proxy-server"]
