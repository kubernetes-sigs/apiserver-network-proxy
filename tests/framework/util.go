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
	"net"
	"net/http"
	"strconv"
)

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

// FreePorts finds [count] available ports.
func FreePorts(count int) ([]int, error) {
	ports := make([]int, count)
	for i := 0; i < count; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("failed to reserve ports: %w", err)
		}
		defer l.Close()
		_, p, err := net.SplitHostPort(l.Addr().String())
		if err != nil {
			return nil, fmt.Errorf("failed to reserve ports: %w", err)
		}
		ports[i], err = strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("failed to reserve ports: %w", err)
		}
	}
	return ports, nil
}
