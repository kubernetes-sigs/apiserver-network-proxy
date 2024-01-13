# Use apiserver-network-proxy with KIND


Change to the `examples/kind` folder and create a `kind` cluster with the `kind.config` file

```sh
$ kind create cluster --config kind.config
Creating cluster "kind" ...
DEBUG: docker/images.go:58] Image: kindest/node:v1.27.3@sha256:3966ac761ae0136263ffdb6cfd4db23ef8a83cba8a463690e98317add2c9ba72 present locally
 ✓ Ensuring node image (kindest/node:v1.27.3) 🖼
⠎⠁ Preparing nodes 📦 📦 📦

This node has joined the cluster:
* Certificate signing request was sent to apiserver and a response was received.
* The Kubelet was informed of the new secure connection details.

Run 'kubectl get nodes' on the control-plane to see this node join the cluster.
 ✓ Joining worker nodes 🚜
Set kubectl context to "kind-kind"
You can now use your cluster with:

kubectl cluster-info --context kind-kind

Have a nice day! 👋
```

Once the cluster is ready install the `apiserver-network-proxy` components:

```sh
$ kubectl apply -f konnectivity-server.yaml
clusterrolebinding.rbac.authorization.k8s.io/system:konnectivity-server created
daemonset.apps/konnectivity-server created

$ kubectl apply -f konnectivity-agent-ds.yaml
serviceaccount/konnectivity-agent created
```

To validate that it works, run a custom image and try to exec into the pod (it goes through the konnectivity proxy):
```sh
$ kubectl run test --image httpd:2
pod/test created
$ kubectl get pods
NAME   READY   STATUS              RESTARTS   AGE
test   0/1     ContainerCreating   0          4s
$ kubectl get pods
NAME   READY   STATUS    RESTARTS   AGE
test   1/1     Running   0          6s
$ kubectl exec -it test bash
kubectl exec [POD] [COMMAND] is DEPRECATED and will be removed in a future version. Use kubectl exec [POD] -- [COMMAND] instead.
```

