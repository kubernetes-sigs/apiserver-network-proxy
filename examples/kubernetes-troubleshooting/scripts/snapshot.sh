#!/bin/bash
# One snapshot of the proxy's state as seen from agents, servers, apiservers and the kind load balancer.
set -u
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"

echo "##### $(date -u +%T)"
echo "== agents"
agent_metrics | awk '$2 ~ /^konnectivity_network_proxy_agent_(open_server_connections|known_server_count|server_connection_lost_total|server_connection_attempts_total|stream_errors_total|open_endpoint_connections|dial_duration_seconds_(sum|count))/ {
  sub(/^konnectivity_network_proxy_agent_/, "", $2); printf "  %-28s %s %s\n", $1, $2, $3 }'
echo "== servers"
for node in $(control_planes); do
  server_metrics "$node" | awk -v n="$node" '/^konnectivity_network_proxy_server_(ready_backends\{|grpc_connections|pending_backend_dials|established_connections |established_connections_closed_total|dial_failure_count|backend_connection_duration_seconds_(count|bucket\{le="(1|60|3600)"\})|dial_duration_seconds_(sum|count|bucket\{le="(0.025|0.1|0.5)"\}))/ {
    sub(/^konnectivity_network_proxy_server_/, ""); printf "  %-28s %s %s\n", n, $1, $2 }'
done
echo "== apiservers"
for node in $(control_planes); do
  apiserver_metrics "$node" | awk -v n="$node" '/^konnectivity_network_proxy_client_(client_connections|dial_failure_total)/ {
    sub(/^konnectivity_network_proxy_client_/, ""); printf "  %-28s %s %s\n", n, $1, $2 }'
done
echo "== load balancer"
docker exec "$(control_planes | head -1)" curl -s -m 3 "http://$(lb_ip):10000/stats" \
  | grep -E '^(cluster\.konnectivity_servers\.(upstream_cx_active|upstream_cx_destroy(_local|_remote)?|upstream_cx_total|health_check\.failure)|tcp\.konnectivity_tcp\.idle_timeout)' \
  | sed 's/^/  /'
