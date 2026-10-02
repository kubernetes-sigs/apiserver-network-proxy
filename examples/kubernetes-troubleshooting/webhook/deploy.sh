#!/bin/bash
# Build and deploy the measuring webhook: image, certificates, Deployment, Service and
# a ValidatingWebhookConfiguration for ConfigMap creates in namespace webhook-load.
set -eu
# shellcheck source=../scripts/env.sh
. "$(dirname "$0")/../scripts/env.sh"
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
IMAGE=${IMAGE:-local/knp-webhook:dev}
WEBHOOK_NODE=${WEBHOOK_NODE:-$(workers | tail -1)}
CERTS="$OUT_DIR/webhook-certs"

(cd "$REPO" && CGO_ENABLED=0 go build -mod=vendor -o "$HERE/bin/webhook" ./examples/kubernetes-troubleshooting/webhook)
docker build -q -t "$IMAGE" -f "$HERE/Dockerfile" "$HERE" >/dev/null
kind --name "$CLUSTER" load docker-image "$IMAGE" >/dev/null 2>&1

mkdir -p "$CERTS"
if [ ! -f "$CERTS/tls.crt" ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=knp-webhook-ca" \
    -keyout "$CERTS/ca.key" -out "$CERTS/ca.crt" >/dev/null 2>&1
  openssl req -newkey rsa:2048 -nodes -subj "/CN=knp-webhook.webhook-load.svc" \
    -keyout "$CERTS/tls.key" -out "$CERTS/tls.csr" >/dev/null 2>&1
  printf 'subjectAltName=DNS:knp-webhook.webhook-load.svc,DNS:knp-webhook.webhook-load.svc.cluster.local\n' > "$CERTS/san.cnf"
  openssl x509 -req -in "$CERTS/tls.csr" -CA "$CERTS/ca.crt" -CAkey "$CERTS/ca.key" -CAcreateserial \
    -days 30 -extfile "$CERTS/san.cnf" -out "$CERTS/tls.crt" >/dev/null 2>&1
fi
CA_BUNDLE=$(base64 -w0 < "$CERTS/ca.crt")

kubectl create namespace webhook-load --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n webhook-load create secret tls knp-webhook-certs --cert="$CERTS/tls.crt" --key="$CERTS/tls.key" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

sed -e "s|__IMAGE__|$IMAGE|" -e "s|__NODE__|$WEBHOOK_NODE|" -e "s|__CA_BUNDLE__|$CA_BUNDLE|" \
  "$HERE/manifests.yaml" | kubectl apply -f -
kubectl -n webhook-load rollout status deploy/knp-webhook --timeout=120s

# The kubeconfig credentials, extracted for webhook-load.
KC="$OUT_DIR/kubeconfig-creds"
mkdir -p "$KC"
kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > "$KC/ca.crt"
kubectl config view --raw --minify -o jsonpath='{.users[0].user.client-certificate-data}' | base64 -d > "$KC/client.crt"
kubectl config view --raw --minify -o jsonpath='{.users[0].user.client-key-data}' | base64 -d > "$KC/client.key"
chmod 600 "$KC/client.key"
kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' > "$KC/server"
echo "webhook deployed on $WEBHOOK_NODE; load generator credentials in $KC"
