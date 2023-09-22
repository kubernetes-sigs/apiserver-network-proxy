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
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

func echo(conn net.Conn) {
	var data [256]byte

	for {
		n, err := conn.Read(data[:])
		if err != nil {
			klog.Info(err)
			return
		}

		_, err = conn.Write(data[:n])
		if err != nil {
			klog.Info(err)
			return
		}
	}
}

func TestEchoServer(t *testing.T) {
	ctx := context.Background()
	ln, err := net.Listen("tcp", "")
	if err != nil {
		t.Error(err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				klog.Info(err)
				break
			}
			go echo(conn)
		}
	}()

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	// run test client
	tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, ps.FrontAddr(), grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}

	conn, err := tunnel.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Error(err)
	}

	msg := "1234567890123456789012345"
	n, err := conn.Write([]byte(msg))
	if err != nil {
		t.Error(err)
	}
	if n != len(msg) {
		t.Errorf("expect write %d; got %d", len(msg), n)
	}

	var data [10]byte

	n, err = conn.Read(data[:])
	if err != nil {
		t.Error(err)
	}
	if string(data[:n]) != msg[:10] {
		t.Errorf("expect %s; got %s", msg[:10], string(data[:n]))
	}

	n, err = conn.Read(data[:])
	if err != nil {
		t.Error(err)
	}
	if string(data[:n]) != msg[10:20] {
		t.Errorf("expect %s; got %s", msg[10:20], string(data[:n]))
	}

	msg2 := "1234567"
	n, err = conn.Write([]byte(msg2))
	if err != nil {
		t.Error(err)
	}
	if n != len(msg2) {
		t.Errorf("expect write %d; got %d", len(msg2), n)
	}

	n, err = conn.Read(data[:])
	if err != nil {
		t.Error(err)
	}
	if string(data[:n]) != msg[20:] {
		t.Errorf("expect %s; got %s", msg[20:], string(data[:n]))
	}

	n, err = conn.Read(data[:])
	if err != nil {
		t.Error(err)
	}
	if string(data[:n]) != msg2 {
		t.Errorf("expect %s; got %s", msg, string(data[:n]))
	}

	if err := conn.Close(); err != nil {
		t.Error(err)
	}
}
