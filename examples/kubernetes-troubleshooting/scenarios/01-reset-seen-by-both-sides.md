# 01. All agent connections reset, seen by both sides

A load balancer in front of the control plane drops its connection table and sends a
TCP reset on every flow it carried. In this variant both ends of every agent-to-server
connection receive the reset. It is the clean version of the event and the reference
for the two variants that follow.

Production symptom this maps to: every agent logs `could not read stream ... read:
connection reset by peer` on all of its streams within the same minute, with no agent
restart and no server restart.

## Preconditions

- [00-baseline](00-baseline.md) running: webhook load, `webhook/monitor.sh`,
  `scripts/monitor.sh`, `scripts/tail-logs.sh`. A `kubectl logs -f` loop against a pod
  on a worker also works as tunnel traffic.
- `scripts/lb-tools.sh start`.

## Steps

```sh
scripts/snapshot.sh > before.txt
scripts/reset-flows.sh both
sleep 120
scripts/snapshot.sh > after.txt
diff before.txt after.txt
```

`reset-flows.sh both` destroys the sockets inside the balancer's network namespace
(`ss -K`), so the balancer's kernel sends a reset to the agent and to the server of
every flow.

## What to observe

- Agent logs: `could not read stream`, then `successfully connected to new proxy
  server` once per server as the re-mesh progresses.
- Agent `open_server_connections`, `server_connection_lost_total{reason}`,
  `server_connection_attempts_total{result}`.
- Server `ready_backends`, `established_connections_closed_total{reason="backend_close"}`,
  `dial_failure_count{reason="no_agent"}`, `backend_connection_duration_seconds`.
- Apiserver `dial_failure_total`, webhook fail-opens.
- Balancer access log for the destroyed flows.

## Results (2026-10-01)

11 tunnels open (`kubectl logs` loop), 3 agents, 5 servers:

| Signal | Observed |
|---|---|
| agent logs | 15 of 15 streams logged `could not read stream ... connection reset by peer` within 12 ms of T0 |
| agent restarts / readiness | 0 restarts; all agents stayed Ready (`/readyz` needs one server connection) |
| `server_connection_lost_total` | `{reason="recv_error"}` +5 per agent |
| tunnels | 11 → 0; every `kubectl logs -f` stream ended at T0 |
| servers | `ready_backends` 3 → 0 on all five within the same second; the first server stayed at 0 for 35 s |
| apiserver | `dial_failure_total{reason="endpoint"}` +66 (`No agent available`) over 25 s, then none |
| re-mesh | agents back to 5 of 5 servers after 36 s, 56 s and 77 s; `server_connection_attempts_total{result="connected"}` +5 per agent and about 80 further connections per agent through the balancer that were closed as duplicates |
| `backend_connection_duration_seconds` | 5 observations per server in the 60 s to 3600 s buckets, against about 50 in `le="1"` from the sync loop in the same window |
| balancer access log | the destroyed flows end with `flags=- details=-`; only the aggregate `upstream_cx_destroy` counters move |

## Interpretation

- The signature matches the production reports exactly, including the absence of
  restarts and the absence of any per-flow evidence on the balancer: an L4 balancer that
  resets flows at the kernel level leaves nothing in its access log.
- Recovery is bounded by the sync loop, which dials the balancer once per
  `--sync-interval` and discards the connection when it lands on a server the agent
  already knows. Reaching all N servers this way takes about N·ln(N) attempts: about 11
  attempts (57 s) for 5 servers, matching the 36 to 77 s measured; about 72 attempts
  (6 min) for 20 servers at a 5 s interval, matching the "more than five minutes"
  reported from production.
- `backend_connection_duration_seconds` is the server-side metric that separates this
  event from the sync-loop churn: long-lived connections ending together.

## Changes and their effect

The agent change measured in [00](00-baseline.md#changes-and-their-effect) leaves
recovery as described above: after a reset the agents dial every interval until each
holds every server (36 s, 71 s and 141 s in that run) and then stop dialing.

Making the agent dial faster while under-connected was considered and withdrawn. The
number of connections a re-mesh needs through a random balancer is about N·ln(N) per
agent whatever the pace; dialing faster only reserves that many ports on the VIP at
once instead of spreading them at the resting rate, and the apiservers recover as soon
as each server has its first agent back, which takes about `N × interval / agents`
(1 s at 100 agents and 20 servers). See
[00](00-baseline.md#connection-churn-and-port-exhaustion-at-the-vip).
