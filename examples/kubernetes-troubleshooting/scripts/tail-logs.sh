#!/bin/bash
# Tail konnectivity-server, konnectivity-agent and load-balancer logs into $OUT_DIR/logs/,
# reading them on the nodes with crictl so they do not depend on the tunnel under test.
set -u
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"
LOGS="$OUT_DIR/logs"
mkdir -p "$LOGS"
: > "$LOGS/pids"

for node in $(control_planes); do
  # shellcheck disable=SC2016  # the $(...) must run on the node
  nohup docker exec "$node" sh -c 'crictl logs -f --since 1s $(crictl ps --name konnectivity-server -q | head -1)' \
    > "$LOGS/konnectivity-server-$node.log" 2>&1 &
  echo $! >> "$LOGS/pids"
done
for node in $(workers); do
  # shellcheck disable=SC2016
  nohup docker exec "$node" sh -c 'crictl logs -f --since 1s $(crictl ps --name konnectivity-agent -q | head -1)' \
    > "$LOGS/konnectivity-agent-$node.log" 2>&1 &
  echo $! >> "$LOGS/pids"
done
nohup docker logs -f --since 1s "$LB" > "$LOGS/load-balancer.log" 2>&1 &
echo $! >> "$LOGS/pids"
echo "tailing $(wc -l < "$LOGS/pids") logs into $LOGS (stop with: xargs kill < $LOGS/pids)"
