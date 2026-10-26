# 04. One agent starved of CPU

The server picks an agent at random for every new dial (two random candidates,
compared on receive-channel occupancy), and a connection stays on that agent for its
whole life. An agent on a node with no idle CPU therefore slows every connection pinned
to it and about 1/N of all new dials, on every apiserver at once, with no error anywhere.

Production symptom this maps to: dial p99 rising on all apiservers at the same time
while p50 stays flat, a small percentage of webhook calls crossing hundreds of
milliseconds, and the whole thing clearing when one node gets CPU back.

## Preconditions

- [00-baseline](00-baseline.md) running, including `scripts/dial-load.sh`.
- Find which agent holds the webhook's keep-alive tunnels:
  `scripts/snapshot.sh | grep open_endpoint_connections`. Starving that agent shows
  the pinned-connection effect; starving another one shows only the new-dial effect.

## Steps

```sh
scripts/snapshot.sh > before.txt
scripts/starve-agent.sh <worker-node> 5000   # 5% of one CPU
sleep 1200
scripts/snapshot.sh > starved-5pct.txt
scripts/starve-agent.sh <worker-node> 1000   # 1% of one CPU
sleep 150
scripts/snapshot.sh > starved-1pct.txt
scripts/starve-agent.sh <worker-node> restore
```

`starve-agent.sh` writes `cpu.max` of the agent container's own cgroup, so kubelet and
the other pods on the node are untouched. The script prints the cgroup's throttling
counters so you can confirm the limit is biting.

## What to observe

- `scripts/dial-share.sh 150`: the starved agent's share of new dials and its mean dial
  time next to the other agents', and the servers' dial time distribution.
- Server `dial_duration_seconds`: the fraction above 25 ms and 100 ms, and the mean.
  The median stays put; the tail is the starved agent's share of dials.
- Agent `dial_duration_seconds` on the starved agent versus the others.
- `webhook/monitor.sh`: `api_mean`, `api_over_100ms`, with `webhook_mean` unchanged.
- `webhook-load` output: p99 per window, and whether the request rate drops.
- `dial-load.log`: `kubectl exec` round trip.
- Agent readiness and restarts.

## Results (2026-10-02)

One of three agents starved; all four webhook keep-alive tunnels happened to be pinned
to it; one third of the dial load went through it. The agent stayed Ready with no
restarts and no probe failures in both runs. The webhook handler time stayed at 0.05 ms
throughout, so every increase below is on the path.

| Measure | baseline | 5% of one CPU, 20 min | 1% of one CPU, 150 s |
|---|---|---|---|
| apiserver-measured admission duration, mean | 1.5 ms | 1.9 ms | 120 to 155 ms |
| admission calls above 100 ms | 0 | 67 of 121,726 (0.055%) | 40 to 50% |
| client p99 per 5 s window | 6 ms | above 20 ms in 93 of 246 windows, max 200 ms | 300 ms typical, max 700 ms |
| client throughput | 100 requests/s | 100 requests/s | 28 requests/s (all four workers queued behind the one agent) |
| server `dial_duration_seconds`, all dials | 0.9 ms mean, none above 25 ms | 0.9 ms mean, 0.1% above 25 ms | 32 ms mean, **33.7% above 25 ms**, 11% above 100 ms |
| `kubectl exec` round trip | p50 187 ms, p99 218 ms | p50 186 ms, p99 236 ms, max 423 ms | p50 190 ms, p90 697 ms, p99 1018 ms, max 1443 ms |
| starved agent's own `dial_duration_seconds` (agent to endpoint) | 0.24 ms | 0.24 ms | 19 ms (other agents 0.2 ms) |
| fail-opens, errors | 0 | 0 | 0 |

## Interpretation

- At 5% the pinned webhook tunnels already show it (p99 up, calls above 100 ms appear)
  while the dial path is almost untouched: a dial needs only a few milliseconds of CPU
  and fits inside the quota most of the time.
- At 1% the dial path shows the expected shape exactly: one third of all dials, the
  starved agent's share, are slow, the median is unchanged and the tail grows by
  hundreds of milliseconds. This is how "one slow agent in N" appears in a dial
  histogram, and it confirms the mechanism described in the production report at
  N = 100 (1% of dials slow).
- The client-side latencies cluster at multiples of 100 ms, the CFS period: the agent
  runs out of quota and waits for the next period.
- Nothing fails and the agent stays Ready, so no alert based on errors or readiness
  fires. The dial histogram on the server is the signal.
- Which agent carries the pinned tunnels is decided once per dial and persists for the
  life of the connection; before the previous reset the four webhook tunnels were
  spread 3+1 over two agents, after it they all landed on one. A reset re-rolls that
  placement.

## Changes and their effect

### Server: compare the two candidates on expected dial time

The server keeps per backend the number of dials in flight (a `DIAL_REQ` sent, no
`DIAL_RSP` yet) and a moving average of the completed dials' latency as the server
sees it (from the apiserver's request to the agent's `DIAL_RSP`; failed dials do not
count, since a failing endpoint says nothing about the agent). The two random candidates
are compared on the product of the two, the expected time for a new dial: latencies
under 5 ms count as equal, a sample older than 30 s is forgotten so that the agent is
tried again, and a candidate whose cost is at least twice the other's loses. Otherwise
the receive-channel comparison and then chance decide, as before. The selection stays
local to each server and needs no new traffic.

Measured on 2026-10-03 with `dial-load.sh` at an interval of 0.1 s (about four dials
per second), `webhook-load` at 100 requests/s, and the agent that held the webhook's
keep-alive tunnels (two before, three after; the servers' restart re-rolled the
placement) starved to 1% of one CPU for 150 s. Baseline before each run: 0.8 ms mean
server dial time, no dial above 25 ms, shares within 26 to 39%.

| | occupancy only | expected dial time |
|---|---|---|
| starved agent's share of new dials | 35.2% (137 of 390) | 5.8% (31 of 530) |
| dial time at the starved agent, mean | 31.5 ms | 22.8 ms |
| server dial time, all dials, mean | 31.1 ms | 5.3 ms |
| dials above 25 ms | 27.4% | 3.8% |
| dials above 100 ms | 11.5% | 1.7% |
| `kubectl exec` round trip p50 / p90 / p99 / max | 181 / 503 / 697 / 1,197 ms | 163 / 187 / 497 / 1,089 ms |
| webhook calls over the pinned tunnels, p99 per 5 s window | 200 to 300 ms in 30 of 31 windows | 200 ms in 31 of 31 windows |

The 31 dials the starved agent still received are the first dials before any sample
existed plus one retry per server after each 30 s window (five servers, five windows);
those are the dials that remain slow. The connections pinned to the agent before it was
starved stay there and stay slow: selection decides where a new dial goes and cannot
move an established connection.

A run with the starved agent carrying no pinned tunnel (its only work the dials
themselves) is not comparable: within 1% of a CPU the agent still dialed in 2.7 ms on
average, so it kept a 25% share on merit, and the servers' mean stayed at 1.4 ms.
