# 05. All agent connections reset, seen by the servers only, through a NAT

Same loss as [03](03-reset-seen-by-servers-only.md), with one difference in the path:
the balancer does not relay the close to the agents. The servers drop their backends at
once; the agents keep half-open streams, count them as live connections, and do not dial.
This is what a NAT-style VIP produces when the server side of a flow goes away: the
server's reset never reaches the agent, and the agent has no proxy in front of it to
close the connection on its behalf.

Production symptom this maps to: after a balancer or control-plane event, one or more
servers stay at `ready_backends` 0 or below the agent count for an hour, every tunnel
call through their apiservers fails with `No agent available`, and the agents report
`open_server_connections` equal to the server count the whole time.

## Preconditions

As in [01](01-reset-seen-by-both-sides.md), with the webhook load and `dial-load.sh`
running. The lb-tools helper container must be present.

## Steps

```sh
scripts/snapshot.sh > before.txt
scripts/reset-flows.sh servers-nat
sleep 120
scripts/snapshot.sh > after.txt
scripts/reset-flows.sh restore
diff before.txt after.txt
```

`reset-flows.sh servers-nat` first adds an iptables rule on each worker node that drops
TCP resets arriving from the balancer's agent port, then destroys both legs of every
agent connection in the balancer's namespace as in 01. The servers receive their
resets; the agents do not. `restore` removes the rules.

## What to observe

- Agent logs: which streams end at T0 with `EOF` (legs the balancer closed cleanly
  before its socket was destroyed) and which stay silent. A silent stream ends later
  with `read: connection timed out` (a write into it went unacknowledged for the TCP
  user timeout) or with `keepalive ping failed to receive ACK within timeout`.
- Agent `open_server_connections` against the servers' `ready_backends`: the agents
  claim connections the servers do not have.
- Server `ready_backends` per server and `dial_failure_count{reason="no_agent"}`.
- `dial-load.log`: `kubectl exec` failures, which continue for as long as one server
  has no agent, since each `kubectl` call lands on a random apiserver.
- In the agent's network namespace, `ss -tnoi 'dport = :8132'` shows the half-open
  sockets with `timer:(keepalive,...)` counting down from the kernel default.

## Results (2026-10-03, webhook load at 100 requests/s, `dial-load.sh` at 0.1 s)

Agents at `--keepalive-time=1h` (no keepalive timeout flag; gRPC's default of 20 s).

| Measure | Value |
|---|---|
| streams closed cleanly by the balancer | 8 of 15, `EOF` at T0 + 0.1 s; the agents replaced them within 40 s |
| half-open streams | 7 of 15. One, which carried traffic at T0, ended at T0 + 20.5 s with `read: connection timed out`. The other six were still open 16 minutes later, with `open_server_connections` 5 on every agent |
| half-open sockets | `timer:(keepalive,56min,0)`, `lastrcv` growing: the agent's sockets use the kernel's default TCP keepalive (7,200 s), and the agent's own gRPC ping is 1 h away |
| servers | `ready_backends` 3 → 0 on all five at T0; four servers had an agent back within 11 s; one server stayed at 0 for the rest of the observation (16 minutes), with 2, 3, 2 and 2 backends on the other four |
| apiservers | 777 fail-opens in the first 12 s (every server at 0), then none: the load generator's connections happened to sit on apiservers whose server had agents |
| `kubectl exec` | 168 of 4,966 calls failed (3.4%) over the 16 minutes, between 3 and 13 in every minute: the share of `kubectl` calls that land on the apiserver whose server has no agent, which answers `No agent available` |

## Interpretation

- The agent detects a half-open server stream the same way the server detects a
  half-open agent stream ([02](02-reset-seen-by-agents-only.md#how-a-server-detects-a-half-open-backend)):
  a write unacknowledged for the TCP user timeout, which gRPC sets from the keepalive
  timeout (20 s), or a keepalive. The difference is in the keepalive. The server's
  listener gets Go's TCP keepalive at 15 s; the gRPC client dialer enables TCP keepalive
  with the kernel's parameters, 7,200 s idle. The agent's own gRPC ping, the HTTP/2
  `PING` that crosses a balancer and that the server itself answers, is governed by
  `--keepalive-time`, 1 h. An idle half-open stream therefore lives one hour on the agent.
- The agent counts the stream as a connection to that server, so with lease counting it
  does not dial (`open_server_connections` equals the lease count), and without lease
  counting the sync dial that lands on that server is closed as a duplicate. Nothing
  replaces the stream before the agent drops it.
- The 5 s `--probe-interval` reads the gRPC channel state; that state changes only when
  the transport notices, so it adds nothing here.
- Through a proxying balancer (03) this does not happen, because the proxy closes the
  agent leg when its server leg fails. The difference between 03 and this scenario is
  the balancer, which is why the same event recovers in seconds in one environment and
  in an hour in another.

## Changes and their effect

### Agent: `--keepalive-timeout`, with `--keepalive-time=30s --keepalive-timeout=5s`

The agent gains `--keepalive-timeout` (default 20 s, the gRPC default, so the default
behaviour is unchanged): the time it waits for the server to acknowledge its keepalive
ping, an HTTP/2 `PING` frame, before closing the connection; gRPC also sets the same
value as the TCP user timeout of the socket, which covers the first hop in addition.
`--keepalive-time` already existed; its lower bound is the servers'
keepalive enforcement minimum of 30 s, below which a server answers the second ping
with `GOAWAY too_many_pings` and the agent reconnects. With the servers pinging every
10 s (the [02](02-reset-seen-by-agents-only.md#recommendation) setting), an agent at
`--keepalive-time=30s` never pings a live connection, since every server ping resets
its timer; it pings only a connection that has been silent for 30 s, and gives up 5 s
later. The expected detection time for a half-open server stream is therefore
`keepalive-timeout` after the agent's first write with traffic, and
`keepalive-time + keepalive-timeout` without.

Measured on 2026-10-03, same load, agents rolled from `--keepalive-time=1h` to
`--keepalive-time=30s --keepalive-timeout=5s`, servers unchanged:

| | 1 h / 20 s | 30 s / 5 s |
|---|---|---|
| half-open stream with traffic at T0 | ended at T0 + 20.5 s, `connection timed out` | ended at T0 + 5.5 s, `connection timed out` (three streams) |
| half-open stream without traffic | still open after 16 minutes; bound 1 h | ended at T0 + 26.9 to 34.6 s, `keepalive ping failed to receive ACK within timeout` (seven streams) |
| servers with an agent back | four within 11 s, one never (bound 1 h) | all five within 33 s |
| all 15 streams back | no | T0 + 86 s |
| `kubectl exec` failures | 3.4% of calls, continuing | 54 calls, none after T0 + 30 s |

The fail-open counts in the first 40 s are not comparable between the runs (1,387
against 777): they depend on how many legs the balancer closed cleanly at T0 and on
which apiservers the load generator's connections sit, and both differ per run. The
lasting part is: with the change no server is left without agents once the agents have
had `keepalive-time + keepalive-timeout` to notice.

### Recommendation

Behind a balancer or NAT that can drop a flow without telling both ends, run the agents
with `--keepalive-time=30s` (the floor the servers enforce) and `--keepalive-timeout`
of 5 to 10 s, together with the server settings from
[02](02-reset-seen-by-agents-only.md#recommendation). The agent then pings a server only
after 30 s of silence, so the pings cost nothing while the servers' own pings keep the
connections busy, and a server stream the agent has lost is replaced within about 35 s
plus a sync interval instead of an hour. The defaults and the example manifests are
unchanged.
