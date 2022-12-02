/*
Copyright 2022 The Kubernetes Authors.

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

package tests

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

func BenchmarkLargeResponse_GRPC(b *testing.B) {
	b.StopTimer()

	expectCleanShutdown(b)

	ctx := context.Background()
	const length = 1 << 20 // 1M
	const chunks = 10
	server := httptest.NewServer(newSizedServer(length, chunks))
	defer server.Close()

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, cleanup, err := runGRPCProxyServer()
	if err != nil {
		b.Fatal(err)
	}
	defer cleanup()

	clientset := runAgent(proxy.agent, stopCh)
	waitForConnectedServerCount(b, 1, clientset)

	req, err := http.NewRequest("GET", server.URL, nil)
	if err != nil {
		b.Fatal(err)
	}
	req.Close = true

	for n := 0; n < b.N; n++ {
		// run test client
		tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, proxy.front, grpc.WithInsecure())
		if err != nil {
			b.Fatal(err)
		}

		c := &http.Client{
			Transport: &http.Transport{
				DialContext: tunnel.DialContext,
			},
		}

		r, err := c.Do(req)
		if err != nil {
			b.Fatal(err)
		}

		b.StartTimer() // BEGIN CRITICAL SECTION

		size, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			b.Fatal(err)
		}
		r.Body.Close()

		b.StopTimer() // END CRITICAL SECTION

		if size != length*chunks {
			b.Fatalf("expect data length %d; got %d", length*chunks, size)
		}
	}
}

func BenchmarkLargeRequest_GRPC(b *testing.B) {
	b.StopTimer()

	expectCleanShutdown(b)

	const length = 10 << 20 // 10M

	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		size, err := io.Copy(io.Discard, req.Body)
		if err != nil {
			b.Fatal(err)
		}
		if size != length {
			b.Fatalf("Expected data length %d; got %d", length, size)
		}
		req.Body.Close()

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, cleanup, err := runGRPCProxyServer()
	if err != nil {
		b.Fatal(err)
	}
	defer cleanup()

	clientset := runAgent(proxy.agent, stopCh)
	waitForConnectedServerCount(b, 1, clientset)

	bodyBytes := make([]byte, length)
	body := bytes.NewReader(bodyBytes)
	req, err := http.NewRequest("POST", server.URL, body)
	if err != nil {
		b.Fatal(err)
	}
	req.Close = true
	for n := 0; n < b.N; n++ {
		// run test client
		tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, proxy.front, grpc.WithInsecure())
		if err != nil {
			b.Fatal(err)
		}

		c := &http.Client{
			Transport: &http.Transport{
				DialContext: tunnel.DialContext,
			},
		}
		body.Reset(bodyBytes) // We're reusing the request, so make sure to reset the body reader.

		b.StartTimer() // BEGIN CRITICAL SECTION

		if _, err := c.Do(req); err != nil {
			b.Fatal(err)
		}

		b.StopTimer() // END CRITICAL SECTION
	}
}
