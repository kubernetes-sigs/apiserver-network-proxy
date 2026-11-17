/*
Copyright 2023 The Kubernetes Authors.

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

package framework

import (
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"time"
)

var ForeverTestTimeout = time.Minute

func checkReadiness(addr string) bool {
	resp, err := http.Get(fmt.Sprintf("http://%s/readyz", addr))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func checkLiveness(addr string) bool {
	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// FreePorts finds [count] available ports. It picks ports below the OS
// ephemeral ranges (32768+ on Linux, 49152+ on macOS) because the server
// options reject ephemeral ports.
func FreePorts(count int) ([]int, error) {
	const minPort, maxPort = 10000, 32768
	ports := make([]int, 0, count)
	var lastErr error
	for attempts := 0; len(ports) < count; attempts++ {
		if attempts >= 100 {
			return nil, fmt.Errorf("failed to reserve %d ports: %w", count, lastErr)
		}
		p := minPort + rand.IntN(maxPort-minPort)
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			lastErr = err
			continue
		}
		defer l.Close()
		ports = append(ports, p)
	}
	return ports, nil
}
