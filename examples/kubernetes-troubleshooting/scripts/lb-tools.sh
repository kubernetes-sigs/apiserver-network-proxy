#!/bin/bash
# Start or stop a privileged helper container that shares the kind load balancer's network
# namespace, so you can run ss, iptables and tcpdump against the balancer's sockets.
set -eu
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"
case "${1:-start}" in
  start)
    docker run -d --rm --name "$LB_TOOLS" --net=container:"$LB" --privileged nicolaka/netshoot:latest sleep infinity >/dev/null
    echo "$LB_TOOLS attached to $LB ($(lb_ip)); example: docker exec $LB_TOOLS ss -tn 'sport = :8132 or dport = :8091'"
    ;;
  stop)
    docker rm -f "$LB_TOOLS" >/dev/null
    ;;
  *) echo "usage: $0 {start|stop}" >&2; exit 2 ;;
esac
