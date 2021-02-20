#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

# shellcheck source=util.sh
source "${BASH_SOURCE%/*}/util.sh"

check-command-installed kubectl
check-command-installed helm
check-command-installed kind

# Use KIND_LOAD_IMAGE=y ./scripts/deploy-kubefed.sh <image> to load
# the built docker image into kind before deploying.
if [[ "${KIND_LOAD_IMAGE:-}" == "y" ]]; then
    kind load docker-image "${SERVER_FULL_IMAGE}-${ARCH}:${TAG}" --name="${KIND_CLUSTER_NAME:-kind}"
    kind load docker-image "${AGENT_FULL_IMAGE}-${ARCH}:${TAG}" --name="${KIND_CLUSTER_NAME:-kind}"
    kind load docker-image "${TEST_CLIENT_FULL_IMAGE}-${ARCH}:${TAG}" --name="${KIND_CLUSTER_NAME:-kind}"
    kind load docker-image "${TEST_SERVER_FULL_IMAGE}-${ARCH}:${TAG}" --name="${KIND_CLUSTER_NAME:-kind}"
fi

if [[ "${FORCE_DEPLOY:-}" == "y" ]]; then
  ${BASH_SOURCE%/*}/delete-asnp.sh
fi

kubectl create secret tls konnectivity-proxyca --cert=certs/proxyca.crt --key=certs/proxyca.key
kubectl create secret tls konnectivity-proxyserver --cert=certs/proxyserver.crt --key=certs/proxyserver.key
kubectl create secret tls konnectivity-proxyclient --cert=certs/proxyclient.crt --key=certs/proxyclient.key
kubectl create secret tls konnectivity-agentca --cert=certs/agentca.crt --key=certs/agentca.key
kubectl create secret tls konnectivity-agentserver --cert=certs/agentserver.crt --key=certs/agentserver.key
kubectl create secret tls konnectivity-agentclient --cert=certs/agentclient.crt --key=certs/agentclient.key

helm install test-asnp ./examples/konnectivity \
  --set agent.image.repository=${AGENT_FULL_IMAGE}-${ARCH} \
  --set agent.image.tag=${TAG} \
  --set server.image.repository=${SERVER_FULL_IMAGE}-${ARCH} \
  --set server.image.tag=${TAG} \
  --set testclient.image.repository=${TEST_CLIENT_FULL_IMAGE}-${ARCH} \
  --set testclient.image.tag=${TAG} \
  --set testserver.image.repository=${TEST_SERVER_FULL_IMAGE}-${ARCH} \
  --set testserver.image.tag=${TAG} \
