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
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	fakeauthenticationv1 "k8s.io/client-go/kubernetes/typed/authentication/v1/fake"
	k8stesting "k8s.io/client-go/testing"

	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	metricstest "sigs.k8s.io/apiserver-network-proxy/pkg/testing/metrics"
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

func prepareFrontendConn(ctrl *gomock.Controller) *agentmock.MockAgentService_ConnectServer {
	// prepare the connection to fontend  of proxy-server
	frontendConn := agentmock.NewMockAgentService_ConnectServer(ctrl)
	frontendConnMD := metadata.MD{
		":authority":   []string{"127.0.0.1:8090"},
		"content-type": []string{"application/grpc"},
		"user-agent":   []string{"grpc-go/1.42.0"},
	}
	frontendConnCtx := metadata.NewIncomingContext(context.Background(), frontendConnMD)
	frontendConn.EXPECT().Context().Return(frontendConnCtx).AnyTimes()
	return frontendConn
}

func prepareAgentConnMD(ctrl *gomock.Controller, proxyServer *ProxyServer) *agentmock.MockAgentService_ConnectServer {
	// prepare the the connection to agent of proxy-server
	agentConn := agentmock.NewMockAgentService_ConnectServer(ctrl)
	agentConnMD := metadata.MD{
		":authority":       []string{"127.0.0.1:8091"},
		"agentid":          []string{uuid.New().String()},
		"agentidentifiers": []string{},
		"content-type":     []string{"application/grpc"},
		"user-agent":       []string{"grpc-go/1.42.0"},
	}
	agentConnCtx := metadata.NewIncomingContext(context.Background(), agentConnMD)
	agentConn.EXPECT().Context().Return(agentConnCtx).AnyTimes()

	_ = proxyServer.addBackend(uuid.New().String(), agentConn)
	return agentConn
}

func baseServerProxyTestWithoutBackend(t *testing.T, validate func(*agentmock.MockAgentService_ConnectServer)) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	frontendConn := prepareFrontendConn(ctrl)
	proxyServer := NewProxyServer(uuid.New().String(), []ProxyStrategy{ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{})

	validate(frontendConn)

	proxyServer.Proxy(frontendConn)

	// add a sleep to make sure `serveRecvFrontend` ends after `Proxy` finished.
	time.Sleep(1 * time.Second)
}

func baseServerProxyTestWithBackend(t *testing.T, validate func(*agentmock.MockAgentService_ConnectServer, *agentmock.MockAgentService_ConnectServer)) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	frontendConn := prepareFrontendConn(ctrl)

	// prepare proxy server
	proxyServer := NewProxyServer(uuid.New().String(), []ProxyStrategy{ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{})

	agentConn := prepareAgentConnMD(ctrl, proxyServer)

	validate(frontendConn, agentConn)

	proxyServer.Proxy(frontendConn)

	// add a sleep to make sure `serveRecvFrontend` ends after `Proxy` finished.
	time.Sleep(1 * time.Second)
}

func TestServerProxyNoBackend(t *testing.T) {
	validate := func(frontendConn *agentmock.MockAgentService_ConnectServer) {
		// receive DIAL_REQ from frontend and proxy to backend
		dialReq := &client.Packet{
			Type: client.PacketType_DIAL_REQ,
			Payload: &client.Packet_DialRequest{
				DialRequest: &client.DialRequest{
					Protocol: "tcp",
					Address:  "127.0.0.1:8080",
					Random:   111,
				},
			},
		}

		dialResp := &client.Packet{
			Type: client.PacketType_DIAL_RSP,
			Payload: &client.Packet_DialResponse{
				DialResponse: &client.DialResponse{
					Random: 111,
					Error:  (&ErrNotFound{}).Error(),
				}},
		}

		gomock.InOrder(
			frontendConn.EXPECT().Recv().Return(dialReq, nil).Times(1),
			frontendConn.EXPECT().Recv().Return(nil, io.EOF).Times(1),
			// NOTE(mainred): `Send` should come before `Recv` io.EOF, but we cannot add wait between
			//                two Recvs, thus `Recv`` comes before `Send`
			frontendConn.EXPECT().Send(dialResp).Return(nil).Times(1),
		)

	}
	baseServerProxyTestWithoutBackend(t, validate)

	if err := metricstest.ExpectServerDialFailure(metrics.DialFailureNoAgent, 1); err != nil {
		t.Error(err)
	}
}

