#!/bin/bash
# Limit the CPU of the konnectivity-agent container on one node through its cgroup, leaving
# kubelet and the other pods on the node alone. Simulates an agent scheduled on a node whose
# CPU is saturated by other workloads.
#
#   starve-agent.sh <node> [quota_us]   default quota 5000 us per 100000 us period (5% of one CPU)
#   starve-agent.sh <node> restore
set -eu
NODE=${1:?node name}
QUOTA=${2:-5000}

CG=$(docker exec "$NODE" sh -c 'id=$(crictl ps --name konnectivity-agent -q | head -1); pid=$(crictl inspect -o go-template --template "{{.info.pid}}" "$id"); cut -d: -f3 /proc/$pid/cgroup')
if [ "$QUOTA" = restore ]; then
  docker exec "$NODE" sh -c "echo 'max 100000' > /sys/fs/cgroup$CG/cpu.max"
else
  docker exec "$NODE" sh -c "echo '$QUOTA 100000' > /sys/fs/cgroup$CG/cpu.max"
fi
echo "$(date -u +%T) $NODE agent cpu.max: $(docker exec "$NODE" cat "/sys/fs/cgroup$CG/cpu.max")"
docker exec "$NODE" grep -E 'nr_throttled|throttled_usec' "/sys/fs/cgroup$CG/cpu.stat" | tr '\n' ' '; echo
