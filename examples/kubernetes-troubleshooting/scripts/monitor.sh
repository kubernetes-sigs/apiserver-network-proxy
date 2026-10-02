#!/bin/bash
# Compact time series of the proxy's state, one line every INTERVAL seconds, appended to $OUT_DIR/monitor.log.
set -u
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"
INTERVAL=${INTERVAL:-5}
OUT="$OUT_DIR/monitor.log"
CP=$(control_planes)

while true; do
  ts=$(date -u +%T)
  agents=$(agent_metrics | awk '$2 == "konnectivity_network_proxy_agent_open_server_connections" {printf "%s,", $3}')
  srv=""; est=0
  for node in $CP; do
    m=$(server_metrics "$node")
    srv+=$(awk '/^konnectivity_network_proxy_server_ready_backend_connections/ {printf "%s", $2}' <<<"$m"),
    est=$((est + $(awk '/^konnectivity_network_proxy_server_established_connections / {print $2+0}' <<<"$m" | head -1)))
  done
  dialing=0; ok=0; fail=0
  for node in $CP; do
    m=$(apiserver_metrics "$node" | grep '^konnectivity_network_proxy_client_')
    dialing=$((dialing + $(awk '/client_connections\{status="dialing"\}/ {print $2+0}' <<<"$m" | head -1)))
    ok=$((ok + $(awk '/client_connections\{status="ok"\}/ {print $2+0}' <<<"$m" | head -1)))
    fail=$((fail + $(awk '/dial_failure_total/ {s+=$2} END {print s+0}' <<<"$m")))
  done
  echo "$ts agent_open=[$agents] server_ready_backends=[$srv] server_established=$est apiserver_dialing=$dialing apiserver_ok=$ok apiserver_dial_failures=$fail" | tee -a "$OUT"
  sleep "$INTERVAL"
done
