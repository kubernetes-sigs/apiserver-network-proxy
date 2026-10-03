#!/bin/bash
# Measure new-connection churn at the load balancer (the VIP) over DURATION seconds.
#
# Every TCP connection an agent opens to the VIP is a new flow for the balancer and, on a
# NAT-style VIP, a port taken from a finite pool for the life of the flow plus the hold
# time after it closes. This script reports, for the interval:
#   - new upstream connections the balancer opened to the servers (envoy upstream_cx_total),
#     which in this setup are all agent connection attempts;
#   - agent connection attempts that produced a new server connection or failed; the rest
#     reached a server the agent already knew and were closed at once (duplicates);
#   - server-side: agent connections that lived under one second
#     (backend_connection_duration_seconds_bucket{le="1"}), the same duplicates seen from the servers;
#   - TIME_WAIT sockets the balancer holds toward the servers at the end, and how many of
#     those are envoy's own TCP health checks (a kind artefact, absent on a real VIP).
#
#   vip-churn.sh [duration_seconds]   default 60
set -u
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"
DURATION=${1:-60}
CP1=$(control_planes | head -1)
LB_IP=$(lb_ip)

lb_stat() { docker exec "$CP1" curl -s -m 3 "http://$LB_IP:10000/stats" | awk -v k="$1:" '$1 == k {print $2}'; }
attempts() { # result
  agent_metrics | awk -v r="result=\"$1\"" '$2 ~ /^konnectivity_network_proxy_agent_server_connection_attempts_total/ && index($2, r) {s+=$3} END {print s+0}'
}
agents() { kubectl -n kube-system get pods -l k8s-app=konnectivity-agent --no-headers 2>/dev/null | wc -l; }
short_lived() { # agent connections the servers saw end within 1 s
  local s=0 node
  for node in $(control_planes); do
    s=$((s + $(server_metrics "$node" | awk '/^konnectivity_network_proxy_server_backend_connection_duration_seconds_bucket\{le="1"\}/ {print $2+0}' | head -1)))
  done
  echo "$s"
}

cx0=$(lb_stat cluster.konnectivity_servers.upstream_cx_total)
hc0=$(lb_stat cluster.konnectivity_servers.health_check.attempt)
c0=$(attempts connected); e0=$(attempts error); sl0=$(short_lived)
sleep "$DURATION"
cx1=$(lb_stat cluster.konnectivity_servers.upstream_cx_total)
hc1=$(lb_stat cluster.konnectivity_servers.health_check.attempt)
c1=$(attempts connected); e1=$(attempts error); sl1=$(short_lived)
tw=$(docker exec "$LB_TOOLS" ss -Htan state time-wait '( dport = :8091 )' 2>/dev/null | wc -l)
est=$(docker exec "$LB_TOOLS" ss -Htan state established '( dport = :8091 )' 2>/dev/null | wc -l)
range=$(docker exec "$LB_TOOLS" sysctl -n net.ipv4.ip_local_port_range 2>/dev/null | tr '\t' '-')

n=$(agents)
new=$((cx1 - cx0)); conn=$((c1 - c0)); err=$((e1 - e0))
echo "$(date -u +%T) interval=${DURATION}s agents=$n"
echo "  new connections through the VIP:        $new  ($(awk -v a="$new" -v d="$DURATION" 'BEGIN {printf "%.2f", a/d}')/s)"
echo "  of which new server connections: $conn, failed: $err, closed as duplicates: $((new - conn - err))"
echo "  server side, connections that lived <1s: $((sl1 - sl0))"
echo "  balancer sockets toward the servers:    established=$est time_wait=$tw"
echo "  of which envoy TCP health checks:       $((hc1 - hc0)) in the interval (about $((hc1 - hc0)) of the time_wait at a 60 s hold)"
echo "  balancer ephemeral port range:          $range"
