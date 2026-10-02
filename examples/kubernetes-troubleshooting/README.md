# Reproducing and troubleshooting proxy failures in a Kubernetes cluster

This directory holds the tooling and the record of experiments used to reproduce
connectivity incidents reported against the network proxy in production clusters,
and to measure what the proxy costs on the request path. The cluster is a
[kind](https://kind.sigs.k8s.io) cluster because kind gives you several control-plane
nodes behind an L4 load balancer (envoy), which is the shape where the interesting
failures happen: agents reach the servers through one virtual IP, and the balancer
decides which server each agent connection lands on.

The directory contains:

- `scripts/`: observe the proxy from every side (agents, servers, apiservers, load
  balancer) and inject faults on the agent-to-server path.
- `webhook/`: an always-allow validating admission webhook that measures its own
  handling time and connection churn, with a deploy script and a two-sided latency
  monitor.
- `webhook-load/`: a load generator that triggers the webhook with server-side dry-run
  requests.
- The sections below: how to set the cluster up, what each signal means, the scenarios,
  and the results collected so far.

## Prerequisites

- `docker`, `kind`, `kubectl`, `go`, `openssl`.
- With 8 or more kind nodes the host needs more inotify instances than the default
  256; otherwise kubelet and kube-proxy fail with `too many open files`:

  ```sh
  sudo sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=1048576
  ```

- The host kernel must have `CONFIG_INET_DIAG_DESTROY=y` for `ss -K` (socket destroy),
  which the fault injection uses. Check with
  `grep INET_DIAG_DESTROY /boot/config-$(uname -r)`.

## Cluster

Build the proxy images from your checkout and create the cluster with
`examples/kind-multinode`. Five control-plane nodes make the re-mesh behaviour visible
(see [Results](#results)); three workers give three agents.

```sh
make docker-build REGISTRY=local TAG=dev
cd examples/kind-multinode
./quickstart-kind.sh --cluster-name knp-test-cluster --num-kcp-nodes 5 --num-worker-nodes 3 \
  --server-image local/proxy-server-amd64:dev --agent-image local/proxy-agent-amd64:dev --sideload-images
```

In this layout each control-plane node runs a kube-apiserver and a konnectivity-server
(hostNetwork, agent port 8091). The kind load balancer listens on 8132 and forwards each
new connection to a random server. The agents dial `<cluster>-external-load-balancer:8132`,
so every agent connection goes through the balancer, as it does through a VIP in
production.

Raise the agent log level so that stream errors and reconnects are logged:

```sh
kubectl -n kube-system patch ds konnectivity-agent --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--v=3"}]'
```

Export the cluster name if you did not use the default; every script reads it:

```sh
export CLUSTER=knp-test-cluster
```

Output files go to `/tmp/$CLUSTER-troubleshooting/` (override with `OUT_DIR`).

## Where to look

`kubectl logs` and `kubectl exec` travel through the tunnel you are testing, including
for pods on control-plane nodes. The scripts read everything on the nodes instead,
through `docker exec`.

| Side | How to read it | Signals |
|---|---|---|
| agent | pod IP, port 8093, from its node: `docker exec <worker> curl http://<pod-ip>:8093/metrics` | `open_server_connections`, `known_server_count`, `server_connection_lost_total{reason}`, `server_connection_attempts_total{result}`, `stream_errors_total` |
| konnectivity-server | `docker exec <control-plane> curl localhost:8093/metrics` | `ready_backends`, `established_connections`, `established_connections_closed_total{reason}`, `backend_connection_duration_seconds`, `pending_backend_dials`, `dial_failure_count{reason}` |
| kube-apiserver, per replica | `docker exec <control-plane> kubectl --kubeconfig /etc/kubernetes/admin.conf --server https://localhost:6443 get --raw /metrics` | `konnectivity_network_proxy_client_client_connections{status}`, `..._dial_failure_total{reason}`, `apiserver_admission_webhook_admission_duration_seconds`, `..._fail_open_count`, `..._request_total{code}` |
| load balancer | envoy admin on `<lb-ip>:10000` (`/stats`, `/clusters`), reachable from any node; access log in `docker logs <lb>` | `upstream_cx_active`, `upstream_cx_destroy_{local,remote}`, `tcp.konnectivity_tcp.idle_timeout`; per-flow lines with `flags=` and `details=` |
| logs | `docker exec <node> crictl logs -f $(crictl ps --name konnectivity-agent -q)` (or `konnectivity-server`, `kube-apiserver`) | agent `could not read stream`, `successfully connected to new proxy server`; server `Stream read from agent cancelled`, `Agent connection closed, failing pending dial`, `Close established connections to agent`; apiserver `Failed calling webhook, failing open` |

Metrics that were added while working on these scenarios
(`server_connection_lost_total`, `server_connection_attempts_total`,
`backend_connection_duration_seconds`, `established_connections_closed_total`) are in
[kubernetes-sigs/apiserver-network-proxy#931](https://github.com/kubernetes-sigs/apiserver-network-proxy/pull/931).
The scripts print them when the images contain them and skip them otherwise.

Scripts:

| Script | What it does |
|---|---|
| `scripts/snapshot.sh` | One snapshot of all four sides. Run it before and after a fault and `diff` the two files. |
| `scripts/monitor.sh` | One line every 5 s with agent connections, server backends, established tunnels, apiserver dialing/ok/failures. |
| `scripts/tail-logs.sh` | Tails server, agent and balancer logs into `$OUT_DIR/logs/`. |
| `scripts/lb-tools.sh start` | Starts a privileged `netshoot` container in the balancer's network namespace, for `ss`, `iptables`, `tcpdump`. |
| `scripts/reset-flows.sh <mode>` | Resets all agent-to-server flows; see [Scenarios](#scenarios). |
| `webhook/deploy.sh` | Builds and deploys the measuring webhook and extracts kubeconfig credentials for the load generator. |
| `webhook/monitor.sh` | Every 10 s, apiserver-measured versus webhook-measured latency, fail-opens, connections. |

## Scenarios

### All agent connections reset at once

A balancer that drops its connection table sends a TCP reset on every flow it carried.
Which side sees the reset changes how the proxy behaves, so the script has three modes.
Start `scripts/monitor.sh`, `scripts/tail-logs.sh` and some tunnel traffic first (for
example a loop of `kubectl logs` against a pod on a worker, or the webhook load below).

```sh
scripts/lb-tools.sh start
scripts/snapshot.sh > before.txt
scripts/reset-flows.sh agents     # or: both, servers
sleep 120
scripts/snapshot.sh > after.txt
scripts/reset-flows.sh restore    # only needed after "agents"
diff before.txt after.txt
```

- `both`: destroys the sockets inside the balancer's namespace. Agents and servers
  both receive a reset. Servers drop their backends at once and new dials fail fast
  with `No agent available` until agents reconnect.
- `agents`: as `both`, with an iptables rule on each control-plane node that drops the
  resets going to the servers. Servers keep half-open backends and route new dials to
  them. This is the shape observed in production: `client_connections{status="dialing"}`
  rises and dials fail by timeout.
- `servers`: destroys the sockets on the server nodes. Only the servers see the loss.

### Webhook latency, steady state and during a reset

The webhook records its handler time; the apiserver records the whole call including
the dial and the tunnel. The difference is the cost of the path between them.

```sh
webhook/deploy.sh
CREDS=/tmp/$CLUSTER-troubleshooting/kubeconfig-creds
go run -mod=vendor ./examples/kubernetes-troubleshooting/webhook-load \
  -server "$(cat $CREDS/server)" -ca $CREDS/ca.crt -cert $CREDS/client.crt -key $CREDS/client.key \
  -workers 4 -rate 25            # 100 requests/s through the kind load balancer, all apiservers
webhook/monitor.sh               # in another terminal
```

The load generator creates ConfigMaps with `dryRun=All`, so the webhook runs and
nothing is stored. The webhook has `failurePolicy: Ignore` and `timeoutSeconds: 10`, so
a tunnel failure shows up as a fail-open and a latency spike instead of a rejected
request. Inject a reset with `scripts/reset-flows.sh` while the load runs.

## Results

Measured on one host, 5 control-plane nodes, 3 workers, agents through the kind envoy
balancer, `--sync-interval=5s`, `--sync-forever`, `--keepalive-time=1h` on both sides.

### Reset signature (2026-10-01)

Mode `both`, 11 tunnels open:

| Signal | Observed |
|---|---|
| agent logs | 15 of 15 streams logged `could not read stream ... connection reset by peer` within 12 ms |
| agent restarts / readiness | 0 restarts, all agents stayed Ready (`/readyz` needs one server connection) |
| tunnels | 11 → 0 |
| apiserver dial failures | `No agent available` for 25 s |
| server `ready_backends` | all five dropped to 0; the first server stayed at 0 for 35 s |
| full re-mesh | 36 s, 56 s and 77 s for the three agents |

Mode `agents` (servers unaware), 11 tunnels open:

| Signal | Observed |
|---|---|
| agent side | identical to `both` |
| servers | legs that envoy happened to close cleanly were dropped at T0; the other legs stayed registered as backends for 21 to 32 s and received new dials meanwhile |
| detection of a half-open backend | happens only when the server writes to it (a `DIAL_REQ`) and the write is not acknowledged within gRPC's TCP user timeout (20 s). `--keepalive-time` plays no part at 1 h. |
| apiserver | `client_connections{status="dialing"}` 0 → 2, `dial_failure_total{reason="endpoint"}` +1 per server with a half-open backend; `kubectl logs` calls into such a backend hung for more than 10 s |
| balancer access log | destroyed flows end with `flags=- details=-`; only the aggregate `upstream_cx_destroy` counters move. An L4 balancer that resets flows leaves no per-flow evidence. |

### Re-mesh cost

The sync loop dials the balancer once per `--sync-interval` and discards the connection
when it lands on a server the agent already knows. Reaching all N servers this way takes
about N·ln(N) attempts: about 11 attempts (57 s) for 5 servers, which matches the 36 to
77 s measured; about 72 attempts (6 min) for 20 servers at a 5 s interval, which matches
the "more than 5 minutes" seen in production. `server_connection_attempts_total`
shows this directly: after a reset each agent counted `connected` +5 and `duplicate` +80.

With `--sync-forever` the same loop runs at rest, so every agent opens and closes one
connection per interval forever. Here that is one new connection every 5 s per agent
(186 per 5 min for 3 agents through the balancer), and ~1,800
`stream_errors_total{segment="from_agent",code="Canceled"}` per server before any fault.
A real loss of all agent streams does not stand out in that counter or in the
`Connect`/cancel log lines; `backend_connection_duration_seconds` separates the two
populations (5 observations in the 60 s to 3600 s buckets against about 50 in `le="1"`
in the same window).

### Balancer idle timeout

Envoy's `tcp_proxy` default `idle_timeout` is 1 h, the same as `--keepalive-time`.
The access log shows the agent streams being recreated every hour and
`tcp.konnectivity_tcp.idle_timeout` counting them. Any middlebox with an idle timeout
at or below the gRPC keepalive interval prunes idle agent streams silently.

### Webhook latency (2026-10-02)

Steady state, 100 requests/s, four keep-alive connections from the apiservers to the
webhook:

| Measure | Value |
|---|---|
| webhook handler time (body read included) | 0.05 ms |
| apiserver-measured admission duration (mean from `_sum/_count`) | 1.3 to 1.5 ms |
| difference, which is the tunnel, TLS and HTTP cost per call | about 1.4 ms |
| client-observed end to end, through the kind balancer | 4.4 to 4.9 ms mean, p99 6 to 7 ms |
| new TCP connections at the webhook | 1 per 10 s (apiserver idle-connection recycling), 4 active |

Mode `agents` reset under that load:

| Measure | Value |
|---|---|
| fail-opens | 75 in 25 s (2.5% of calls), then 0 |
| per apiserver | one apiserver lost all three of its agent streams cleanly and fail-opened 70 calls with `No agent available` within 3 s; two apiservers had a keep-alive webhook connection pinned to a half-open backend and fail-opened 2+1 calls with `context deadline exceeded` plus 1+1 with `backend connection closed while dialing` at T0+20 s; two apiservers were unaffected |
| client latency | p99 20.004 s in one 5 s window, max 20.006 s; all other windows 4 to 5 ms |
| apiserver histogram | those calls landed in `le="25"`, above the webhook's `timeoutSeconds: 10` |
| webhook | handler time unchanged; active connections 4 → 0 → 4 |

The 20 s calls are `timeoutSeconds` plus the client library's `CloseTimeout`. The request
was written into a tunnel whose agent was gone. At 10 s the admission context expired;
`net/http` then calls `Close()` on the connection synchronously inside `RoundTrip`, and
the konnectivity client's `conn.Close()` sends `CLOSE_REQ` and waits for the `CLOSE_RSP`
for up to `CloseTimeout` (10 s) before returning. The apiserver logged
`context deadline exceeded` at exactly start + 10 s + 10 s, 250 ms before the server
detected the dead stream, so the wait ended by timeout. A webhook `timeoutSeconds` is
therefore not an upper bound on the admission call when the tunnel behind it is dead;
the call takes `timeoutSeconds + 10 s`, and the apiserver request goroutine is blocked
for that time. This needs a change in `konnectivity-client` so that `Close()` returns
without waiting for the agent.

## Open items

- Run mode `servers` under webhook load and compare fail-fast behaviour with mode `agents`.
- Starve one agent's node of CPU (`docker update --cpus 0.05 <worker>`) to reproduce
  "one slow agent slows 1/N of all dials", since the server picks a backend at random.
- Switch the apiserver egress selector to `Direct` and restart the apiservers to measure
  the tunnel cost against a direct path on the same host.
- Fix `conn.Close()` in `konnectivity-client` so that a dead backend does not add
  `CloseTimeout` to every request that times out on it.
- Decide whether the agent should resync aggressively after losing all server
  connections instead of one random dial per `--sync-interval`.

## Cleanup

```sh
scripts/lb-tools.sh stop
kubectl delete validatingwebhookconfiguration knp-webhook
kubectl delete namespace webhook-load
kind delete cluster --name "$CLUSTER"
```
