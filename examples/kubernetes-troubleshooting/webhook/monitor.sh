#!/bin/bash
# Every INTERVAL seconds, compare the apiserver's view of the webhook calls with the
# webhook's own view, from counter deltas so histogram bucket edges do not limit precision.
# Appends to $OUT_DIR/webhook-monitor.log.
set -u
# shellcheck source=../scripts/env.sh
. "$(dirname "$0")/../scripts/env.sh"
INTERVAL=${INTERVAL:-10}
OUT="$OUT_DIR/webhook-monitor.log"
CP=$(control_planes)

scrape() {
  # api_sum api_count api_fail_open api_over_100ms webhook_sum webhook_count webhook_conns_accepted webhook_conns_active
  local asum=0 acnt=0 afail=0 agt100=0 m
  for node in $CP; do
    m=$(apiserver_metrics "$node" | grep -E '^apiserver_admission_webhook_[a-z_]+\{[^}]*name="knp-webhook')
    asum=$(awk -v a="$asum" '/admission_duration_seconds_sum/ {a+=$2} END {print a}' <<<"$m")
    acnt=$(awk -v a="$acnt" '/admission_duration_seconds_count/ {a+=$2} END {print a}' <<<"$m")
    afail=$(awk -v a="$afail" '/fail_open_count/ {a+=$2} END {print a}' <<<"$m")
    agt100=$(awk -v a="$agt100" '/admission_duration_seconds_bucket\{[^}]*le="0.1"/ {le+=$2} /admission_duration_seconds_count/ {c+=$2} END {print a + c - le}' <<<"$m")
  done
  local w
  w=$(webhook_metrics)
  echo "$asum $acnt $afail $agt100 \
$(awk '/^knp_webhook_handler_duration_seconds_sum/ {print $2+0}' <<<"$w" | head -1) \
$(awk '/^knp_webhook_handler_duration_seconds_count/ {print $2+0}' <<<"$w" | head -1) \
$(awk '/^knp_webhook_connections_accepted_total/ {print $2+0}' <<<"$w" | head -1) \
$(awk '/^knp_webhook_connections_active/ {print $2+0}' <<<"$w" | head -1)"
}

webhook_metrics() {
  local node ip
  read -r node ip < <(kubectl -n webhook-load get pod -l app=knp-webhook \
    -o jsonpath='{.items[0].spec.nodeName} {.items[0].status.podIP}' 2>/dev/null)
  docker exec "$node" curl -s -m 3 "http://$ip:9090/metrics" 2>/dev/null
}

read -r pasum pacnt pafail pagt pwsum pwcnt pwconn _ < <(scrape)
while true; do
  sleep "$INTERVAL"
  read -r asum acnt afail agt wsum wcnt wconn wact < <(scrape)
  awk -v ts="$(date -u +%T)" -v asum="$asum" -v pasum="$pasum" -v acnt="$acnt" -v pacnt="$pacnt" \
      -v wsum="$wsum" -v pwsum="$pwsum" -v wcnt="$wcnt" -v pwcnt="$pwcnt" -v afail="$afail" -v pafail="$pafail" \
      -v wconn="$wconn" -v pwconn="$pwconn" -v wact="$wact" -v agt="$agt" -v pagt="$pagt" 'BEGIN {
        dac = acnt - pacnt; dwc = wcnt - pwcnt
        am = dac > 0 ? (asum - pasum) / dac * 1000 : 0
        wm = dwc > 0 ? (wsum - pwsum) / dwc * 1000 : 0
        printf "%s api_calls=%d api_mean=%.2fms webhook_calls=%d webhook_mean=%.3fms gap=%.2fms api_over_100ms=%d fail_open=%d new_conns=%d active_conns=%d\n",
          ts, dac, am, dwc, wm, am - wm, agt - pagt, afail - pafail, wconn - pwconn, wact
      }' | tee -a "$OUT"
  pasum=$asum; pacnt=$acnt; pafail=$afail; pagt=$agt; pwsum=$wsum; pwcnt=$wcnt; pwconn=$wconn
done
