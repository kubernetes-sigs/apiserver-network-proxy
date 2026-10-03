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
| detection of a half-open backend | 21 to 32 s after T0. The server's writes into the dead leg (a `DIAL_REQ`, or data for a tunnel) are retransmitted and never acknowledged; the kernel aborts the connection when the oldest unacknowledged write is 20 s old, the TCP user timeout that gRPC sets from its keepalive timeout. See the [detection mechanics](#how-a-server-detects-a-half-open-backend) below. |
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

- The servers do not notice a half-open agent stream until a write into it goes
  unacknowledged for the TCP user timeout (20 s), or, with no write at all, until the
  kernel's TCP keepalive gives up (15 to 30 s). During that time the server routes new
  dials into the dead backend and the apiserver's requests into it wait.
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

### How a server detects a half-open backend

Measured with `tcpdump` in the balancer's namespace on the server leg (`tcp port 8091`)
during `reset-flows.sh agents`, with the servers at `--keepalive-time=1h` and the gRPC
default keepalive timeout of 20 s. Two mechanisms end a half-open connection, both in
the kernel:

- A write. The server writes a `DIAL_REQ`, tunnel data, or a gRPC `PING` into the dead
  leg; the segment is retransmitted with exponential backoff (0.2, 0.4, 0.8, 1.6, 3.2,
  6.4 s) and never acknowledged. gRPC sets `TCP_USER_TIMEOUT` on every agent connection
  to its keepalive timeout, so the kernel aborts the connection once the oldest
  unacknowledged write is that old: 20 s after the first write. In the capture every leg
  that carried traffic at T0 shows its last retransmission at T0 + 13.5 s and no further
  packet; legs that received their first `DIAL_REQ` later (T0 + 11 s, T0 + 20.6 s) were
  aborted at that time + 20 s. The server logs `Stream read from agent cancelled` at the
  abort.
- No write. Go enables TCP keepalive on accepted sockets with a 15 s idle time and a
  15 s interval. The capture shows one empty probe (`Flags [.], length 0`) 7 to 13 s
  after T0, no answer, and a `[R.]` from the server 15 s later: with `TCP_USER_TIMEOUT`
  set, the kernel aborts at the first keepalive tick that finds the connection
  unanswered for longer than the user timeout. An idle half-open backend therefore lives
  15 to 30 s (log: 15.2 to 27.9 s across the five servers).

`--keepalive-time=1h` means the server itself never writes to an idle stream; the gRPC
ping that this flag controls is what would turn the second case into the first one at
a chosen interval. Both mechanisms above are TCP and watch the first hop only. Through a
balancer that terminates TCP the server's peer is the balancer: when the agent leg dies
and the balancer keeps the server leg open, the server's socket stays healthy and neither
the TCP keepalive nor the user timeout fires. The gRPC keepalive is an HTTP/2 `PING`
frame that the balancer relays and the agent itself answers, so it is the detection
that works on every kind of path, with the kernel timeouts as an extra on the first
hop. Its acknowledgement timeout was not configurable on the server.

## Changes and their effect

### `konnectivity-client`: `conn.Close()` returns without waiting for `CLOSE_RSP`

Measured on 2026-10-03 with `tunnel-probe`, which sends one request every 500 ms over a
single keep-alive connection through a konnectivity-server's unix socket, with a 10 s
per-request timeout, the way the apiserver calls a webhook. Two probes ran side by side
on two control-plane nodes: one built against the previous client, one against the
changed client. Then `reset-flows.sh agents`.

| | previous `Close()` | changed `Close()` |
|---|---|---|
| request in flight on the pinned connection at T0 | 20,018 ms, `context deadline exceeded` | 10,000 ms, `context deadline exceeded` |
| next request | 1 ms (fresh dial reached a live agent) | 10,000 ms: the fresh dial was routed to another half-open backend, which the server detected 10 s later |
| requests completed in the first 60 s | 79 | 79 |
| everything else | at most 6 ms | at most 6 ms |

