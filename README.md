# apiserver-network-proxy

Created due to https://github.com/kubernetes/org/issues/715.

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack channel](https://kubernetes.slack.com/messages/sig-cloud-provider)
- [Mailing list](https://groups.google.com/forum/#!forum/kubernetes-sig-cloud-provider)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

## Build

```console
make clean
make certs
make build
```

## Examples

The current example runs the two actual services as well as a sample client on one end and a sample destination for requests on the other. 
- *Proxy service:* The proxy service takes the API server requests and forwards them appropriately.
- *Agent service:* The agent service connects to the proxy and then allows traffic to be forwarded to it.

### mTLS Proxy with dial back Agent

```
client ==> (:8090) proxy (:8091) <== agent ==> SimpleHTTPServer(:8000)
  |                                                    ^
  |                          Tunnel                    |
  +----------------------------------------------------+
```

- Start SimpleHTTPServer (Sample destination)
```console
python -m SimpleHTTPServer
```

- Start proxy service
```
./bin/proxy-server --serverCaCert=certs/master/issued/ca.crt --serverCert=certs/master/issued/proxy-master.crt --serverKey=certs/master/private/proxy-master.key --clusterCaCert=certs/agent/issued/ca.crt --clusterCert=certs/agent/issued/proxy-master.crt --clusterKey=certs/agent/private/proxy-master.key
```

- Start agent service
```
./bin/proxy-agent --caCert=certs/agent/issued/ca.crt --agentCert=certs/agent/issued/proxy-agent.crt --agentKey=certs/agent/private/proxy-agent.key
```

- Run client (mTLS enabled sample client)
```
./bin/proxy-test-client --caCert=certs/master/issued/ca.crt --clientCert=certs/master/issued/proxy-client.crt --clientKey=certs/master/private/proxy-client.key
```

## Troubleshoot

### Undefined ProtoPackageIsVersion3
As explained in https://github.com/golang/protobuf/issues/763#issuecomment-442767135,
protoc-gen-go binary has to be built from the vendored version:

```console
go install ./vendor/github.com/golang/protobuf/protoc-gen-go
make gen
```