func TestServerProxyNormalClose(t *testing.T) {
	validate := func(frontendConn, agentConn *agentmock.MockAgentService_ConnectServer) {
		const connectID = 123456
		// receive DIAL_REQ from frontend and proxy to backend
		dialReq := &client.Packet{
			Type: client.PacketType_DIAL_REQ,
			Payload: &client.Packet_DialRequest{
				DialRequest: &client.DialRequest{
					Protocol: "tcp",
					Address:  "127.0.0.1:8080",
					Random:   111,
				},
			},
		}
		data := &client.Packet{
			Type: client.PacketType_DATA,
			Payload: &client.Packet_Data{
				Data: &client.Data{
					ConnectID: connectID,
				},
			},
		}

		gomock.InOrder(
			frontendConn.EXPECT().Recv().Return(dialReq, nil).Times(1),
			frontendConn.EXPECT().Recv().Return(data, nil).Times(1),
			frontendConn.EXPECT().Recv().Return(closeReqPkt(connectID), nil).Times(1),
			frontendConn.EXPECT().Recv().Return(nil, io.EOF).Times(1),
		)
		gomock.InOrder(
			agentConn.EXPECT().Send(dialReq).Return(nil).Times(1),
			agentConn.EXPECT().Send(data).Return(nil).Times(1),
			agentConn.EXPECT().Send(closeReqPkt(connectID)).Return(nil).Times(1),
			// This extra close is unwanted and should be removed; see
			// https://github.com/kubernetes-sigs/apiserver-network-proxy/pull/307
			agentConn.EXPECT().Send(closeReqPkt(connectID)).Return(nil).Times(1),
		)
	}
	baseServerProxyTestWithBackend(t, validate)
}

func TestServerProxyRecvChanFull(t *testing.T) {
	validate := func(frontendConn, agentConn *agentmock.MockAgentService_ConnectServer) {
		// receive DIAL_REQ from frontend and proxy to backend
		dialReq := &client.Packet{
			Type: client.PacketType_DIAL_REQ,
			Payload: &client.Packet_DialRequest{
				DialRequest: &client.DialRequest{
					Protocol: "tcp",
					Address:  "127.0.0.1:8080",
					Random:   111,
				},
			},
		}

		data := &client.Packet{
			Type: client.PacketType_DATA,
			Payload: &client.Packet_Data{
				Data: &client.Data{
					ConnectID: 1,
					Data:      []byte("hello world"),
				},
			},
		}

		const defaultTimeout = 5 * time.Minute
		deadline := time.Now().Add(defaultTimeout)
		if testDeadline, ok := t.Deadline(); ok && testDeadline.Before(deadline) {
			deadline = testDeadline.Add(-1 * time.Second)
		}

		waitForMetricVal := func(expected float64) {
			err := wait.Poll(10*time.Millisecond, time.Until(deadline), func() (bool, error) {
				val := promtest.ToFloat64(metrics.Metrics.FullRecvChannel(metrics.Proxy))
				return val == expected, nil
			})
			if err != nil {
				t.Fatalf("Failed to observe expected metric: %v", err)
			}
		}

		expectMetricVal := func(expected float64) {
			val := promtest.ToFloat64(metrics.Metrics.FullRecvChannel(metrics.Proxy))
			if val != expected {
				t.Errorf("Unexpected metric value: %v (expected %v)", val, expected)
			}
		}

		// WaitGroups for coordinating test stages.
		recvWG := sync.WaitGroup{}
		recvWG.Add(1)
		sendWG := sync.WaitGroup{}
		sendWG.Add(1)

		gomock.InOrder(
			frontendConn.EXPECT().Recv().Return(dialReq, nil),
			// First packet goes through to agentConn.Send
			frontendConn.EXPECT().Recv().Return(data, nil),
			// Next xfrChannelSize packets fill the channel.
			frontendConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
				// Wait for initial packet send before filling the channel.
				recvWG.Wait()
				return data, nil
			}),
			frontendConn.EXPECT().Recv().Return(data, nil).Times(xfrChannelSize-1),
			// Last packet should trigger channel full condition.
			frontendConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
				// Verify that the full channel condition hasn't triggered yet.
				expectMetricVal(0)
				return data, nil
			}),

			frontendConn.EXPECT().Recv().Return(closeReqPkt(1), nil),
			frontendConn.EXPECT().Recv().Return(nil, io.EOF),
		)
		gomock.InOrder(
			agentConn.EXPECT().Send(dialReq).Return(nil),
			agentConn.EXPECT().Send(data).DoAndReturn(func(_ *client.Packet) error {
				// Channel should not be full at this point.
				expectMetricVal(0)
				recvWG.Done() // Proceed to fill the channel.

				// Block the send from completing until the full channel condition is detected.
				waitForMetricVal(1)
				return nil
			}),
			agentConn.EXPECT().Send(data).Return(nil).Times(xfrChannelSize+1), // Expect the remaining packets to be sent.
			agentConn.EXPECT().Send(closeReqPkt(1)).Return(nil),
			// This extra close is unwanted and should be removed; see
			// https://github.com/kubernetes-sigs/apiserver-network-proxy/pull/307
			agentConn.EXPECT().Send(closeReqPkt(1)).Return(nil),
		)
	}
	baseServerProxyTestWithBackend(t, validate)
}

