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
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

type simpleServer struct {
	mu sync.Mutex
}

func (s *simpleServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	time.Sleep(time.Millisecond)

	bytes, err := io.ReadAll(req.Body)
	if err != nil {
		w.Write([]byte(err.Error()))
	}
	w.Write(bytes)
}

// TODO: test http-connect as well.
func getTestClient(front string, t *testing.T) *http.Client {
	ctx := context.Background()
	tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, front, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}

	return &http.Client{
		Transport: &http.Transport{
			DialContext: tunnel.DialContext,
		},
		Timeout: wait.ForeverTestTimeout,
	}
}

func TestConcurrentClientRequest(t *testing.T) {
	const numConcurrentRequests = 100
	s := httptest.NewServer(&simpleServer{})
	defer s.Close()

	ps := runGRPCProxyServerWithServerCount(t, 1)
	defer ps.Stop()

	// Run two agents
	a1 := runAgent(t, ps.AgentAddr())
	a2 := runAgent(t, ps.AgentAddr())
	defer a1.Stop()
	defer a2.Stop()
	waitForConnectedServerCount(t, 1, a1)
	waitForConnectedServerCount(t, 1, a2)

	var wg sync.WaitGroup
	wg.Add(numConcurrentRequests)
	for i := 0; i < numConcurrentRequests; i++ {
		id := i
		go func() {
			defer wg.Done()
			client1 := getTestClient(ps.FrontAddr(), t)

			r, err := client1.Post(s.URL, "text/plain", bytes.NewBufferString(strconv.Itoa(id)))
			if err != nil {
				t.Error(err)
				return
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			r.Body.Close()

			if string(data) != strconv.Itoa(id) {
				t.Errorf("expect %d; got %s", id, string(data))
			}
		}()
	}
	wg.Wait()
}
