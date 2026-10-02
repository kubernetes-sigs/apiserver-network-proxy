#!/bin/bash
# Shared settings for the troubleshooting scripts. Source this file.
CLUSTER=${CLUSTER:-knp-test-cluster}
LB="${CLUSTER}-external-load-balancer"
# shellcheck disable=SC2034  # used by the scripts that source this file
LB_TOOLS="${CLUSTER}-lb-tools"
OUT_DIR=${OUT_DIR:-/tmp/${CLUSTER}-troubleshooting}
mkdir -p "$OUT_DIR"

control_planes() { kind get nodes --name "$CLUSTER" | grep control-plane | sort; }
workers()        { kind get nodes --name "$CLUSTER" | grep worker | sort; }
lb_ip()          { docker inspect "$LB" --format '{{.NetworkSettings.Networks.kind.IPAddress}}'; }

# Metrics of one kube-apiserver, read on its own node so each replica is seen separately.
apiserver_metrics() { # node
  docker exec "$1" kubectl --kubeconfig /etc/kubernetes/admin.conf --server https://localhost:6443 \
    --request-timeout=5s get --raw /metrics 2>/dev/null
}
# Metrics of the konnectivity-server on a control-plane node (hostNetwork, admin port 8093).
server_metrics() { docker exec "$1" curl -s -m 3 http://localhost:8093/metrics; }
# Metrics of every konnectivity-agent, read from its node. Prints "<node> <metric line>".
agent_metrics() {
  kubectl -n kube-system get pods -l k8s-app=konnectivity-agent \
    -o jsonpath='{range .items[*]}{.spec.nodeName} {.status.podIP}{"\n"}{end}' 2>/dev/null \
  | while read -r node ip; do
      docker exec "$node" curl -s -m 3 "http://$ip:8093/metrics" | sed "s/^/$node /"
    done
}
