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

Please make sure you have the REGISTRY and PROJECT_ID environment variables set.
For local builds these can be set to anything.
For image builds these determine the location of your image.
For GCE the registry should be gcr.io and PROJECT_ID should be the project you
want to use the images in.

### Local builds

```console
make clean
make certs
make build
```

### Build images

```console
build docker/proxy-server
build docker/proxy-agent
```

## Examples

Currently there is only a [basic example](docs/basic_example.md) and a [simple Kubernetes integration](docs/kubernetes_setup.md).

## Troubleshoot

### Undefined ProtoPackageIsVersion3
As explained in https://github.com/golang/protobuf/issues/763#issuecomment-442767135,
protoc-gen-go binary has to be built from the vendored version:

```console
go install ./vendor/github.com/golang/protobuf/protoc-gen-go
make gen
```

