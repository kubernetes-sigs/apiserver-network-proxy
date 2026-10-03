#!/bin/bash
# Measure new-connection churn at the load balancer (the VIP) over DURATION seconds.
#
# Every TCP connection an agent opens to the VIP is a new flow for the balancer and, on a
# NAT-style VIP, a port taken from a finite pool for the life of the flow plus the hold
# time after it closes. This script reports, for the interval:
#   - new upstream connections the balancer opened to the servers (envoy upstream_cx_total),
#     which in this setup are all agent connection attempts;
#   - agent connection attempts by result (connected / duplicate / error);
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

cx0=$(lb_stat cluster.konnectivity_servers.upstream_cx_total)
hc0=$(lb_stat cluster.konnectivity_servers.health_check.attempt)
c0=$(attempts connected); d0=$(attempts duplicate); e0=$(attempts error)
sleep "$DURATION"
cx1=$(lb_stat cluster.konnectivity_servers.upstream_cx_total)
hc1=$(lb_stat cluster.konnectivity_servers.health_check.attempt)
c1=$(attempts connected); d1=$(attempts duplicate); e1=$(attempts error)
tw=$(docker exec "$LB_TOOLS" ss -Htan state time-wait '( dport = :8091 )' 2>/dev/null | wc -l)
est=$(docker exec "$LB_TOOLS" ss -Htan state established '( dport = :8091 )' 2>/dev/null | wc -l)
range=$(docker exec "$LB_TOOLS" sysctl -n net.ipv4.ip_local_port_range 2>/dev/null | tr '\t' '-')

n=$(agents)
echo "$(date -u +%T) interval=${DURATION}s agents=$n"
echo "  new connections through the VIP:        $((cx1 - cx0))  ($(awk -v a=$((cx1 - cx0)) -v d="$DURATION" 'BEGIN {printf "%.2f", a/d}')/s)"
echo "  agent attempts: connected=$((c1 - c0)) duplicate=$((d1 - d0)) error=$((e1 - e0))"
echo "  balancer sockets toward the servers:    established=$est time_wait=$tw"
echo "  of which envoy TCP health checks:       $((hc1 - hc0)) in the interval (about $((hc1 - hc0)) of the time_wait at a 60 s hold)"
echo "  balancer ephemeral port range:          $range"
