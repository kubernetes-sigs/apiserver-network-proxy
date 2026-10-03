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
- `tunnel-probe/`: a client that sends requests through a konnectivity-server's unix
  socket over one keep-alive connection, the way the apiserver calls a webhook, and logs
  each request's duration. Build it against two versions of `konnectivity-client` to
  compare them on the same fault.
- `scenarios/`: one document per fault scenario, with steps, measurements, and the
  effect of each fix once it lands.

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
(see [01](scenarios/01-reset-seen-by-both-sides.md)); three workers give three agents.

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
| `scripts/reset-flows.sh <mode>` | Resets all agent-to-server flows; `both`, `agents`, `servers`, `restore`. See scenarios 01 to 03. |
| `scripts/starve-agent.sh <node> [quota]` | Limits the agent container on one node to `quota` µs of CPU per 100 ms through its cgroup (default 5000, 5% of one CPU); `restore` lifts the limit. |
| `scripts/dial-load.sh` | Runs `kubectl exec` in a loop; each call is a new tunnel dial through a randomly chosen agent. Logs per-call latency to `$OUT_DIR/dial-load.log`. |
| `scripts/vip-churn.sh [seconds]` | Counts the new connections through the balancer in the interval, how many produced a new server connection, how many were closed as duplicates, and the sockets the balancer holds toward the servers. |
| `webhook/deploy.sh` | Builds and deploys the measuring webhook and extracts kubeconfig credentials for the load generator. |
| `webhook/monitor.sh` | Every 10 s, apiserver-measured versus webhook-measured latency, fail-opens, connections. |
| `tunnel-probe` | Runs on a control-plane node (`docker cp` the binary to `/usr/local/bin`; `/tmp` on kind nodes is a tmpfs that `docker cp` does not reach). `kt-probe -url http://<webhook-pod-ip>:9090/metrics -timeout 10s`. |

## Scenarios

Each scenario has its own document with the fault it injects, the production symptom it
maps to, the exact steps, the signals to watch, the measurements, and, once a fix
exists, the measurements with the fix applied.

| Document | What it covers |
|---|---|
| [00-baseline](scenarios/00-baseline.md) | The cluster with no fault: cost of the tunnel per webhook call, cost of a dial, the sync loop at rest, the balancer's idle timeout. |
| [01-reset-seen-by-both-sides](scenarios/01-reset-seen-by-both-sides.md) | All agent-to-server connections reset at once, both ends notice. Reset signature and re-mesh time. |
| [02-reset-seen-by-agents-only](scenarios/02-reset-seen-by-agents-only.md) | Same reset, servers kept unaware. Half-open backends, `dialing` pile-up, the `timeoutSeconds + CloseTimeout` stall. Matches the production symptoms. |
| [03-reset-seen-by-servers-only](scenarios/03-reset-seen-by-servers-only.md) | Same reset, servers notice first. Fail-fast reference for 02. |
| [04-cpu-starved-agent](scenarios/04-cpu-starved-agent.md) | One agent on a node without idle CPU: 1/N of dials and every pinned connection slow, no errors. |

All scenarios share these conditions: one host, 5 control-plane nodes, 3 workers, agents
through the kind envoy balancer, `--sync-interval=5s`, `--sync-forever`,
`--keepalive-time=1h` on both sides, webhook with `failurePolicy: Ignore` and
`timeoutSeconds: 10`.

## Fixes

Each problem the scenarios isolate has a change associated with it. The changes are
ordered by the scenario that measures them and are worked on in that order, each as a
commit on this branch followed by a commit that records, in the scenario document, the
measurements with the change applied next to the measurements without it.

| # | Scenario | Change | Problem it addresses | State |
|---|---|---|---|---|
| 1 | 00 | agent: skip the sync dial when the lease count is satisfied | every agent opens and closes one connection to the VIP per interval forever: a port reservation on a NAT-style VIP, a connection setup per attempt, and noise that hides real events | measured: 36 → 0 flows/min for 3 agents, see [00](scenarios/00-baseline.md#changes-and-their-effect) |
| 2 | 01, 03 | agent: fast re-sync while under-connected | re-mesh takes N·ln(N) × `--sync-interval` | withdrawn: it would spend the same N·ln(N) connections in seconds and reserve that many ports on the VIP at once, for a small gain; see [00](scenarios/00-baseline.md#connection-churn-and-port-exhaustion-at-the-vip) |
| 3 | 02 | `konnectivity-client`: `conn.Close()` returns without waiting for `CLOSE_RSP` | a call into a dead tunnel takes `timeoutSeconds + 10 s` and holds the apiserver's request goroutine | measured: 20 s → 10 s, see [02](scenarios/02-reset-seen-by-agents-only.md#changes-and-their-effect) |
| 4 | 02 | server: `--keepalive-timeout` flag; run with `--keepalive-time=10s --keepalive-timeout=5s` | servers keep dead backends for 20 to 40 s and route dials into them | measured: half-open backends gone in 12 s instead of 28 s idle, dials into them fail in 5 s instead of the caller's timeout; in-band pings, no new flows, see [02](scenarios/02-reset-seen-by-agents-only.md#changes-and-their-effect) |
| 5 | 04 | server: backend selection that compares candidates on recent dial latency or in-flight dials | one slow agent receives its full 1/N share of new dials | planned |

Not changeable here, recorded as conclusions for operators: the balancer resetting its
flows, the agents' CPU request and placement, the webhook's `timeoutSeconds` and
`failurePolicy`, and the server keepalive values behind a balancer or NAT
(see [02](scenarios/02-reset-seen-by-agents-only.md#recommendation)).

## Open items

- Switch the apiserver egress selector to `Direct` and restart the apiservers to measure
  the tunnel cost against a direct path on the same host.

## Cleanup

```sh
scripts/lb-tools.sh stop
kubectl delete validatingwebhookconfiguration knp-webhook
kubectl delete namespace webhook-load
kind delete cluster --name "$CLUSTER"
```
