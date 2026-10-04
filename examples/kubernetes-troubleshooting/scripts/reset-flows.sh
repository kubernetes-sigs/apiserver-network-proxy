#!/bin/bash
# Reset every agent<->server flow at once, the way a load balancer in front of the
# control plane does when it drops its connection table.
#
#   reset-flows.sh both         TCP RST reaches agents and servers (servers drop the backend immediately)
#   reset-flows.sh agents       only agents see the RST; servers keep half-open backends until they try to write
#   reset-flows.sh servers      only servers see the loss (sockets destroyed on the server nodes); through a
#                               proxying balancer such as envoy the agents still learn of it at once
#   reset-flows.sh servers-nat  servers see the RST, agents do not: the balancer drops both legs and the
#                               resets toward the agents are dropped on the worker nodes, as behind a
#                               NAT-style VIP; agents keep half-open streams until they try to write
#
# Requires the lb-tools helper container (see README) for the "both", "agents" and "servers-nat" modes.
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
  servers-nat)
    # Agent pods are routed through the worker's FORWARD chain.
    for n in $(workers); do
      docker exec "$n" iptables -I FORWARD -s "$LB_IP" -p tcp --sport 8132 --tcp-flags RST RST -j DROP
    done
    echo "T0=$(date -u +%T.%N) mode=$MODE" | tee -a "$OUT_DIR/events.txt"
    docker exec "$LB_TOOLS" ss -K 'sport = :8132 or dport = :8091' | wc -l
    echo "RSTs toward the agents are being dropped; restore with: $0 restore"
    ;;
  restore)
    for n in $(control_planes); do
      docker exec "$n" iptables -D INPUT -s "$LB_IP" -p tcp --dport 8091 --tcp-flags RST RST -j DROP 2>/dev/null || true
    done
    for n in $(workers); do
      docker exec "$n" iptables -D FORWARD -s "$LB_IP" -p tcp --sport 8132 --tcp-flags RST RST -j DROP 2>/dev/null || true
    done
    echo "RST drop rules removed"
    ;;
  *)
    echo "usage: $0 {both|agents|servers|servers-nat|restore}" >&2
    exit 2
    ;;
esac
echo "done at $(date -u +%T.%N)"
