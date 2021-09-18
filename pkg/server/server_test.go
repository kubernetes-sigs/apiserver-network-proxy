/*
Copyright 2019 The Kubernetes Authors.

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
	"io"
	"math/rand"
	"net"
	"reflect"
	"sync"
	"testing"

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"

	"github.com/golang/mock/gomock"
	"google.golang.org/grpc/metadata"

	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	fakeauthenticationv1 "k8s.io/client-go/kubernetes/typed/authentication/v1/fake"
	k8stesting "k8s.io/client-go/testing"

	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
	agentmock "sigs.k8s.io/apiserver-network-proxy/proto/agent/mocks"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

func TestAgentTokenAuthenticationErrorsToken(t *testing.T) {
	stub := gomock.NewController(t)
	defer stub.Finish()

	ns := "test_ns"
	sa := "test_sa"

	testCases := []struct {
		desc               string
		mdKey              string
		tokens             []string
		wantNamespace      string
		wantServiceAccount string
		authenticated      bool
		authError          string
		tokenReviewError   error
		wantError          bool
	}{
		{
			desc:      "no context",
			wantError: true,
		},
		{
			desc:      "non valid metadata key",
			mdKey:     "someKey",
			tokens:    []string{"token1"},
			wantError: true,
		},
		{
			desc:      "non valid token prefix",
			mdKey:     header.AuthenticationTokenContextKey,
			tokens:    []string{"token1"},
			wantError: true,
		},
		{
			desc:      "multiple valid tokens",
			mdKey:     header.AuthenticationTokenContextKey,
			tokens:    []string{header.AuthenticationTokenContextSchemePrefix + "token1", header.AuthenticationTokenContextSchemePrefix + "token2"},
			wantError: true,
		},
		{
			desc:               "not authenticated",
			authenticated:      false,
			mdKey:              header.AuthenticationTokenContextKey,
			tokens:             []string{header.AuthenticationTokenContextSchemePrefix + "token1"},
			wantNamespace:      ns,
			wantServiceAccount: sa,
			wantError:          true,
		},
		{
			desc:               "tokenReview error",
			authenticated:      false,
			mdKey:              header.AuthenticationTokenContextKey,
			tokens:             []string{header.AuthenticationTokenContextSchemePrefix + "token1"},
			tokenReviewError:   fmt.Errorf("some error"),
			wantNamespace:      ns,
			wantServiceAccount: sa,
			wantError:          true,
		},
		{
			desc:               "non valid namespace",
			authenticated:      true,
			mdKey:              header.AuthenticationTokenContextKey,
			tokens:             []string{header.AuthenticationTokenContextSchemePrefix + "token1"},
			wantNamespace:      "_" + ns,
			wantServiceAccount: sa,
			wantError:          true,
		},
		{
			desc:               "non valid service account",
			authenticated:      true,
			mdKey:              header.AuthenticationTokenContextKey,
			tokens:             []string{header.AuthenticationTokenContextSchemePrefix + "token1"},
			wantNamespace:      ns,
			wantServiceAccount: "_" + sa,
			wantError:          true,
		},
		{
			desc:               "authorization succeed",
			authenticated:      true,
			mdKey:              header.AuthenticationTokenContextKey,
			tokens:             []string{header.AuthenticationTokenContextSchemePrefix + "token1"},
			wantNamespace:      ns,
			wantServiceAccount: sa,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			kcs := k8sfake.NewSimpleClientset()

			kcs.AuthenticationV1().(*fakeauthenticationv1.FakeAuthenticationV1).Fake.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
				tr := &authv1.TokenReview{
					Status: authv1.TokenReviewStatus{
						Authenticated: tc.authenticated,
						Error:         tc.authError,
						User: authv1.UserInfo{
							Username: fmt.Sprintf("system:serviceaccount:%v:%v", ns, sa),
						},
					},
				}
				return true, tr, tc.tokenReviewError
			})

			var md metadata.MD
			for _, token := range tc.tokens {
				md = metadata.Join(md, metadata.Pairs(tc.mdKey, token))
			}

			md = metadata.Join(md, metadata.Pairs(header.AgentID, ""))

			ctx := context.Background()
			defer ctx.Done()
			ctx = metadata.NewIncomingContext(ctx, md)
			conn := agentmock.NewMockAgentService_ConnectServer(stub)
			conn.EXPECT().Context().AnyTimes().Return(ctx)

			// close agent's connection if no error is expected
			if !tc.wantError {
				conn.EXPECT().SendHeader(gomock.Any()).Return(nil)
				conn.EXPECT().Recv().Return(nil, io.EOF)
			}

			p := NewProxyServer("", []ProxyStrategy{ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{
				Enabled:             true,
				KubernetesClient:    kcs,
				AgentNamespace:      tc.wantNamespace,
				AgentServiceAccount: tc.wantServiceAccount,
			})

			err := p.Connect(conn)
			if tc.wantError {
				if err == nil {
					t.Errorf("test case expected for error")
				}
			} else {
				if err != nil {
					t.Errorf("did not expected for error but got :%v", err)
				}
			}
		})
	}
}

func TestAddRemoveFrontends(t *testing.T) {
	agent1ConnID1 := new(ProxyClientConnection)
	agent1ConnID2 := new(ProxyClientConnection)
	agent2ConnID1 := new(ProxyClientConnection)
	agent2ConnID2 := new(ProxyClientConnection)
	agent3ConnID1 := new(ProxyClientConnection)

	p := NewProxyServer("", []ProxyStrategy{ProxyStrategyDefault}, 1, nil)
	p.addFrontend("agent1", int64(1), agent1ConnID1)
	p.removeFrontend("agent1", int64(1))
	expectedFrontends := make(map[string]map[int64]*ProxyClientConnection)
	if e, a := expectedFrontends, p.frontends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p = NewProxyServer("", []ProxyStrategy{ProxyStrategyDefault}, 1, nil)
	p.addFrontend("agent1", int64(1), agent1ConnID1)
	p.addFrontend("agent1", int64(2), agent1ConnID2)
	p.addFrontend("agent2", int64(1), agent2ConnID1)
	p.addFrontend("agent2", int64(2), agent2ConnID2)
	p.addFrontend("agent3", int64(1), agent3ConnID1)
	p.removeFrontend("agent2", int64(1))
	p.removeFrontend("agent2", int64(2))
	p.removeFrontend("agent1", int64(1))
	expectedFrontends = map[string]map[int64]*ProxyClientConnection{
		"agent1": {
			int64(2): agent1ConnID2,
		},
		"agent3": {
			int64(1): agent3ConnID1,
		},
	}
	if e, a := expectedFrontends, p.frontends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

type testStream struct {
	agent.AgentService_ConnectServer
	ch chan *client.Packet
}

func (f testStream) SendHeader(md metadata.MD) error {
	return nil
}

func (f testStream) Context() context.Context {
	ctx := context.Background()
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(header.AgentID, "test-agent-1"))
	return ctx
}

func (t testStream) Send(packet *client.Packet) error {
	return nil
}

func (t testStream) Recv() (*client.Packet, error) {
	v, ok := <-t.ch
	if !ok {
		return nil, io.EOF
	}
	return v, nil
}

func TestNodeToControlPlane(t *testing.T) {
	testData := []byte("this is test data")
	stub := gomock.NewController(t)
	defer stub.Finish()
	var wg sync.WaitGroup

	// create proxy server
	p := NewProxyServer("", []ProxyStrategy{ProxyStrategyDefault}, 1,
		&AgentTokenAuthenticationOptions{})

	// start a controlplane server
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		panic(err)
	}

	var receivedData []byte
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := lis.Accept()
		if err != nil {
			panic(err)
		}
		for {
			var data [20]byte
			n, err := conn.Read(data[:])
			if err == io.EOF {
				break
			}
			if err != nil {
				panic(err)
			}
			receivedData = append(receivedData, data[:n]...)
		}
	}()

	// connect the agent to proxy server
	stream := &testStream{ch: make(chan *client.Packet, 15)}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := p.Connect(stream)
		if err != nil {
			panic(err)
		}
	}()

	connID := int64(1)
	stream.ch <- packetDialReq(lis.Addr().String())
	stream.ch <- packetData(connID, testData)
	stream.ch <- packetDataEOF(connID)
	stream.ch <- packetCloseReq(connID)
	close(stream.ch)
	wg.Wait()

	if !reflect.DeepEqual(testData, receivedData) {
		t.Fatalf("Expected: %v Got: %v", testData, receivedData)
	}
}

func packetCloseReq(connID int64) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_CLOSE_REQ,
		Dest: client.Network_ControlPlane,
		Payload: &client.Packet_CloseRequest{
			CloseRequest: &client.CloseRequest{
				ConnectID: connID,
			},
		},
	}
}

func packetData(connID int64, testData []byte) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DATA,
		Dest: client.Network_ControlPlane,
		Payload: &client.Packet_Data{
			Data: &client.Data{
				ConnectID: connID,
				Data:      testData,
			},
		},
	}
}

func packetDialReq(addr string) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DIAL_REQ,
		Dest: client.Network_ControlPlane,
		Payload: &client.Packet_DialRequest{
			DialRequest: &client.DialRequest{
				Protocol: "tcp",
				Address:  addr,
				Random:   rand.Int63(),
			},
		},
	}
}

func packetDataEOF(connID int64) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DATA,
		Dest: client.Network_ControlPlane,
		Payload: &client.Packet_Data{
			Data: &client.Data{
				ConnectID: connID,
				Error:     io.EOF.Error(),
			},
		},
	}
}
