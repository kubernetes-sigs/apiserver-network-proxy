# Setting up apiserver-network-proxy in Kubernetes

This example will set up apiserver-network-proxy server as a Deployment in the master and apiserver-network-proxy agent as a DaemontSet on the worker nodes.

## Prerequisites

In order to set up the network-proxy, it's needed to generate 2 CA. One for the master communication, `konnectivity-server-ca.crt`
and one for the cluster communication, `konnectivity-cluster-ca.crt`.

Then, create:
- A client authentication certificate for the network-proxy agent, `konnectivity-agent-client.crt` and `konnectivity-agent-client.key`.
- A server authentication certificate for the cluster side of the network-proxy server, `konnectivity-cluster.crt` and `konnectivity-cluster.key` (one for each node or one per node).
- A client authentication certificate for the Kubernetes API server, `konnectivity-server-client.crt` and `konnectivity-server-client.key`.
- A server authentication certificate for the server side of the network-proxy server, `konnectivity-server.crt` and `konnectivity-server.key`.

## Master setup

### Kubernetes API server configuration

The Kubernetes API server needs to be started with the following flag: `--egress-selector-config-file=/path/to/your/egress-selector-configuration.yaml`.
The `egress-selector-configuration.yaml` file looks like:

```yaml
apiVersion: apiserver.k8s.io/v1alpha1
kind: EgressSelectorConfiguration
egressSelections:
- name: cluster
  connection:
    type: http-connect
    httpConnect:
      # Here we use 127.0.0.1 since we are binding the apiserver-network-proxy ports on the host
      # It's possible to use a service instead
      url: https://127.0.0.1:8131
      caBundle: /path/to/konnectivity-server-ca.crt
      clientKey: /path/to/konnectivity-server-client.key
      clientCert: /path/to/konnectivity-server-client.crt
- name: master
  connection:
    type: direct
- name: etcd
  connection:
    type: direct
```

### apiserver-network-proxy server

To run the `apiserver-network-proxy` server, use the following Deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    k8s-app: konnectivity-server
  name: konnectivity-server
spec:
  selector:
    matchLabels:
      k8s-app: konnectivity-server
  template:
    metadata:
      annotations:
        scheduler.alpha.kubernetes.io/critical-pod: ''
      labels:
        k8s-app: konnectivity-server
    spec:
      containers:
      - name: konnectivity-server
        image: gcr.io/google-containers/proxy-server:v0.0.3
        resources:
          requests:
            cpu: 40m
        command: 
        - /proxy-server
        args:
        - --cluster-ca-cert=/etc/kubernetes/pki/konnectivity-cluster-ca.crt
        - --cluster-cert=/etc/kubernetes/pki/konnectivity-cluster.crt
        - --cluster-key=/etc/kubernetes/pki/konnectivity-cluster.key
        - --server-ca-cert=/etc/kubernetes/pki/konnectivity-server-ca.crt
        - --server-cert=/etc/kubernetes/pki/konnectivity-server.crt
        - --server-key=/etc/kubernetes/pki/konnectivity-server.key
        - --mode=http-connect
        - --server-port=8131
        - --agent-port=8132
        - --admin-port=8133
        livenessProbe:
          httpGet:
            scheme: HTTP
            port: 8133
            path: /healthz
          initialDelaySeconds: 30
          timeoutSeconds: 60
        ports:
        - name: serverport
          containerPort: 8131
          hostPort: 8131
        - name: agentport
          containerPort: 8132
          hostPort: 8132
        - name: adminport
          containerPort: 8133
          hostPort: 8133
        volumeMounts:
        - name: konnectivity-server-pki
          mountPath: /etc/kubernetes/pki/
          readOnly: true
      volumes:
      - name: konnectivity-server-pki
        hostPath:
          # the path on your master where the certificate are located
          path: /var/lib/konnectivity-server/pki/
```

The `apiserver-network-proxy` server should now be running on the master.

## Nodes setup

We will use the following DaemonSet in order to deploy the `apiserver-network-proxy` agent on the worker nodes:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  labels:
    k8s-app: konnectivity-agent
  namespace: kube-system
  name: konnectivity-agent
spec:
  selector:
    matchLabels:
      k8s-app: konnectivity-agent
  template:
    metadata:
      labels:
        k8s-app: konnectivity-agent
      annotations:
        scheduler.alpha.kubernetes.io/critical-pod: ''
    spec:
      priorityClassName: system-cluster-critical
      tolerations:
        - key: "CriticalAddonsOnly"
          operator: "Exists"
      hostNetwork: true
      volumes:
        - name: pki
          hostPath:
            # the path of your certificates on the worker nodes
            path: /var/lib/konnectivity-agent/pki
      containers:
        - image: gcr.io/google-containers/proxy-agent:v0.0.3
          name: konnectivity-agent
          command: 
          - /proxy-agent
          args: 
          - --logtostderr=true
          - --ca-cert=/etc/srv/kubernetes/pki/konnectivity-agent/ca.crt
          - --agent-cert=/etc/srv/kubernetes/pki/konnectivity-agent/client.crt
          - --agent-key=/etc/srv/kubernetes/pki/konnectivity-agent/client.key
          - --proxy-server-host=<URL of the API server>
          - --proxy-server-port=8132
          env:
            - name: POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
          resources:
            limits:
              cpu: 50m
              memory: 30Mi
          livenessProbe:
            httpGet:
              host: 127.0.0.1
              port: 8093
              path: /healthz
            initialDelaySeconds: 15
            timeoutSeconds: 15
          volumeMounts:
            - name: pki
              mountPath: /etc/srv/kubernetes/pki/konnectivity-agent
```

## Verification

In order to verify that the setup is working, you can try to exec a `kubectl logs`, and check in the `apiserver-network-proxy` agent and server that packets are indeed being forwarded.
