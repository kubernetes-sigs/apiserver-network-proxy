/*
Copyright 2020 The Kubernetes Authors.

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

package server

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc/metadata"

	agentmock "sigs.k8s.io/apiserver-network-proxy/proto/agent/mocks"
)

func mockAgentConn(ctrl *gomock.Controller, agentID string, agentIdentifiers []string) *agentmock.MockAgentService_ConnectServer {
	agentConn := agentmock.NewMockAgentService_ConnectServer(ctrl)
	agentConnMD := metadata.MD{
		":authority":       []string{"127.0.0.1:8091"},
		"agentid":          []string{agentID},
		"agentidentifiers": agentIdentifiers,
		"content-type":     []string{"application/grpc"},
		"user-agent":       []string{"grpc-go/1.42.0"},
	}
	agentConnCtx := metadata.NewIncomingContext(context.Background(), agentConnMD)
	agentConn.EXPECT().Context().Return(agentConnCtx).AnyTimes()
	return agentConn
}

func TestBackendManagerAddRemoveBackends(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{}))

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDefault})

	p.AddBackend(backend1)

	input := "127.0.0.1"
	if got, _ := p.Backend(input); got != backend1 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend1)
	}
	p.AddBackend(backend2)
	p.AddBackend(backend3)
	p.RemoveBackend(backend1)
	p.RemoveBackend(backend2)

	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}

	p.RemoveBackend(backend3)

	if got, _ := p.Backend(input); got != nil {
		t.Errorf("Backend(%v) = %v, want nil", input, got)
	}
}

func TestBackendManagerAddRemoveBackends_DefaultRouteStrategy(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"default-route=false"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"default-route=true"}))

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDefaultRoute})

	p.AddBackend(backend1)
	p.AddBackend(backend2)

	input := "127.0.0.1"
	if got, _ := p.Backend(input); got != nil {
		t.Errorf("Backend(%v) = %v, want nil", input, got)
	}

	p.AddBackend(backend3)

	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}

	p.RemoveBackend(backend3)

	if got, _ := p.Backend(input); got != nil {
		t.Errorf("Backend(%v) = %v, want nil", input, got)
	}
}

func TestBackendManagerAddRemoveBackends_DestHostStrategy(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"host=localhost&host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"default-route=true"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"host=node2.mydomain.com&ipv4=5.6.7.8&ipv6=::"}))

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDestHost})

	p.AddBackend(backend1)
	p.AddBackend(backend2)
	p.AddBackend(backend3)

	input := "127.0.0.1"
	if got, _ := p.Backend(input); got != nil {
		t.Errorf("Backend(%v) = %v, want nil", input, got)
	}
	input = "localhost"
	if got, _ := p.Backend(input); got != backend1 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend1)
	}
	input = "node1.mydomain.com"
	if got, _ := p.Backend(input); got != backend1 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend1)
	}
	input = "1.2.3.4"
	if got, _ := p.Backend(input); got != backend1 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend1)
	}
	input = "9878::7675:1292:9183:7562"
	if got, _ := p.Backend(input); got != backend1 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend1)
	}
	input = "node2.mydomain.com"
	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}
	input = "5.6.7.8"
	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}
	input = "::"
	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}

	p.RemoveBackend(backend1)
	p.RemoveBackend(backend2)
	p.RemoveBackend(backend3)

	input = "127.0.0.1"
	if got, _ := p.Backend(input); got != nil {
		t.Errorf("Backend(%v) = %v, want nil", input, got)
	}
}

func TestBackendManagerAddRemoveBackends_DestHostWithDefault(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"host=localhost&host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"default-route=false"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"host=node2.mydomain.com&ipv4=5.6.7.8&ipv6=::"}))

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDestHost, ProxyStrategyDefault})

	p.AddBackend(backend1)
	p.AddBackend(backend2)
	p.AddBackend(backend3)

	input := "127.0.0.1"
	if got, _ := p.Backend(input); got == nil {
		t.Errorf("expected random fallback, got nil")
	}
	input = "localhost"
	if got, _ := p.Backend(input); got != backend1 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend1)
	}
	input = "node1.mydomain.com"
	if got, _ := p.Backend(input); got != backend1 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend1)
	}
	input = "1.2.3.4"
	if got, _ := p.Backend(input); got != backend1 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend1)
	}
	input = "9878::7675:1292:9183:7562"
	if got, _ := p.Backend(input); got != backend1 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend1)
	}
	input = "node2.mydomain.com"
	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}
	input = "5.6.7.8"
	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}
	input = "::"
	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}

	p.RemoveBackend(backend1)
	p.RemoveBackend(backend2)

	input = "127.0.0.1"
	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}

	p.RemoveBackend(backend3)

	input = "127.0.0.1"
	if got, _ := p.Backend(input); got != nil {
		t.Errorf("Backend(%v) = %v, want nil", input, got)
	}
}

func TestBackendManagerAddRemoveBackends_DuplicateIdents(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"default-route=true"}))

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDestHost, ProxyStrategyDefaultRoute})

	p.AddBackend(backend1)
	p.AddBackend(backend2)
	p.AddBackend(backend3)

	input := "node1.mydomain.com"
	if got, _ := p.Backend(input); got == nil || got == backend3 {
		t.Errorf("Backend(%v) = %v, want any other backend", got, backend3)
	}
	input = "1.2.3.4"
	if got, _ := p.Backend(input); got == nil || got == backend3 {
		t.Errorf("Backend(%v) = %v, want any other backend", got, backend3)
	}
	input = "9878::7675:1292:9183:7562"
	if got, _ := p.Backend(input); got == nil || got == backend3 {
		t.Errorf("Backend(%v) = %v, want any other backend", got, backend3)
	}
	input = "node2.mydomain.com"
	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}
	input = "5.6.7.8"
	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}
	input = "::"
	if got, _ := p.Backend(input); got != backend3 {
		t.Errorf("Backend(%v) = %v, want %v", input, got, backend3)
	}

	p.RemoveBackend(backend1)
	p.RemoveBackend(backend2)
	p.RemoveBackend(backend3)

	input = "127.0.0.1"
	if got, _ := p.Backend(input); got != nil {
		t.Errorf("Backend(%v) = %v, want nil", input, got)
	}
}

func TestBackendManagerNumBackends(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{""}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"default-route=true"}))

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDestHost, ProxyStrategyDefaultRoute, ProxyStrategyDefault})

	p.AddBackend(backend1)
	p.AddBackend(backend2)
	p.AddBackend(backend3)
	if got := p.NumBackends(); got != 3 {
		t.Errorf("NumBackends() = %v, want 3", got)
	}
	wantReady := true
	wantMsg := ""
	ready, msg := p.Ready()
	if ready != wantReady || msg != wantMsg {
		t.Errorf("Ready() = %t / %q, want %t / %q", ready, msg, wantReady, wantMsg)
	}

	p.RemoveBackend(backend1)
	p.RemoveBackend(backend2)
	p.RemoveBackend(backend3)

	if got := p.NumBackends(); got != 0 {
		t.Errorf("NumBackends() = %v, want 0", got)
	}
	wantReady = false
	wantMsg = "no connection to any proxy agent"
	ready, msg = p.Ready()
	if ready != wantReady || msg != wantMsg {
		t.Errorf("Ready() = %t / %q, want %t / %q", ready, msg, wantReady, wantMsg)
	}
}

func TestProxyStrategy(t *testing.T) {
	for desc, tc := range map[string]struct {
		input     ProxyStrategy
		want      string
		wantPanic string
	}{
		"default": {
			input: ProxyStrategyDefault,
			want:  "default",
		},
		"destHost": {
			input: ProxyStrategyDestHost,
			want:  "destHost",
		},
		"defaultRoute": {
			input: ProxyStrategyDefaultRoute,
			want:  "defaultRoute",
		},
		"unrecognized": {
			input:     ProxyStrategy(0),
			wantPanic: "unhandled ProxyStrategy: 0",
		},
	} {
		t.Run(desc, func(t *testing.T) {
			if tc.wantPanic != "" {
				assert.PanicsWithValue(t, tc.wantPanic, func() {
					_ = tc.input.String()
				})
			} else {
				got := tc.input.String()
				if got != tc.want {
					t.Errorf("ProxyStrategy.String() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestParseProxyStrategy(t *testing.T) {
	for desc, tc := range map[string]struct {
		input   string
		want    ProxyStrategy
		wantErr error
	}{
		"empty": {
			input:   "",
			wantErr: fmt.Errorf("unknown proxy strategy: "),
		},
		"unrecognized": {
			input:   "unrecognized",
			wantErr: fmt.Errorf("unknown proxy strategy: unrecognized"),
		},
		"default": {
			input: "default",
			want:  ProxyStrategyDefault,
		},
		"destHost": {
			input: "destHost",
			want:  ProxyStrategyDestHost,
		},
		"defaultRoute": {
			input: "defaultRoute",
			want:  ProxyStrategyDefaultRoute,
		},
	} {
		t.Run(desc, func(t *testing.T) {
			got, err := ParseProxyStrategy(tc.input)
			assert.Equal(t, err, tc.wantErr, "ParseProxyStrategy(%s) = error %q, want %v", tc.input, err, tc.wantErr)
			if got != tc.want {
				t.Errorf("ParseProxyStrategy(%s) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseProxyStrategies(t *testing.T) {
	for desc, tc := range map[string]struct {
		input   string
		want    []ProxyStrategy
		wantErr error
	}{
		"empty": {
			input:   "",
			wantErr: fmt.Errorf("proxy strategies cannot be empty"),
		},
		"unrecognized": {
			input:   "unrecognized",
			wantErr: fmt.Errorf("unknown proxy strategy: unrecognized"),
		},
		"default": {
			input: "default",
			want:  []ProxyStrategy{ProxyStrategyDefault},
		},
		"destHost": {
			input: "destHost",
			want:  []ProxyStrategy{ProxyStrategyDestHost},
		},
		"defaultRoute": {
			input: "defaultRoute",
			want:  []ProxyStrategy{ProxyStrategyDefaultRoute},
		},
		"duplicate": {
			input: "destHost,defaultRoute,defaultRoute,default",
			want:  []ProxyStrategy{ProxyStrategyDestHost, ProxyStrategyDefaultRoute, ProxyStrategyDefaultRoute, ProxyStrategyDefault},
		},
		"multiple": {
			input: "destHost,defaultRoute,default",
			want:  []ProxyStrategy{ProxyStrategyDestHost, ProxyStrategyDefaultRoute, ProxyStrategyDefault},
		},
	} {
		t.Run(desc, func(t *testing.T) {
			got, err := ParseProxyStrategies(tc.input)
			assert.Equal(t, err, tc.wantErr, "ParseProxyStrategies(%s) = error %q, want %v", tc.input, err, tc.wantErr)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseProxyStrategies(%s) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
