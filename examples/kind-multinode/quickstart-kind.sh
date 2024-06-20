#!/bin/bash

set -e

# DEFAULT ARGS
CLUSTER_NAME="knp-test-cluster"
AGENT_IMAGE="gcr.io/k8s-staging-kas-network-proxy/proxy-agent:master"
SERVER_IMAGE="gcr.io/k8s-staging-kas-network-proxy/proxy-server:master"
NUM_WORKER_NODES=1
NUM_KCP_NODES=2
OVERWRITE_CLUSTER=false
SIDELOAD_IMAGES=false
SERVER_COUNT_OVERRIDE=-1

# Provide usage info
usage() {
  printf "USAGE:\n./quickstart-kind.sh\n\t[--cluster-name <NAME>]\n\t[--server-image <IMAGE_NAME>[:<IMAGE_TAG>]]\n\t[--agent-image <IMAGE_NAME>[:<IMAGE_TAG>]]\n\t[--num-worker-nodes <NUM>]\n\t[--num-kcp-nodes <NUM>]\n\t[--overwrite-cluster]\n"
}

# ARG PARSING
VALID_ARGS=$(getopt --options "h" --longoptions "sideload-images,cluster-name:,agent-image:,server-image:,num-worker-nodes:,num-kcp-nodes:,help,overwrite-cluster,server-count-override:" --name "$0" -- "$@") || exit 2

eval set -- "$VALID_ARGS"
while true; do
  case "$1" in
    --cluster-name)
      CLUSTER_NAME=$2
      shift 2
      ;;
    --agent-image)
      AGENT_IMAGE=$2
      shift 2
      ;;
    --server-image)
      SERVER_IMAGE=$2
      shift 2
      ;;
    --num-worker-nodes)
      NUM_WORKER_NODES=$2
      shift 2
      ;;
    --num-kcp-nodes)
      NUM_KCP_NODES=$2
      shift 2
      ;;
    --overwrite-cluster)
      OVERWRITE_CLUSTER=true
      shift 1
      ;;
    --sideload-images)
      SIDELOAD_IMAGES=true
      shift 1
      ;;
    --server-count-override)
      SERVER_COUNT_OVERRIDE=$2
      shift 2
      ;;
    --)
      shift
      break
      ;;
    *|-h|--help)
      usage
      exit
      ;;
  esac
done

# RENDER CONFIG TEMPLATES
echo "Rendering config templates..."
if [ ! -d rendered ]; then
  echo "Creating ./rendered"
  mkdir rendered
fi
echo "Adding $NUM_KCP_NODES control plane nodes and $NUM_WORKER_NODES worker nodes to kind.config..."
cp templates/kind/kind.config rendered/kind.config
for i in $(seq 1 "$NUM_KCP_NODES")
do
 cat templates/kind/control-plane.config >> rendered/kind.config
done
for i in $(seq 1 "$NUM_WORKER_NODES")
do
 cat templates/kind/worker.config >> rendered/kind.config
done

SERVER_COUNT=$NUM_KCP_NODES
if [ "$SERVER_COUNT_OVERRIDE" -ge 0 ]; then
  echo "Overriding default server count from $NUM_KCP_NODES to $SERVER_COUNT_OVERRIDE"
  SERVER_COUNT=$SERVER_COUNT_OVERRIDE
fi

echo "Setting server image to $SERVER_IMAGE, agent image to $AGENT_IMAGE, and --server-count flag on server to $SERVER_COUNT"
sed -e "s|image: .*|image: $AGENT_IMAGE|" <templates/k8s/konnectivity-agent-ds.yaml >rendered/konnectivity-agent-ds.yaml
sed -e "s|image: .*|image: $SERVER_IMAGE|" -e "s/--server-count=[0-9]\+/--server-count=$SERVER_COUNT/" <templates/k8s/konnectivity-server.yaml >rendered/konnectivity-server.yaml



# CLUSTER CREATION
if [ $OVERWRITE_CLUSTER = true ] && kind get clusters | grep -q "$CLUSTER_NAME"; then
  echo "Deleting old cluster $CLUSTER_NAME..."
  kind delete clusters "$CLUSTER_NAME"
fi

echo "Creating cluster $CLUSTER_NAME..."
kind create cluster --config rendered/kind.config --name $CLUSTER_NAME

echo "Successfully created cluster. Switching kubectl context to kind-$CLUSTER_NAME"
kubectl cluster-info --context kind-$CLUSTER_NAME

# SIDELOAD IMAGES IF REQUESTED
if [ $SIDELOAD_IMAGES = true ]; then
  echo "Sideloading images into the kind cluster..."
  kind --name "$CLUSTER_NAME" load docker-image "$SERVER_IMAGE"
  kind --name "$CLUSTER_NAME" load docker-image "$AGENT_IMAGE"
fi

# DEPLOY KONNECTIVITY
echo "Requesting creation of konnectivity proxy servers on cluster $CLUSTER_NAME..."
kubectl apply -f rendered/konnectivity-server.yaml
echo "Requesting creation of konnectivity proxy agents on cluster $CLUSTER_NAME..."
kubectl apply -f rendered/konnectivity-agent-ds.yaml
