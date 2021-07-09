#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

helm uninstall test-asnp  || true
kubectl delete secret konnectivity-agentca || true
kubectl delete secret konnectivity-agentclient || true
kubectl delete secret konnectivity-agentserver || true
kubectl delete secret konnectivity-proxyca || true
kubectl delete secret konnectivity-proxyserver || true
kubectl delete secret konnectivity-proxyclient || true
