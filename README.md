# apiserver-network-proxy

Created due to https://github.com/kubernetes/org/issues/715.

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack channel](https://kubernetes.slack.com/messages/sig-cloud-provider)
- [Mailing list](https://groups.google.com/forum/#!forum/kubernetes-sig-cloud-provider)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

## Examples

There are 2 modes to run the examples:
- client/server mode
- agent dial back mode

### Client/Server mode

```
client ==> server(:8090) ==> SimpleHTTPServer(:8000)
```

- Start SimpleHTTPServer
```console
python -m SimpleHTTPServer
```

- Start server
```
go run examples/server/main.go
```

- Run client
```
go run examples/client/main.go
```

### Agent dial back mode

```
client ==> (:8090) agentserver (:8091) <== agentclient ==> SimpleHTTPServer(:8000)
  |                                                                           ^
  |                               Tunnel                                      |
  +---------------------------------------------------------------------------+
```

- Start SimpleHTTPServer
```console
python -m SimpleHTTPServer
```

- Start agentserver
```
go run examples/agentserver/main.go
```

- Start agentclient
```
go run examples/agentclient/main.go
```

- Run client
```
go run examples/client/main.go
```
