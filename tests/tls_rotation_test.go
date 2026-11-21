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

package tests

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog/v2"

	"sigs.k8s.io/apiserver-network-proxy/pkg/server"
	metricsserver "sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/tests/framework"
)

// The agent and the probe reach the proxy server by address, so its serving
// certificate is identified by an IP SAN rather than a DNS name.
const rotationServerHost = "127.0.0.1"

// rotationTimeout bounds each wait for a reload to take effect. The file watch
// starts a reload at once. The timeout leaves room for the one-minute poll in
// case the watch fails.
const rotationTimeout = 2 * time.Minute

// TestTLSCertificateRotation walks the proxy server and the agent through a
// staged CA rotation (distribute the new CA, swap the identities signed by it,
// retire the old CA) while an established tunnel connection stays open and is
// exercised after every stage.
func TestTLSCertificateRotation(t *testing.T) {
	if runtime.GOOS != "linux" {
		// Every stage would wait for the next poll.
		t.Skip("TLS files are only polled once a minute on this platform")
	}
	expectCleanShutdown(t)
	ctx := context.Background()

	const (
		initialServerName = "anp-rotation-server-initial"
		rotatedServerName = "anp-rotation-server-rotated"
	)

	certsDir := t.TempDir()
	oldCA := framework.NewTestCA(t, "anp-rotation-ca-old")
	newCA := framework.NewTestCA(t, "anp-rotation-ca-new")

	initialServerCert, initialServerKey := oldCA.IssueServing(t, initialServerName, rotationServerHost)
	rotatedServerCert, rotatedServerKey := newCA.IssueServing(t, rotatedServerName, rotationServerHost)
	oldClientCert, oldClientKey := oldCA.IssueClient(t, "anp-rotation-agent-old")
	newClientCert, newClientKey := newCA.IssueClient(t, "anp-rotation-agent-new")

	writeMaterial(t, certsDir, framework.TestCAFile, oldCA.PEM)
	writeMaterial(t, certsDir, framework.TestServerCertFile, initialServerCert)
	writeMaterial(t, certsDir, framework.TestServerKeyFile, initialServerKey)
	writeMaterial(t, certsDir, framework.TestAgentCertFile, oldClientCert)
	writeMaterial(t, certsDir, framework.TestAgentKeyFile, oldClientKey)

	// The agent has to find the restarted server at the end of the test, so the
	// port cannot be left to chance.
	ports, err := framework.FreePorts(1)
	if err != nil {
		t.Fatal(err)
	}
	serverOpts := framework.ProxyServerOpts{
		Mode:                       server.ModeGRPC,
		ServerCount:                1,
		AgentPort:                  ports[0],
		CertsDir:                   certsDir,
		EnableTLSCertificateReload: true,
	}
	ps, err := Framework.ProxyServerRunner.Start(t, serverOpts)
	if err != nil {
		t.Fatalf("Failed to start gRPC proxy server: %v", err)
	}

	a, err := Framework.AgentRunner.Start(t, framework.AgentOpts{
		AgentID:                    uuid.New().String(),
		ServerAddr:                 ps.AgentAddr(),
		CertsDir:                   certsDir,
		EnableTLSCertificateReload: true,
	})
	if err != nil {
		t.Fatalf("Failed to start agent: %v", err)
	}
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	// The first load exports the failure counts, so they show that the server
	// and the agent record their reloads.
	if err := ps.Metrics().ExpectServerTLSReloadFailures(map[string]int{metricsserver.TLSPurposeCluster: 0}); err != nil {
		t.Error(err)
	}
	if err := a.Metrics().ExpectAgentTLSReloadFailures(0); err != nil {
		t.Error(err)
	}

	// Both probes verify the server against either CA, so they keep working
	// across the identity swap and only report on the server's decision about
	// the client certificate they present.
	trustBothCAs := rotationCertPool(t, oldCA.PEM, newCA.PEM)
	oldClient := newRotationProbe(t, ps.AgentAddr(), trustBothCAs, oldClientCert, oldClientKey)
	newClient := newRotationProbe(t, ps.AgentAddr(), trustBothCAs, newClientCert, newClientKey)

	// Baseline: the server accepts the old CA's client and rejects the new CA's.
	if _, err := oldClient.handshake(); err != nil {
		t.Fatalf("A client certificate from the initially trusted CA was rejected: %v", err)
	}
	if _, err := newClient.handshake(); err == nil {
		t.Fatal("A client certificate from a CA the server does not yet trust was accepted")
	}

	backendAddr := startEchoBackend(t)
	tunnel, err := createSingleUseGrpcTunnel(ctx, ps.FrontAddr())
	if err != nil {
		t.Fatalf("Failed to create tunnel: %v", err)
	}
	conn, err := tunnel.DialContext(ctx, "tcp", backendAddr)
	if err != nil {
		t.Fatalf("Failed to dial backend through the tunnel: %v", err)
	}
	assertTunnelAlive(t, conn, "initial")

	// Stage 1: publish the new CA alongside the old one, so that peers holding
	// a certificate from either CA are accepted while the rotation is underway.
	writeMaterial(t, certsDir, framework.TestCAFile, bytes.Join([][]byte{oldCA.PEM, newCA.PEM}, nil))
	waitFor(t, "the proxy server to trust both CAs", func() error {
		if _, err := newClient.handshake(); err != nil {
			return err
		}
		_, err := oldClient.handshake()
		return err
	})
	assertTunnelAlive(t, conn, "after extending the trust bundle")

	// Stage 2: replace the identities. The agent's certificate is rotated here
	// too, but its effect is only visible on a new connection, which the server
	// restart at the end of the test forces.
	writeMaterial(t, certsDir, framework.TestServerCertFile, rotatedServerCert)
	writeMaterial(t, certsDir, framework.TestServerKeyFile, rotatedServerKey)
	writeMaterial(t, certsDir, framework.TestAgentCertFile, newClientCert)
	writeMaterial(t, certsDir, framework.TestAgentKeyFile, newClientKey)
	waitFor(t, "the proxy server to serve its rotated identity", func() error {
		peer, err := newClient.handshake()
		if err != nil {
			return err
		}
		if peer.Subject.CommonName != rotatedServerName {
			return fmt.Errorf("server presented certificate %q, want %q", peer.Subject.CommonName, rotatedServerName)
		}
		return nil
	})
	assertTunnelAlive(t, conn, "after rotating the identities")

	// Stage 3: retire the old CA. Acceptance and rejection are checked together
	// so a server that stopped accepting anything at all cannot pass.
	writeMaterial(t, certsDir, framework.TestCAFile, newCA.PEM)
	waitFor(t, "the proxy server to drop the old CA", func() error {
		if _, err := oldClient.handshake(); err == nil {
			return fmt.Errorf("a client certificate from the retired CA is still accepted")
		}
		if _, err := newClient.handshake(); err != nil {
			return fmt.Errorf("a client certificate from the new CA is rejected: %w", err)
		}
		return nil
	})
	assertTunnelAlive(t, conn, "after retiring the old CA")

	if err := conn.Close(); err != nil {
		t.Errorf("Failed to close the tunnel connection: %v", err)
	}

	// The agent's reload is only observable on a fresh handshake. Restart the
	// proxy server on the same port: it trusts only the new CA, so a successful
	// reconnect proves the agent picked up its rotated certificate.
	ps.Stop()
	waitForConnectedServerCount(t, 0, a)

	ps2, err := Framework.ProxyServerRunner.Start(t, serverOpts)
	if err != nil {
		t.Fatalf("Failed to restart gRPC proxy server: %v", err)
	}
	defer ps2.Stop()
	waitForConnectedServerCount(t, 1, a)
}

