# 00. Baseline

What the cluster looks like with no fault injected. Every other scenario is compared
against these numbers.

## Preconditions

- Cluster from the [README](../README.md#cluster): 5 control-plane nodes, 3 workers,
  agents dialing the servers through the kind load balancer.
- `export CLUSTER=<name>`; `scripts/monitor.sh` and `scripts/tail-logs.sh` running.

## Steps

```sh
webhook/deploy.sh
CREDS=/tmp/$CLUSTER-troubleshooting/kubeconfig-creds
go run -mod=vendor ./examples/kubernetes-troubleshooting/webhook-load \
  -server "$(cat $CREDS/server)" -ca $CREDS/ca.crt -cert $CREDS/client.crt -key $CREDS/client.key \
  -workers 4 -rate 25 &            # 100 requests/s through the kind load balancer
webhook/monitor.sh &               # apiserver-measured vs webhook-measured, every 10 s
scripts/dial-load.sh &             # one new tunnel dial every ~0.7 s (kubectl exec)
sleep 300
scripts/snapshot.sh > baseline.txt
```

The load generator creates ConfigMaps with `dryRun=All`, so the webhook runs and
nothing is stored. The webhook has `failurePolicy: Ignore` and `timeoutSeconds: 10`, so
in the fault scenarios a tunnel failure shows up as a fail-open and a latency spike
instead of a rejected request.

## What to observe

- `webhook/monitor.sh`: `api_mean` is the apiserver's admission duration for the
  webhook, `webhook_mean` is the webhook's own handler time, `gap` is the path between
  them (dial if any, tunnel, TLS, HTTP). `new_conns` shows how often the apiservers open
  a new connection to the webhook.
- Server `dial_duration_seconds` and `dial-load.log`: cost of a new tunnel dial.
- Agent `server_connection_attempts_total{result}` and server
  `stream_errors_total{code="Canceled",segment="from_agent"}`: the sync loop at rest.
- Balancer access log and `tcp.konnectivity_tcp.idle_timeout`.

## Results (2026-10-02)

Webhook path, 100 requests/s, four keep-alive connections from the apiservers to the
webhook:

| Measure | Value |
|---|---|
| webhook handler time (body read included) | 0.05 ms |
| apiserver-measured admission duration, mean from `_sum/_count` | 1.3 to 1.5 ms |
| difference, which is the tunnel, TLS and HTTP cost per call | about 1.4 ms |
| client-observed end to end, through the kind balancer | 4.4 to 4.9 ms mean, p99 6 to 7 ms |
| new TCP connections at the webhook | 1 per 10 s (apiserver idle-connection recycling), 4 active |

Dial path (`kubectl exec` every 0.7 s):

| Measure | Value |
|---|---|
| server `dial_duration_seconds` (DIAL_REQ sent to DIAL_RSP received) | 0.9 ms mean, none above 25 ms |
| agent `dial_duration_seconds` (agent to endpoint) | 0.24 ms |
| `kubectl exec ... true` round trip | p50 187 ms, p99 218 ms |

Sync loop at rest (`--sync-interval=5s`, `--sync-forever`, 3 agents, 5 servers):

| Measure | Value |
|---|---|
| new agent connections through the balancer | one per 5 s per agent; 186 in 5 min |
| `server_connection_attempts_total{result="duplicate"}` | about 60 per agent per 5 min |
| server `stream_errors_total{code="Canceled",segment="from_agent"}` | about 1,800 per server after a day |
| server log | one `Connect request from agent` / `Stream read from agent cancelled` pair per attempt, 56 per server per 5 min |

Balancer: envoy's `tcp_proxy` default `idle_timeout` is 1 h, the same as the proxy's
`--keepalive-time`. The access log shows the agent streams being recreated every hour
and `tcp.konnectivity_tcp.idle_timeout` counting them.

## Interpretation

- The proxy adds about 1.4 ms to each webhook call on one host. Fleet measurements
  that show a larger gap between apiserver-measured and webhook-measured latency are
  dominated by network distance between the control plane and the agents.
- With `--sync-forever` every agent opens and closes one connection per interval
  forever, even when it is connected to every server. That churn is the dominant
  content of the server's `Connect`/cancel log lines and of
  `stream_errors_total{Canceled}`, which hides a real loss of agent streams in both.
- Any middlebox with an idle timeout at or below the gRPC keepalive interval prunes
  idle agent streams silently; `--keepalive-time` has to be below the middlebox timeout.

## Changes and their effect

None yet.
