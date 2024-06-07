#!/bin/bash

set -e

CLUSTER_NAME=$1

echo "Creating cluster $CLUSTER_NAME..."
kind create cluster --config kind.config --name $CLUSTER_NAME

echo "Successfully created cluster. Switching kubectl context to kind-$CLUSTER_NAME"
kubectl cluster-info --context kind-$CLUSTER_NAME

echo "Requesting creation of konnectivity proxy servers on cluster $CLUSTER_NAME..."
kubectl apply -f konnectivity-server.yaml
echo "Requesting creation of konnectivity proxy agents on cluster $CLUSTER_NAME..."
kubectl apply -f konnectivity-agent-ds.yaml
