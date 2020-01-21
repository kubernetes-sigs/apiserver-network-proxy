# Build the proxy-agent binary
FROM golang:1.13.4 as builder

# Copy in the go src
WORKDIR /go/src/sigs.k8s.io/apiserver-network-proxy

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# Cache dependencies
RUN go mod download

# Copy the sources
COPY pkg/    pkg/
COPY cmd/    cmd/
COPY proto/  proto/

# Build
ARG ARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${ARCH} go build -a -ldflags '-extldflags "-static"' -o proxy-agent sigs.k8s.io/apiserver-network-proxy/cmd/agent

# Copy the loader into a thin image
FROM scratch
WORKDIR /
COPY --from=builder /go/src/sigs.k8s.io/apiserver-network-proxy/proxy-agent .
ENTRYPOINT ["/proxy-agent"]
