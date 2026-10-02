# 02. All agent connections reset, seen by the agents only

Same event as [01](01-reset-seen-by-both-sides.md), with the resets toward the servers
dropped. The agents learn at once that their streams are dead; the servers do not, and
keep the dead agents registered as backends. This is the variant that matches the
production symptoms: apiserver connections stuck in `dialing`, dial failures by timeout,
webhook fail-opens, and a slow recovery.

## Preconditions

As in [01](01-reset-seen-by-both-sides.md). The webhook load matters here: it keeps
long-lived connections pinned to agents, which is where the damage shows.

## Steps

```sh
scripts/snapshot.sh > before.txt
scripts/reset-flows.sh agents
sleep 120
scripts/snapshot.sh > after.txt
scripts/reset-flows.sh restore
diff before.txt after.txt
```

`reset-flows.sh agents` first adds an iptables rule on each control-plane node that
drops TCP resets arriving from the balancer on port 8091, then destroys the sockets in
the balancer's namespace as in 01. The servers' sockets stay in `ESTABLISHED` until the
server writes to them and the write is not acknowledged. `restore` removes the rules.

## What to observe

In addition to the signals of 01:

- Server logs: `Stream read from agent cancelled` arriving 20 to 30 s after T0 for the
  half-open backends, followed by `Agent connection closed, failing pending dial` and
  `Close established connections to agent`.
- Server `dial_failure_count{reason="backend_close"}`, apiserver
  `client_connections{status="dialing"}`.
- Apiserver log: `Failed calling webhook, failing open ...` with the reason after
  `failed to call webhook:`.
- `webhook-load` output: p99 and max per 5 s window, and the request rate.

## Results

### 2026-10-01, `kubectl logs` loop, 11 tunnels

| Signal | Observed |
|---|---|
| agent side | identical to 01: 15 of 15 streams reset within 8 ms, `recv_error` +5 per agent |
| servers | legs that the balancer happened to close cleanly were dropped at T0; the other legs stayed registered as backends for 21 to 32 s and received new dials meanwhile |
| detection of a half-open backend | only when the server wrote to it (a `DIAL_REQ`) and the write was not acknowledged within gRPC's TCP user timeout (20 s). `--keepalive-time` plays no part at 1 h. |
| apiserver | `client_connections{status="dialing"}` 0 → 2; `dial_failure_total{reason="endpoint"}` +1 per server with a half-open backend; `kubectl logs` calls into such a backend hung for more than 10 s |
| re-mesh | 36 to 77 s, as in 01 |

### 2026-10-02, webhook load at 100 requests/s

| Measure | Value |
|---|---|
| fail-opens | 75 in 25 s (2.5% of calls), then 0 |
| per apiserver | one apiserver lost all three of its agent streams cleanly and fail-opened 70 calls with `No agent available` within 3 s; two apiservers had a keep-alive webhook connection pinned to a half-open backend and fail-opened 2+1 calls with `context deadline exceeded` plus 1+1 with `backend connection closed while dialing` at T0+20 s; two apiservers carried no load-generator connection |
| client latency | p99 20.004 s in one 5 s window, max 20.006 s; all other windows 4 to 5 ms |
| client throughput | 100 → 25 requests/s for 20 s: three of the four workers were blocked on those calls |
| apiserver histogram | the stalled calls landed in `le="25"`, above the webhook's `timeoutSeconds: 10` |
| webhook | handler time unchanged; active connections 4 → 0 → 4; 6 new connections during re-mesh |

## Interpretation

- The servers do not notice a half-open agent stream until they write to it. With
  `--keepalive-time=1h` there is no periodic write, so detection depends on traffic
  and takes the TCP user timeout (20 s) after the first write. During that time the
  server routes new dials into the dead backend and the apiserver's requests into it
  wait.
- The 20 s calls are `timeoutSeconds` plus the client library's `CloseTimeout`. The
  request was written into a tunnel whose agent was gone. At 10 s the admission context
  expired; `net/http` then called `Close()` on the connection synchronously inside
  `RoundTrip`, and `konnectivity-client`'s `conn.Close()` sent `CLOSE_REQ` and waited for
  the `CLOSE_RSP` for up to `CloseTimeout` (10 s) before returning. The apiserver logged
  `context deadline exceeded` at exactly start + 10 s + 10 s, 250 ms before the server
  detected the dead stream, so the wait ended by timeout. A webhook `timeoutSeconds` is
  therefore not an upper bound on the admission call when the tunnel behind it is dead;
  the call takes `timeoutSeconds + 10 s`, and the apiserver request goroutine is blocked
  for that time.
- Compared with [03](03-reset-seen-by-servers-only.md): fewer calls fail here, but each
  failing call holds its request for 20 s and the client loses throughput. The
  production symptoms match this variant, so the damage comes from the servers not
  learning about the reset.

## Changes and their effect

None yet. Planned, in this order:

1. `konnectivity-client`: `conn.Close()` returns without waiting for `CLOSE_RSP`.
   Expected: the 20 s calls become 10 s (the webhook's own timeout) and the client
   keeps its throughput.
2. Server: `--keepalive-time` default low enough to detect a half-open stream without
   traffic; `--backend-dial-timeout` enabled and a backend that times out a dial marked
   draining. Expected: half-open backends leave the selection within seconds.
3. Agent: fast re-sync while under-connected. Expected: re-mesh in seconds.