func TestServerProxyNoDial(t *testing.T) {
	baseServerProxyTestWithBackend(t, func(frontendConn, agentConn *agentmock.MockAgentService_ConnectServer) {
		const connectID = 123456
		data := &client.Packet{
			Type: client.PacketType_DATA,
			Payload: &client.Packet_Data{
				Data: &client.Data{
					ConnectID: connectID,
				},
			},
		}

		gomock.InOrder(
			frontendConn.EXPECT().Recv().Return(data, nil),
			frontendConn.EXPECT().Recv().Return(nil, io.EOF),
		)
		frontendConn.EXPECT().Send(closeRspPkt(connectID, "backend not initialized")).Return(nil)
	})
}

func TestServerProxyConnectionMismatch(t *testing.T) {
	baseServerProxyTestWithBackend(t, func(frontendConn, agentConn *agentmock.MockAgentService_ConnectServer) {
		const firstConnectID = 123456
		const secondConnectID = 654321
		dialReq := &client.Packet{
			Type: client.PacketType_DIAL_REQ,
			Payload: &client.Packet_DialRequest{
				DialRequest: &client.DialRequest{
					Protocol: "tcp",
					Address:  "127.0.0.1:8080",
					Random:   111,
				},
			},
		}
		data := &client.Packet{
			Type: client.PacketType_DATA,
			Payload: &client.Packet_Data{
				Data: &client.Data{
					ConnectID: firstConnectID,
					Data:      []byte("hello"),
				},
			},
		}
		mismatchedData := &client.Packet{
			Type: client.PacketType_DATA,
			Payload: &client.Packet_Data{
				Data: &client.Data{
					ConnectID: secondConnectID,
					Data:      []byte("world"),
				},
			},
		}

		gomock.InOrder(
			frontendConn.EXPECT().Recv().Return(dialReq, nil),
			frontendConn.EXPECT().Recv().Return(data, nil),
			frontendConn.EXPECT().Recv().Return(mismatchedData, nil),
			frontendConn.EXPECT().Recv().Return(nil, io.EOF),
		)
		gomock.InOrder(
			agentConn.EXPECT().Send(dialReq).Return(nil),
			agentConn.EXPECT().Send(data).Return(nil),
		)
		agentConn.EXPECT().Send(closeReqPkt(secondConnectID)).Return(nil)
		agentConn.EXPECT().Send(closeReqPkt(firstConnectID)).Return(nil)
		frontendConn.EXPECT().Send(closeRspPkt(secondConnectID, "mismatched connection IDs")).Return(nil)
		frontendConn.EXPECT().Send(closeRspPkt(firstConnectID, "mismatched connection IDs")).Return(nil)
	})
}

func closeReqPkt(connectID int64) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_CLOSE_REQ,
		Payload: &client.Packet_CloseRequest{
			CloseRequest: &client.CloseRequest{
				ConnectID: connectID,
			}},
	}
}

func closeRspPkt(connectID int64, errMsg string) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_CLOSE_RSP,
		Payload: &client.Packet_CloseResponse{
			CloseResponse: &client.CloseResponse{
				ConnectID: connectID,
				Error:     errMsg,
			},
		},
	}
}
