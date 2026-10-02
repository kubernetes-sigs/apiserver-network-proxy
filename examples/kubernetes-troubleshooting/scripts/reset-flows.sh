#!/bin/bash
# Reset every agent<->server flow at once, the way a load balancer in front of the
# control plane does when it drops its connection table.
#
#   reset-flows.sh both     TCP RST reaches agents and servers (servers drop the backend immediately)
#   reset-flows.sh agents   only agents see the RST; servers keep half-open backends until they try to write
#   reset-flows.sh servers  only servers see the loss (sockets destroyed on the server nodes)
#
# Requires the lb-tools helper container (see README) for the "both" and "agents" modes.
set -eu
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"
MODE=${1:-both}
LB_IP=$(lb_ip)

case "$MODE" in
  both)
    echo "T0=$(date -u +%T.%N) mode=$MODE" | tee -a "$OUT_DIR/events.txt"
    docker exec "$LB_TOOLS" ss -K 'sport = :8132 or dport = :8091' | wc -l
    ;;
  agents)
    for n in $(control_planes); do
      docker exec "$n" iptables -I INPUT -s "$LB_IP" -p tcp --dport 8091 --tcp-flags RST RST -j DROP
    done
    echo "T0=$(date -u +%T.%N) mode=$MODE" | tee -a "$OUT_DIR/events.txt"
    docker exec "$LB_TOOLS" ss -K 'sport = :8132 or dport = :8091' | wc -l
    echo "RSTs toward the servers are being dropped; restore with: $0 restore"
    ;;
  servers)
    echo "T0=$(date -u +%T.%N) mode=$MODE" | tee -a "$OUT_DIR/events.txt"
    for n in $(control_planes); do
      docker exec "$n" ss -K 'sport = :8091' >/dev/null 2>&1 &
    done
    wait
    ;;
  restore)
    for n in $(control_planes); do
      docker exec "$n" iptables -D INPUT -s "$LB_IP" -p tcp --dport 8091 --tcp-flags RST RST -j DROP 2>/dev/null || true
    done
    echo "RST drop rules removed"
    ;;
  *)
    echo "usage: $0 {both|agents|servers|restore}" >&2
    exit 2
    ;;
esac
echo "done at $(date -u +%T.%N)"
