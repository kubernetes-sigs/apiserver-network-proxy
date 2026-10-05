/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command tunnel-probe sends HTTP requests to an endpoint through a
// konnectivity-server reached over its unix socket, the way kube-apiserver
// reaches a webhook, and logs how long each request takes. Run it on a
// control-plane node. One keep-alive connection is reused across requests,
// so a tunnel that dies under it is observed the way the apiserver observes it.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

func main() {
	uds := flag.String("uds", "/etc/kubernetes/konnectivity-server/konnectivity-server.socket", "konnectivity-server unix socket")
	url := flag.String("url", "", "URL to request through the tunnel (plain HTTP)")
	timeout := flag.Duration("timeout", 10*time.Second, "per-request timeout, like a webhook's timeoutSeconds")
	interval := flag.Duration("interval", 500*time.Millisecond, "pause between requests")
	flag.Parse()
	if *url == "" {
		log.Fatal("-url is required")
	}

	dialer := func(ctx context.Context, _, addr string) (net.Conn, error) {
		tunnel, err := client.CreateSingleUseGrpcTunnel(context.Background(), *uds,
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", *uds)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithUserAgent("tunnel-probe"))
		if err != nil {
			return nil, err
		}
		return tunnel.DialContext(ctx, "tcp", addr)
	}
	httpClient := &http.Client{Transport: &http.Transport{DialContext: dialer, MaxIdleConnsPerHost: 1}}

	for {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
		resp, err := httpClient.Do(req)
		result := "ok"
		if err != nil {
			result = "error: " + err.Error()
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				result = fmt.Sprintf("status %d", resp.StatusCode)
			}
		}
		cancel()
		fmt.Printf("%s %d %s\n", time.Now().UTC().Format("15:04:05.000"), time.Since(start).Milliseconds(), result)
		time.Sleep(*interval)
	}
}
