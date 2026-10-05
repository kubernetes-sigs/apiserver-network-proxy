# 03. All agent connections reset, seen by the servers only

Same event as [01](01-reset-seen-by-both-sides.md), injected on the server nodes. The
servers learn at once and drop their backends; the balancer relays the close to the
agents. Nobody keeps a dead backend. This is the fail-fast reference for
[02](02-reset-seen-by-agents-only.md).

## Preconditions

As in [01](01-reset-seen-by-both-sides.md), with the webhook load running.

## Steps

```sh
scripts/snapshot.sh > before.txt
scripts/reset-flows.sh servers
sleep 120
scripts/snapshot.sh > after.txt
diff before.txt after.txt
```

`reset-flows.sh servers` destroys the agent-facing sockets (`sport = :8091`) on every
control-plane node at the same time.

## What to observe

- Agent logs: the agents see the balancer closing the downstream connection, which
  arrives as `could not read stream ... error reading from server: EOF`. It is counted
  under `server_connection_lost_total{reason="recv_error"}` because the stream ended
  without a graceful end of stream; `reason="eof"` is reserved for a server that ends
  the stream itself.
- Server `ready_backends` and `dial_failure_count{reason="no_agent"}`.
- Apiserver fail-open reasons and the time window in which they occur, per apiserver.
- Server logs: `Register backend for agent` marks when each server got an agent back.

## Results (2026-10-02, webhook load at 100 requests/s)

| Measure | Value |
|---|---|
| fail-opens | 532 in 12 s, then 0 |
| per apiserver | 66, 297 and 169 calls, all `No agent available` except two `EOF` on the keep-alive connections that were open at T0. Each apiserver failed every call from T0 until its server registered its first agent again: 1.4 s, 12 s and 6.9 s. The two apiservers that the kind balancer had not assigned any load-generator connection to show nothing. |
| client latency | unchanged: mean 4.5 to 5.2 ms, p99 at most 8.4 ms, max 73 ms |
| client throughput | 100 requests/s throughout |
| apiserver histogram | no call above 100 ms |
| servers | `ready_backends` 3 → 0 on all five within 4 s; first agent back after 1.5 to 17 s; all 15 streams back at T0+86 s; `dial_failure_count{reason="no_agent"}` +66, +296, +168, equal to the fail-opens |
| webhook | handler time unchanged; active connections 4 → 3 → 4 |

## Interpretation

Both 02 and 03 inject the same fault on the same code; they differ only in which side
learns that the connections are gone.

- When the servers see the loss (this scenario), they drop their backends at once,
  every dial fails with `No agent available` and the apiserver fails open immediately.
  The outage is complete for the apiservers whose server has no agent, and lasts until
  the first agent comes back: a few seconds with three agents and a 5 s sync interval,
  longer with a larger server count because the first agent to reach a given server is
  a random event.
- When the servers do not see the loss (02), they keep routing dials and requests into
  dead backends; fewer calls fail, but each failing call holds its request for
  `timeoutSeconds + CloseTimeout` and the client loses throughput.
- With `failurePolicy: Fail`, this scenario is a short hard outage and 02 a long
  latency stall on a fraction of requests.

## Changes and their effect

None that target this scenario. The no-agent window per server is `N × interval /
agents` on average (1.4 s, 12 s and 6.9 s measured here with 3 agents and 5 servers;
about 1 s at 100 agents and 20 servers), and shortening it by dialing faster was
withdrawn because of its cost in VIP ports; see
[00](00-baseline.md#connection-churn-and-port-exhaustion-at-the-vip).