The change removes the 10 s that `Close()` added on top of the request's own timeout;
the request now takes exactly `timeoutSeconds`. What remains is the server routing new
dials into half-open backends for up to 20 s after the reset, which the second request
on the changed client shows; that is addressed by change 4 in the
[fixes table](../README.md#fixes).

### Server: `--keepalive-timeout`, with `--keepalive-time=10s --keepalive-timeout=5s`

The server gains `--keepalive-timeout` (default 20 s, the gRPC default, so the default
behaviour is unchanged). It is the time the server waits for the agent to acknowledge
its keepalive ping, an HTTP/2 `PING` frame, before closing the connection; the
acknowledgement comes from the agent, so a balancer in between does not hide a dead
agent. gRPC also sets the same value as the TCP user timeout of the socket, which covers
the first hop in addition. With
`--keepalive-time=10s` the server pings each idle agent connection every 10 s; the ping
is a 17-byte HTTP/2 frame (39 bytes inside TLS) on the existing connection. It opens no
connection and takes no port on the balancer: the cost is one small packet and its
acknowledgement per agent connection per `--keepalive-time`, that is
`agents × servers / keepalive-time` packets per second across the control plane, and it
keeps the flows alive in the balancer's and any NAT's tables. The expected detection
time for a half-open backend becomes at most `keepalive-time + keepalive-timeout` with
no traffic and `keepalive-timeout` after the first write with traffic.

Measured on 2026-10-03 with the servers rolled from `--keepalive-time=1h` (keepalive
timeout 20 s) to `--keepalive-time=10s --keepalive-timeout=5s`, everything else equal
(agents with the sync change from 00, `konnectivity-client` with the `Close()` change in
`tunnel-probe`; the apiservers' own client is unchanged).

Idle, no tunnel traffic (`reset-flows.sh agents`, `tcpdump` on the server leg):

| | 1h / 20 s | 10 s / 5 s |
|---|---|---|
| first packet the server sends into a dead leg | TCP keepalive probe, 7 to 13 s after T0 | gRPC ping, 0.1 to 6.6 s after T0 |
| abort of the connection | 15 s after the probe: T0 + 22.3 s and T0 + 27.9 s | 5 s after the ping: T0 + 5.1 to T0 + 11.6 s |
| half-open backends gone from all five servers | 28 s | 12 s |

Under load (`webhook-load` at 100 requests/s and `tunnel-probe` every 500 ms with a
10 s timeout, then `reset-flows.sh agents`):

| | 1h / 20 s | 10 s / 5 s |
|---|---|---|
| server: abort of a leg that carried traffic at T0 | first write + 20 s (last retransmission at T0 + 13.5 s, then silence) | first write + 5 s (last retransmission at T0 + 3.3 s) |
| server: abort of a leg that received its first dial later | T0 + 31 s and T0 + 41 s | T0 + 11 s |
| `tunnel-probe` | 3 consecutive requests of 10 s, `context deadline exceeded`; 32 s without a successful request | 2 consecutive requests of 5.4 s, `backend connection closed while dialing`; 12 s without a successful request |
| `webhook-load` | one call of 20.0 s (the apiserver's `timeoutSeconds + CloseTimeout`), two of 10.0 s; 50 requests/s instead of 100 for 15 s | one call of 5.65 s; one 5 s window at 13 requests/s and one at 70 |
| fail-opens | 87 | 11 |

The fail-open counts depend on which legs the balancer happened to close cleanly at T0
(those fail with `No agent available` at once) and are not comparable across runs; the
stall durations and the server-side abort times are. Dials into a half-open backend now
fail after 5 s with `backend connection closed while dialing` instead of waiting for the
caller's timeout, and the apiserver's own webhook calls, whose client still blocks in
`Close()`, end at 5.6 s instead of 20 s because the server closes the frontend
connection when it drops the backend.

With these values the kernel's TCP keepalive (15 s idle) never fires on an agent
connection, since the gRPC ping keeps it busy; the only timers left are the two flags.
Smaller values shorten the detection further at the price of more pings; the lower
bound is what the agents tolerate (a ping is answered by the transport, the agent's
`--keepalive-time` and the server's enforcement policy only concern pings the agent
sends).

`--backend-dial-timeout` with the timed-out backend marked draining, considered for the
same purpose, is not pursued: it fires on a slow endpoint behind a healthy agent as
readily as on a dead agent (the agent's own dial takes up to 5 s), and the transport
timeout above removes the dead agent without that ambiguity.

### Recommendation

The server defaults (`--keepalive-time` 1 h, `--keepalive-timeout` 20 s) and the
example manifests are unchanged. Behind a balancer or a NAT that can drop flows without
telling both ends, run the servers with `--keepalive-time` of 10 to 30 s and
`--keepalive-timeout` of 5 to 10 s. The cost is `agents × servers / keepalive-time`
in-band pings per second across the control plane; a half-open backend is then gone
within the sum of the two values with no traffic and within `--keepalive-timeout` of
the first write with traffic.
