#!/bin/bash
# Generate a steady stream of new tunnel dials: each `kubectl exec` opens a new connection from
# the apiserver to the kubelet through a randomly chosen agent. Logs one line per call to
# $OUT_DIR/dial-load.log: "<time> <ms> <ok|fail>".
set -u
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"
POD=${POD:-test}
INTERVAL=${INTERVAL:-0.5}
OUT="$OUT_DIR/dial-load.log"
while true; do
  start=$(date +%s%N)
  if timeout 60 kubectl exec "$POD" -- true >/dev/null 2>&1; then r=ok; else r=fail; fi
  echo "$(date -u +%T) $(( ($(date +%s%N) - start) / 1000000 )) $r" >> "$OUT"
  sleep "$INTERVAL"
done