// waitFor polls condition until it succeeds, reporting the most recent failure
// if it never does.
func waitFor(t *testing.T, description string, condition func() error) {
	t.Helper()
	var lastErr error
	err := wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, rotationTimeout, true,
		func(context.Context) (bool, error) {
			lastErr = condition()
			return lastErr == nil, nil
		})
	if err != nil {
		t.Fatalf("Timed out waiting for %s: %v", description, lastErr)
	}
}

// assertTunnelAlive sends a message through an already established tunnel
// connection and waits for the echo backend to send it back.
func assertTunnelAlive(t *testing.T, conn net.Conn, stage string) {
	t.Helper()
	sent := []byte(fmt.Sprintf("tunnel is alive %s", stage))
	received := make([]byte, len(sent))

	// Tunnel connections do not implement SetDeadline (it always returns "not
	// implemented"), so the round trip runs on its own goroutine and is bounded
	// by a timer. Closing the connection is what releases that goroutine.
	done := make(chan error, 1)
	go func() {
		if _, err := conn.Write(sent); err != nil {
			done <- fmt.Errorf("write failed: %w", err)
			return
		}
		if _, err := io.ReadFull(conn, received); err != nil {
			done <- fmt.Errorf("read failed: %w", err)
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Tunnel connection broken %s: %v", stage, err)
		}
		if !bytes.Equal(received, sent) {
			t.Fatalf("Tunnel connection returned %q %s, want %q", received, stage, sent)
		}
	case <-time.After(10 * time.Second):
		conn.Close()
		t.Fatalf("Timed out on the tunnel round trip %s", stage)
	}
}

