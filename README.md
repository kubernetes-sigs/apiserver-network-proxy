# apiserver-network-proxy

Created due to https://github.com/kubernetes/org/issues/715.

See [the KEP proposal for architecture and details](https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/1281-network-proxy#proposal).

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack channel](https://kubernetes.slack.com/messages/apiserver-network-proxy)
- [Mailing list](https://groups.google.com/forum/#!forum/kubernetes-sig-cloud-provider)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

## Releases

To reduce version-skew risk, it is recommended to match apiserver-network-proxy server & client
minor release versions. With Kubernetes, this means:

- Kubernetes v1.27+: `0.1.X` tags, `master` branch
- Kubernetes versions v1.23 to v1.27: `0.0.X` tags, `release-0.0` branch
- Kubernetes versions up to v1.23: apiserver-network-proxy versions up to `v0.0.30`.
  Refer to the kubernetes go.mod file for the specific release version.

## Build

Please make sure you have the REGISTRY and PROJECT_ID environment variables set.
For local builds these can be set to anything.
For image builds these determine the location of your image.
For GCE the registry should be gcr.io and PROJECT_ID should be the project you
want to use the images in.

### Mockgen

The [```mockgen```](https://github.com/golang/mock) tool must be installed on your system.

### Protoc

Proto definitions are compiled with `protoc`. Please ensure you have protoc installed ([Instructions](https://grpc.io/docs/languages/go/quickstart/)) and the `protoc-gen-go` and `protoc-gen-go-grpc` libraries at the appropriate version.

Currently, we are using protoc-gen-go@v1.27.1

`go get google.golang.org/protobuf/cmd/protoc-gen-go@v1.27.1`

Currently, we are using protoc-gen-go-grpc@v1.2

`go get google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.2`

### Local builds

```console
make clean
make certs
make gen
make build
```

### Build images

```console
make docker-build
```

## Examples

The current examples run two actual services as well as a sample client on one end and a sample destination for
requests on the other.
- *Proxy service:* The proxy service takes the API server requests and forwards them appropriately.
- *Agent service:* The agent service connects to the proxy and then allows traffic to be forwarded to it.

### GRPC Client using mTLS Proxy with dial back Agent

```
Frontend client =HTTP over GRPC=> (:8090) proxy (:8091) <=GRPC= agent =HTTP=> http-test-server(:8000)
  |                                                               ^
  |                               Tunnel                          |
  +---------------------------------------------------------------+
```

- Start Simple test HTTP Server (Sample destination)
```console
./bin/http-test-server
```

- Start proxy service
```console
./bin/proxy-server --server-ca-cert=certs/frontend/issued/ca.crt --server-cert=certs/frontend/issued/proxy-frontend.crt --server-key=certs/frontend/private/proxy-frontend.key --cluster-ca-cert=certs/agent/issued/ca.crt --cluster-cert=certs/agent/issued/proxy-frontend.crt --cluster-key=certs/agent/private/proxy-frontend.key
```

- Start agent service
```console
./bin/proxy-agent --ca-cert=certs/agent/issued/ca.crt --agent-cert=certs/agent/issued/proxy-agent.crt --agent-key=certs/agent/private/proxy-agent.key
```

- Run client (mTLS enabled sample client)
```console
./bin/proxy-test-client --ca-cert=certs/frontend/issued/ca.crt --client-cert=certs/frontend/issued/proxy-client.crt --client-key=certs/frontend/private/proxy-client.key
```

### GRPC+UDS Client using Proxy with dial back Agent

```
Frontend client =HTTP over GRPC+UDS=> (/tmp/uds-proxy) proxy (:8091) <=GRPC= agent =HTTP=> SimpleHTTPServer(:8000)
  |                                                                            ^
  |                                     Tunnel                                 |
  +----------------------------------------------------------------------------+
```

- Start Simple test HTTP Server (Sample destination)
```console
./bin/http-test-server
```

- Start proxy service
```console
./bin/proxy-server --server-port=0 --uds-name=/tmp/uds-proxy --cluster-ca-cert=certs/agent/issued/ca.crt --cluster-cert=certs/agent/issued/proxy-frontend.crt --cluster-key=certs/agent/private/proxy-frontend.key
```

- Start agent service
```console
./bin/proxy-agent --ca-cert=certs/agent/issued/ca.crt --agent-cert=certs/agent/issued/proxy-agent.crt --agent-key=certs/agent/private/proxy-agent.key
```

- Run client (mTLS enabled sample client)
```console
./bin/proxy-test-client --proxy-port=0 --proxy-uds=/tmp/uds-proxy --proxy-host=""
```


### HTTP-Connect Client using mTLS Proxy with dial back Agent (Either curl OR test client)

```
Frontend client =HTTP-CONNECT=> (:8090) proxy (:8091) <=GRPC= agent =HTTP=> SimpleHTTPServer(:8000)
  |                                                             ^
  |                              Tunnel                         |
  +-------------------------------------------------------------+
```

- Start SimpleHTTPServer (Sample destination)
```console
./bin/http-test-server
```

- Start proxy service
```console
./bin/proxy-server --mode=http-connect --server-ca-cert=certs/frontend/issued/ca.crt --server-cert=certs/frontend/issued/proxy-frontend.crt --server-key=certs/frontend/private/proxy-frontend.key --cluster-ca-cert=certs/agent/issued/ca.crt --cluster-cert=certs/agent/issued/proxy-frontend.crt --cluster-key=certs/agent/private/proxy-frontend.key
```

- Start agent service
```console
./bin/proxy-agent --ca-cert=certs/agent/issued/ca.crt --agent-cert=certs/agent/issued/proxy-agent.crt --agent-key=certs/agent/private/proxy-agent.key
```

- Run client (mTLS & http-connect enabled sample client)
```console
./bin/proxy-test-client --mode=http-connect  --proxy-host=127.0.0.1 --ca-cert=certs/frontend/issued/ca.crt --client-cert=certs/frontend/issued/proxy-client.crt --client-key=certs/frontend/private/proxy-client.key
```

- Run curl client (curl using a mTLS http-connect proxy)
```console
curl -v -p --proxy-key certs/frontend/private/proxy-client.key --proxy-cert certs/frontend/issued/proxy-client.crt --proxy-cacert certs/frontend/issued/ca.crt --proxy-cert-type PEM -x https://127.0.0.1:8090  http://localhost:8000/success
```

### Running on kubernetes
See following [README.md](examples/kubernetes/README.md)

### Clients

`apiserver-network-proxy` components are intended to run as standalone binaries and should not be imported as a library. Clients communicating with the network proxy can import the `konnectivity-client` module.
