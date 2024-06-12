# Set up KIND cluster with multiple KCP and worker nodes running konnectivity

Change to the `examples/kind-multinode-kcp` folder and run `./quickstart-kind`. This script
performs the following operations:

1. Render config templates in `templates/` using provided values.
2. Create a new `kind` cluster with the desired number of KCP and worker nodes.
3. Changes `kubectl` context to point to the new `kind` cluster.
4. Deploys `konnectivity` proxy servers and agents to the KCP and worker nodes.

To validate that it works, run a custom image and get pod logs (it goes through the konnectivity proxy):
```sh
$ kubectl run test --image httpd:2
pod/test created
$ kubectl get pods
NAME   READY   STATUS              RESTARTS   AGE
test   0/1     ContainerCreating   0          4s
$ kubectl get pods
NAME   READY   STATUS    RESTARTS   AGE
test   1/1     Running   0          6s
$ kubectl logs test
...
[Tue Apr 09 20:58:36.756720 2024] [mpm_event:notice] [pid 1:tid 139788897408896] AH00489: Apache/2.4.59 (Unix) configured -- resuming normal operations
```

## `./quickstart-kind.sh` command-line flags
- `--cluster-name <NAME>`: Name of the `kind` cluster to be created Default: `knp-test-cluster`
- `--overwrite-cluster`: Overwrite existing `kind` cluster if necessary. Default: do not overwrite.
- `--server-image <IMAGE_NAME>[:<IMAGE_TAG>]`: Proxy server image to deploy. Default: `gcr.io/k8s-staging-kas-network-proxy/proxy-server:master`
- `--agent-image <IMAGE_NAME>[:<IMAGE_TAG>]`: Proxy server image to deploy. Default: `gcr.io/k8s-staging-kas-network-proxy/proxy-agent:master`
- `--num-kcp-nodes <NUM>`: Number of control plane nodes to spin up. Default: 2.
- `--num-worker-nodes <NUM>`: Number of worker nodes to spin up. Default: 1.
- `--sideload-images`: Use `kind load ...` to sideload custom proxy server and agent images with the names set by `--server-image` and `--agent-image` into the kind cluster. Default: do not sideload.
  - Use this if you don't want to publish your custom KNP images to a public registry.
  - NOTE: You MUST specify an image tag (i.e. `my-image-name:my-image-tag` and not just `my-image-name`) and the image tag MUST NOT be `:latest` for this to work! See [`kind` docs](https://kind.sigs.k8s.io/docs/user/quick-start/#loading-an-image-into-your-cluster) for why this is necessary.

## Example usage to deploy custom local KNP images
In the repo root, build KNP and its docker images with the following:
```shell
make clean
make certs
make gen
make build
make docker-build
```

Verify that the new images are available in the local docker registry with `docker images`. Then, bring up the cluster:

```shell
cd examples/kind-multinode-kcp

# These are the default values of the registry, image name, and tag used by the Makefile.
# Edit them if necessary.
REGISTRY=gcr.io/$(gcloud config get-value project)
TAG=$(git rev-parse HEAD)
TARGET_ARCH="amd64"
SERVER_IMAGE="$REGISTRY/proxy-server-$TARGET_ARCH:$TAG"
AGENT_IMAGE="$REGISTRY/proxy-agent-$TARGET_ARCH:$TAG"

# Bring up the cluster!
./quickstart-kind.sh --cluster-name custom-knp-test --server-image "$SERVER_IMAGE" --agent-image "$AGENT_IMAGE" \
  --num-kcp-nodes 3 --num-worker-nodes 2 --sideload-images
```

Check that the `konnectivity` pods are up and running:
```shell
kubectl --namespace kube-system get pods | grep konnectivity
# Output:
# konnectivity-agent-4db5j                                 1/1     Running   0          34m
# konnectivity-agent-c7gj5                                 1/1     Running   0          34m
# konnectivity-agent-h86l9                                 1/1     Running   0          34m
# konnectivity-server-9bl45                                1/1     Running   0          34m
# konnectivity-server-dcfz8                                1/1     Running   0          34m
# konnectivity-server-klww5                                1/1     Running   0          34m
# konnectivity-server-nrfz8                                1/1     Running   0          34m
```

Then create a test pod on a worker node and verify you can get logs from it:
```shell
kubectl run test --image httpd:2
# Output:
# pod/test created
kubectl get pods
# Output:
# NAME   READY   STATUS    RESTARTS   AGE
# test   1/1     Running   0          34s
kubectl logs test
# Output:
# AH00558: httpd: Could not reliably determine the server's fully qualified domain name, using 10.244.5.3. Set the 'ServerName' directive globally to suppress this message
# AH00558: httpd: Could not reliably determine the server's fully qualified domain name, using 10.244.5.3. Set the 'ServerName' directive globally to suppress this message
# [Wed Jun 12 20:42:06.471169 2024] [mpm_event:notice] [pid 1:tid 139903660291968] AH00489: Apache/2.4.59 (Unix) configured -- resuming normal operations
# [Wed Jun 12 20:42:06.471651 2024] [core:notice] [pid 1:tid 139903660291968] AH00094: Command line: 'httpd -D FOREGROUND'
```