// startEchoBackend runs a TCP echo server for the rest of the test and returns
// the address to dial it on.
func startEchoBackend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				klog.Info(err)
				return
			}
			t.Cleanup(func() { conn.Close() })
			go echo(conn)
		}
	}()
	return ln.Addr().String()
}

// rotationProbe handshakes with the agent-facing listener directly, which is
// the only way to observe the server's current trust bundle and identity
// without disturbing the agent's connection.
type rotationProbe struct {
	addr        string
	roots       *x509.CertPool
	certificate tls.Certificate
}

func newRotationProbe(t *testing.T, addr string, roots *x509.CertPool, certPEM, keyPEM []byte) rotationProbe {
	t.Helper()
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return rotationProbe{addr: addr, roots: roots, certificate: certificate}
}

// handshake returns the certificate the server presented, or the error that
// ended the handshake.
func (p rotationProbe) handshake() (*x509.Certificate, error) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", p.addr, &tls.Config{
		RootCAs:      p.roots,
		Certificates: []tls.Certificate{p.certificate},
		ServerName:   rotationServerHost,
		MinVersion:   tls.VersionTLS12,
		// Pinned to TLS 1.2. Under TLS 1.3 the client certificate is sent after
		// the client already considers the handshake complete, so a server that
		// rejects it fails a later read instead of the handshake, and this probe
		// would quietly stop testing rejection at all.
		MaxVersion: tls.VersionTLS12,
		NextProtos: []string{"h2"}, // The agent-facing listener advertises only h2.
	})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	peers := conn.ConnectionState().PeerCertificates
	if len(peers) == 0 {
		return nil, fmt.Errorf("server presented no certificate")
	}
	return peers[0], nil
}

// writeMaterial replaces one of the files the proxy server and the agent are
// watching. The writes are deliberately not atomic: rotating a certificate and
// its key leaves a window in which the pair does not match, and the reloader is
// expected to keep serving the last valid material until both files are written.
func writeMaterial(t *testing.T, dir, name string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), contents, 0600); err != nil {
		t.Fatal(err)
	}
}

func rotationCertPool(t *testing.T, pemBlocks ...[]byte) *x509.CertPool {
	t.Helper()
	pool, err := certutil.NewPoolFromBytes(bytes.Join(pemBlocks, nil))
	if err != nil {
		t.Fatal(err)
	}
	return pool
}
