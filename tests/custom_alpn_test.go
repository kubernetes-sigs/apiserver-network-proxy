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
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

func TestCustomALPN(t *testing.T) {
	const proto = "test-proto"
	protoUsed := int32(0)

	svr := httptest.NewUnstartedServer(http.DefaultServeMux)
	svr.TLS = &tls.Config{NextProtos: []string{proto}, MinVersion: tls.VersionTLS13}
	svr.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){
		proto: func(svr *http.Server, conn *tls.Conn, handle http.Handler) {
			atomic.AddInt32(&protoUsed, 1)
		},
	}
	svr.StartTLS()

	ca, err := os.CreateTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	defer os.Remove(ca.Name())

	err = pem.Encode(ca, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: svr.TLS.Certificates[0].Certificate[0],
	})
	if err != nil {
		t.Fatal(err)
	}
	ca.Close()

	tlsConfig, err := util.GetClientTLSConfig(ca.Name(), "", "", "", []string{proto})
	if err != nil {
		t.Fatal(err)
	}

	addr := strings.TrimPrefix(svr.URL, "https://")
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		t.Fatal(err)
	}
	grpcClient := client.NewProxyServiceClient(conn)

	grpcClient.Proxy(context.Background())
	if atomic.LoadInt32(&protoUsed) != 1 {
		t.Error("expected custom ALPN protocol to have been used")
	}
}
