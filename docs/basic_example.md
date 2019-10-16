# Basic examples

The current examples run two actual services as well as a sample client on one end and a sample destination for 
requests on the other.
- *Proxy service:* The proxy service takes the API server requests and forwards them appropriately.
- *Agent service:* The agent service connects to the proxy and then allows traffic to be forwarded to it.

## GRPC Client using mTLS Proxy with dial back Agent

```
client =HTTP over GRPC=> (:8090) proxy (:8091) <=GRPC= agent =HTTP=> SimpleHTTPServer(:8000)
  |                                                    ^
  |                          Tunnel                    |
  +----------------------------------------------------+
```

- Start SimpleHTTPServer (Sample destination)
```console
python -m SimpleHTTPServer
```

- Start proxy service
```console
./bin/proxy-server --server-ca-cert=certs/master/issued/ca.crt --server-cert=certs/master/issued/proxy-master.crt --server-key=certs/master/private/proxy-master.key --cluster-ca-cert=certs/agent/issued/ca.crt --cluster-cert=certs/agent/issued/proxy-master.crt --cluster-key=certs/agent/private/proxy-master.key
```

- Start agent service
```console
./bin/proxy-agent --ca-cert=certs/agent/issued/ca.crt --agent-cert=certs/agent/issued/proxy-agent.crt --agent-key=certs/agent/private/proxy-agent.key
```

- Run client (mTLS enabled sample client)
```console
./bin/proxy-test-client --ca-cert=certs/master/issued/ca.crt --client-cert=certs/master/issued/proxy-client.crt --client-key=certs/master/private/proxy-client.key
```

## HTTP-Connect Client using mTLS Proxy with dial back Agent (Either curl OR test client)

```
client =HTTP-CONNECT=> (:8090) proxy (:8091) <=GRPC= agent =HTTP=> SimpleHTTPServer(:8000)
  |                                                    ^
  |                          Tunnel                    |
  +----------------------------------------------------+
```

- Start SimpleHTTPServer (Sample destination)
```console
python -m SimpleHTTPServer
```

- Start proxy service
```console
./bin/proxy-server --mode=http-connect --server-ca-cert=certs/master/issued/ca.crt --server-cert=certs/master/issued/proxy-master.crt --server-key=certs/master/private/proxy-master.key --cluster-ca-cert=certs/agent/issued/ca.crt --cluster-cert=certs/agent/issued/proxy-master.crt --cluster-key=certs/agent/private/proxy-master.key
```

- Start agent service
```console
./bin/proxy-agent --ca-cert=certs/agent/issued/ca.crt --agent-cert=certs/agent/issued/proxy-agent.crt --agent-key=certs/agent/private/proxy-agent.key
```

- Run client (mTLS & http-connect enabled sample client)
```console
./bin/proxy-test-client --mode=http-connect  --proxy-host=127.0.0.1 --ca-cert=certs/master/issued/ca.crt --client-cert=certs/master/issued/proxy-client.crt --client-key=certs/master/private/proxy-client.key
```

- Run curl client (curl using a mTLS http-connect proxy)
```console
curl -v -p --proxy-key certs/master/private/proxy-client.key --proxy-cert certs/master/issued/proxy-client.crt --proxy-cacert certs/master/issued/ca.crt --proxy-cert-type PEM -x https://127.0.0.1:8090  http://localhost:8000```
```

