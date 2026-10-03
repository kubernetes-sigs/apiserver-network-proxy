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
- `scripts/vip-churn.sh`: new connections through the balancer per interval, how many
  of them produced a new server connection, how many were closed as duplicates, and the
  sockets the balancer holds. Server `backend_connection_duration_seconds_bucket{le="1"}`
  counts the same duplicates from the server side.
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

Sync loop at rest (`--sync-interval=5s`, `--sync-forever`, `--count-server-leases`,
3 agents connected to all 5 servers), `scripts/vip-churn.sh 60`:

```
new connections through the VIP:        36  (0.60/s)
of which new server connections: 0, failed: 0, closed as duplicates: 36
server side, connections that lived <1s: 36
balancer sockets toward the servers:    established=15 time_wait=191
of which envoy TCP health checks:       155 in the interval
```

Every one of the 36 flows is an agent that dialed the VIP, completed TCP, TLS, the gRPC
`Connect` and the token authentication on the server, learned from the response header
that it was already connected to that server, and closed the connection. Over a day that
is about 17,000 connections per agent and ~1,800
`stream_errors_total{code="Canceled",segment="from_agent"}` per server, plus one
`Connect request from agent` / `Stream read from agent cancelled` log pair per attempt.

Balancer: envoy's `tcp_proxy` default `idle_timeout` is 1 h, the same as the proxy's
`--keepalive-time`. The access log shows the agent streams being recreated every hour
and `tcp.konnectivity_tcp.idle_timeout` counting them.

## Interpretation

- The proxy adds about 1.4 ms to each webhook call on one host. Fleet measurements
  that show a larger gap between apiserver-measured and webhook-measured latency are
  dominated by network distance between the control plane and the agents.
- Any middlebox with an idle timeout at or below the gRPC keepalive interval prunes
  idle agent streams silently; `--keepalive-time` has to be below the middlebox timeout.

### Connection churn and port exhaustion at the VIP

With `--sync-forever` every agent opens and closes one connection per interval forever,
even when it is connected to every server. A TCP connection is the most expensive
operation the agent performs against the control plane, and on the VIP each one is a
new flow:

- A VIP that translates addresses (a NAT, a private service endpoint, a proxy balancer)
  allocates a source port for every flow from a finite pool, and keeps it allocated for
  the life of the flow plus a hold time after it closes (TCP `TIME_WAIT` is 60 s; NAT
  mapping timeouts are often 120 s or more). The pool is per NAT address and is shared
  with every other flow through the VIP, so a steady stream of short connections is a
  permanent reservation against it. When the pool is exhausted, new connections through
  the VIP fail, for every client of that VIP.
- The flow rate is `agents / sync-interval`, independent of how many servers there are.
  Three idle agents produce 0.6 flows per second (measured above). One hundred agents at
  a 5 s interval produce 20 flows per second: about 1,200 ports reserved at any moment
  with a 60 s hold and 2,400 with a 120 s hold, on top of the 2,000 long-lived
  agent-to-server connections (100 × 20), all for connections that carry no traffic.
- A reset ([01](01-reset-seen-by-both-sides.md)) adds about N·ln(N) attempts per agent
  on top: 7,200 flows for 100 agents and 20 servers. At the current one attempt per
  interval they are spread over about six minutes at the same 20 flows per second, so
  they add nothing to the resting reservation. Any change that makes the agent dial
  faster while under-connected would spend those 7,200 flows in seconds and reserve
  7,200 ports at once. The availability gain would be small: what the apiservers need
  is the first agent back on each server, which takes about `N × interval / agents` (1 s
  at 100 agents and 20 servers; 1.4 to 12 s measured with 3 agents and 5 servers in
  [03](03-reset-seen-by-servers-only.md)). The six minutes are the time until every
  agent holds every server, which only affects how evenly load spreads afterwards.
  The re-mesh rate should stay bounded; the lever for faster recovery is on the server
  side ([02](02-reset-seen-by-agents-only.md)).
- The churn also hides real events: the server log and `stream_errors_total{Canceled}`
  are dominated by it, so a loss of every agent stream does not stand out in either.

## Changes and their effect

### agent: with lease counting, stop dialing once connected to every server

When the server count comes from leases, a new server appears in the count without the
agent dialing, so the `--sync-forever` dial is skipped while the agent is connected to
every counted server. The agent still dials every interval while under-connected.
Measured on 2026-10-03 with `scripts/vip-churn.sh 60`, same cluster, agents at rest:

| | before | after |
|---|---|---|
| new connections through the VIP per minute | 36 (0.60/s) | **0** |
| of which closed as duplicates | 36 | 0 |
| server side, connections that lived under 1 s | 36 | 0 |
| balancer `TIME_WAIT` toward the servers | 191 (155 of them envoy health checks) | 150 (all envoy health checks) |
| scaled to 100 agents at a 5 s interval | 20 flows/s, 1,200 to 2,400 ports held | 0 |

Recovery is unchanged. After `reset-flows.sh both` the agents dialed again every
interval until each held all five servers (36 s, 71 s and 141 s, within the spread of
[01](01-reset-seen-by-both-sides.md)) and then stopped; the following minute showed 0
new connections through the VIP. Without `--count-server-leases` the agent behaves as
before, since dialing is then the only way to learn the server count.
