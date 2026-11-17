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

package app

import (
	"context"
	"path/filepath"
	"testing"

	"sigs.k8s.io/apiserver-network-proxy/cmd/server/app/options"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server"
)

func TestRunUDSFrontendServerListenError(t *testing.T) {
	for _, mode := range []string{server.ModeGRPC, server.ModeHTTPConnect} {
		t.Run(mode, func(t *testing.T) {
			o := options.NewProxyRunOptions()
			o.Mode = mode
			o.UdsName = filepath.Join(t.TempDir(), "missing", "s.sock")

			p := &Proxy{}
			stop, err := p.runUDSFrontendServer(context.Background(), o, &server.ProxyServer{})
			if err == nil {
				stop(context.Background())
				t.Fatal("expected error for unlistenable UDS path")
			}
		})
	}
}
