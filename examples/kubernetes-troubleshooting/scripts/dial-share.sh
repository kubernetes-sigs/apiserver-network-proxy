#!/bin/bash
# Measure how new dials are shared among the agents over DURATION seconds.
#
# Every dial that reaches an agent is counted in that agent's dial_duration_seconds
# histogram, and every dial the servers complete in theirs. This script reports, for the
# interval:
#   - per agent: dials received, share of the total, mean dial time at the agent;
#   - servers: dials completed, mean dial time as the server sees it (apiserver request
#     to DIAL_RSP), and the fraction above 25 ms and 100 ms.
# Run it with scripts/dial-load.sh (or any dial source) active, and with one agent
# starved through scripts/starve-agent.sh to see whether the servers keep sending that
# agent its share.
#
#   dial-share.sh [duration_seconds]   default 60
set -u
export LC_ALL=C # sort and join must agree on the node-name order
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"
DURATION=${1:-60}

agent_dials() { # prints "<node> <count> <sum_seconds>"
  agent_metrics | awk '$2 ~ /^konnectivity_network_proxy_agent_dial_duration_seconds_(count|sum)$/ {
      if ($2 ~ /count$/) c[$1]=$3; else s[$1]=$3 }
    END { for (n in c) printf "%s %d %s\n", n, c[n], s[n]+0 }' | sort
}
server_dials() { # prints "count sum le25 le100" summed over the servers
  local node
  for node in $(control_planes); do server_metrics "$node"; done | awk '
    /^konnectivity_network_proxy_server_dial_duration_seconds_count/ {c+=$2}
    /^konnectivity_network_proxy_server_dial_duration_seconds_sum/ {s+=$2}
    /^konnectivity_network_proxy_server_dial_duration_seconds_bucket\{le="0.025"\}/ {b25+=$2}
    /^konnectivity_network_proxy_server_dial_duration_seconds_bucket\{le="0.1"\}/ {b100+=$2}
    END {printf "%d %s %d %d\n", c, s+0, b25, b100}'
}

A0=$(agent_dials); S0=$(server_dials)
sleep "$DURATION"
A1=$(agent_dials); S1=$(server_dials)

echo "$(date -u +%T) interval=${DURATION}s"
total=$(join <(echo "$A0") <(echo "$A1") | awk '{t+=$4-$2} END {print t+0}')
echo "  dials per agent:"
join <(echo "$A0") <(echo "$A1") | awk -v t="$total" '{
    n=$4-$2; s=$5-$3; share=(t>0)?100*n/t:0; mean=(n>0)?1000*s/n:0
    printf "    %-28s %6d  %5.1f%%  mean %.1f ms at the agent\n", $1, n, share, mean }'
echo "$S0 $S1" | awk '{
    n=$5-$1; s=$6-$2; over25=n-($7-$3); over100=n-($8-$4)
    printf "  dials completed by the servers: %d, mean %.1f ms", n, (n>0)?1000*s/n:0
    printf ", above 25 ms: %d (%.1f%%), above 100 ms: %d (%.1f%%)\n", over25, (n>0)?100*over25/n:0, over100, (n>0)?100*over100/n:0 }'
