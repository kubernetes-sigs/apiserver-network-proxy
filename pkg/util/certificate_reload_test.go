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

package util

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/apimachinery/pkg/util/wait"
	certutil "k8s.io/client-go/util/cert"
)

func TestReloadingClientTLSCredentialsRotatesCertificateAndCA(t *testing.T) {
	caFile, certFile, keyFile := tlsFilePaths(t)

	firstCA := newTestCA(t, "first-ca")
	firstServerCert, firstServerKey := firstCA.issue(t, "proxy.test")
	firstClientCert, firstClientKey := firstCA.issue(t, "client-one")
	writeTLSFiles(t, caFile, certFile, keyFile, firstCA.pem, firstClientCert, firstClientKey)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reloading, err := GetReloadingClientTLSCredentials(ctx, caFile, certFile, keyFile, "proxy.test", []string{"h2"}, TLSReloadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	clientReloader := reloading.(*reloadingClientTLSCredentials)
	if got, err := clientCertificateCommonName(reloading, firstCA.serverConfig(t, firstServerCert, firstServerKey)); err != nil || got != "client-one" {
		t.Fatalf("initial handshake client = %q, err=%v", got, err)
	}

	secondClientCert, secondClientKey := firstCA.issue(t, "client-two")
	writeTLSFiles(t, caFile, certFile, keyFile, firstCA.pem, secondClientCert, secondClientKey)
	if err := clientReloader.material.reload(); err != nil {
		t.Fatal(err)
	}
	if got, err := clientCertificateCommonName(reloading, firstCA.serverConfig(t, firstServerCert, firstServerKey)); err != nil || got != "client-two" {
		t.Fatalf("client certificate rotation = %q, err=%v", got, err)
	}

	secondCA := newTestCA(t, "second-ca")
	secondServerCert, secondServerKey := secondCA.issue(t, "proxy.test")
	thirdClientCert, thirdClientKey := secondCA.issue(t, "client-three")
	writeTLSFiles(t, caFile, certFile, keyFile, secondCA.pem, thirdClientCert, thirdClientKey)
	if err := clientReloader.material.reload(); err != nil {
		t.Fatal(err)
	}
	if got, err := clientCertificateCommonName(reloading, secondCA.serverConfig(t, secondServerCert, secondServerKey)); err != nil || got != "client-three" {
		t.Fatalf("CA rotation = %q, err=%v", got, err)
	}

	if err := os.WriteFile(certFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clientReloader.material.reload(); err == nil {
		t.Fatal("malformed rotated certificate unexpectedly reloaded")
	}
	if got, err := clientCertificateCommonName(reloading, secondCA.serverConfig(t, secondServerCert, secondServerKey)); err != nil || got != "client-three" {
		t.Fatalf("last valid client identity was not retained: client=%q err=%v", got, err)
	}
	writeTLSFiles(t, caFile, certFile, keyFile, secondCA.pem, thirdClientCert, thirdClientKey)
	if err := clientReloader.material.reload(); err != nil {
		t.Fatal(err)
	}
	if got, err := clientCertificateCommonName(reloading, secondCA.serverConfig(t, secondServerCert, secondServerKey)); err != nil || got != "client-three" {
		t.Fatalf("handshake did not recover after repairing projected files: client=%q err=%v", got, err)
	}
}

func TestReloadingClientTLSCredentialsPreservesCAOnlyAuthority(t *testing.T) {
	caFile, _, _ := tlsFilePaths(t)
	ca := newTestCA(t, "ca")
	if err := os.WriteFile(caFile, ca.pem, 0o600); err != nil {
		t.Fatal(err)
	}

	creds, err := GetReloadingClientTLSCredentials(t.Context(), caFile, "", "", "proxy.test", nil, TLSReloadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	// CA-only credentials leave authority selection to gRPC, including the port.
	conn, err := grpc.NewClient("proxy.test:8091", grpc.WithTransportCredentials(creds), grpc.WithAuthority("proxy.test:8091"))
	if err != nil {
		t.Fatalf("CA-only credentials rejected the dial target authority: %v", err)
	}
	conn.Close()
}

func TestReloadingClientTLSCredentialsCloneUsesRotatedMaterial(t *testing.T) {
	caFile, certFile, keyFile := tlsFilePaths(t)

	firstCA := newTestCA(t, "first-ca")
	firstServerCert, firstServerKey := firstCA.issue(t, "proxy.test")
	firstClientCert, firstClientKey := firstCA.issue(t, "client-one")
	writeTLSFiles(t, caFile, certFile, keyFile, firstCA.pem, firstClientCert, firstClientKey)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reloading, err := GetReloadingClientTLSCredentials(ctx, caFile, certFile, keyFile, "proxy.test", []string{"h2"}, TLSReloadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	clientReloader := reloading.(*reloadingClientTLSCredentials)
	cloned := reloading.Clone()

	if got, err := clientCertificateCommonName(cloned, firstCA.serverConfig(t, firstServerCert, firstServerKey)); err != nil || got != "client-one" {
		t.Fatalf("initial cloned handshake client = %q, err=%v", got, err)
	}

	secondCA := newTestCA(t, "second-ca")
	secondServerCert, secondServerKey := secondCA.issue(t, "proxy.test")
	secondClientCert, secondClientKey := secondCA.issue(t, "client-two")
	writeTLSFiles(t, caFile, certFile, keyFile, secondCA.pem, secondClientCert, secondClientKey)
	if err := clientReloader.material.reload(); err != nil {
		t.Fatal(err)
	}

	if got, err := clientCertificateCommonName(cloned, secondCA.serverConfig(t, secondServerCert, secondServerKey)); err != nil || got != "client-two" {
		t.Fatalf("rotated cloned handshake client = %q, err=%v", got, err)
	}
}

func TestTLSFileRejectsStream(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := writer.Write(newTestCA(t, "streamed-ca").pem); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	file := tlsFile{path: reader.Name()}
	if err := file.readFrom(reader); err == nil {
		t.Fatal("read TLS material from a stream")
	}
}

func TestServerTLSConfigReloaderSessionResumption(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			caFile, certFile, keyFile := tlsFilePaths(t)
			servingCA := newTestCA(t, "serving-ca")
			clientCA := newTestCA(t, "client-ca")
			cert, key := servingCA.issue(t, "server")
			writeTLSFiles(t, caFile, certFile, keyFile, clientCA.pem, cert, key)
			reloader, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nil, nil, version, TLSReloadHooks{})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			server.TLS = reloader.tlsConfig()
			server.StartTLS()
			defer server.Close()

			request := func(config *tls.Config) (*tls.ConnectionState, error) {
				config = config.Clone()
				config.MaxVersion = version
				transport := &http.Transport{TLSClientConfig: config, DisableKeepAlives: true}
				defer transport.CloseIdleConnections()
				client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
				resp, err := client.Get(server.URL)
				if err != nil {
					return nil, err
				}
				defer resp.Body.Close()
				return resp.TLS, nil
			}
			oldCert, oldKey := clientCA.issue(t, "old-client")
			oldClient := servingCA.clientConfig(t, oldCert, oldKey)
			oldClient.ClientSessionCache = tls.NewLRUClientSessionCache(1)
			check := func(client *tls.Config, resumed bool, name string) {
				t.Helper()
				state, err := request(client)
				if err != nil {
					t.Fatal(err)
				}
				if state.DidResume != resumed || state.PeerCertificates[0].Subject.CommonName != name {
					t.Fatalf("DidResume = %v, server = %q, want %v, %q", state.DidResume, state.PeerCertificates[0].Subject.CommonName, resumed, name)
				}
			}
			check(oldClient, false, "server")
			cert, key = servingCA.issue(t, "rotated-server")
			writeTLSFiles(t, caFile, certFile, keyFile, clientCA.pem, cert, key)
			if err := reloader.material.reload(); err != nil {
				t.Fatal(err)
			}
			check(oldClient, true, "server")
			check(servingCA.clientConfig(t, oldCert, oldKey), false, "rotated-server")

			if err := os.WriteFile(caFile, servingCA.pem, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := reloader.material.reload(); err != nil {
				t.Fatal(err)
			}
			if _, err := request(oldClient); err == nil {
				t.Fatal("server accepted the client from the retired CA")
			}
			newCert, newKey := servingCA.issue(t, "new-client")
			if _, err := request(servingCA.clientConfig(t, newCert, newKey)); err != nil {
				t.Fatalf("client from the current CA: %v", err)
			}
		})
	}
}

func TestServerTLSConfigReloaderKeepsLastValidMaterialOnPartialUpdate(t *testing.T) {
	caFile, certFile, keyFile := tlsFilePaths(t)

	firstCA := newTestCA(t, "first-ca")
	firstServerCert, firstServerKey := firstCA.issue(t, "server-one")
	firstClientCert, firstClientKey := firstCA.issue(t, "client-one")
	writeTLSFiles(t, caFile, certFile, keyFile, firstCA.pem, firstServerCert, firstServerKey)

	reloader, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nil, nil, tls.VersionTLS12, TLSReloadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := reloader.tlsConfig()

	// Write the new CA and certificate but not the key, as a non-atomic update
	// would. Nothing may be published from the half-written state.
	secondCA := newTestCA(t, "second-ca")
	secondServerCert, secondServerKey := secondCA.issue(t, "server-two")
	secondClientCert, secondClientKey := secondCA.issue(t, "client-two")
	if err := os.WriteFile(caFile, secondCA.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, secondServerCert, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloader.material.reload(); err == nil {
		t.Fatal("mismatched certificate and key unexpectedly reloaded")
	}
	if got, err := serverCertificateCommonName(tlsConfig, firstCA.clientConfig(t, firstClientCert, firstClientKey)); err != nil || got != "server-one" {
		t.Fatalf("last valid identity and trust were not retained: server=%q err=%v", got, err)
	}
	if _, err := serverCertificateCommonName(tlsConfig, firstCA.clientConfig(t, secondClientCert, secondClientKey)); err == nil {
		t.Fatal("server accepted a client from the half-written trust bundle")
	}

	// Once the key is written, the whole update is applied together.
	if err := os.WriteFile(keyFile, secondServerKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloader.material.reload(); err != nil {
		t.Fatal(err)
	}
	if got, err := serverCertificateCommonName(tlsConfig, secondCA.clientConfig(t, secondClientCert, secondClientKey)); err != nil || got != "server-two" {
		t.Fatalf("update was not applied after the key was written: server=%q err=%v", got, err)
	}
}

func TestServerTLSConfigReloaderRejectsMalformedCertificateChain(t *testing.T) {
	caFile, certFile, keyFile := tlsFilePaths(t)
	firstCA := newTestCA(t, "first-ca")
	firstServerCert, firstServerKey := firstCA.issue(t, "server-one")
	firstClientCert, firstClientKey := firstCA.issue(t, "client-one")
	writeTLSFiles(t, caFile, certFile, keyFile, firstCA.pem, firstServerCert, firstServerKey)

	reloader, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nil, nil, tls.VersionTLS12, TLSReloadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := reloader.tlsConfig()

	secondCA := newTestCA(t, "second-ca")
	secondServerCert, secondServerKey := secondCA.issue(t, "server-two")
	secondClientCert, secondClientKey := secondCA.issue(t, "client-two")
	invalidChain := append(append([]byte(nil), secondServerCert...), malformedCertificatePEM()...)
	writeTLSFiles(t, caFile, certFile, keyFile, secondCA.pem, invalidChain, secondServerKey)
	if err := reloader.material.reload(); err == nil {
		t.Fatal("malformed certificate chain unexpectedly reloaded")
	}
	if got, err := serverCertificateCommonName(tlsConfig, firstCA.clientConfig(t, firstClientCert, firstClientKey)); err != nil || got != "server-one" {
		t.Fatalf("last valid identity and trust were not retained: server=%q err=%v", got, err)
	}
	if _, err := serverCertificateCommonName(tlsConfig, firstCA.clientConfig(t, secondClientCert, secondClientKey)); err == nil {
		t.Fatal("server accepted a client from the rejected update")
	}

	validChain := append(append([]byte(nil), secondServerCert...), secondCA.pem...)
	if err := os.WriteFile(certFile, validChain, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloader.material.reload(); err != nil {
		t.Fatal(err)
	}
	if got, err := serverCertificateCommonName(tlsConfig, secondCA.clientConfig(t, secondClientCert, secondClientKey)); err != nil || got != "server-two" {
		t.Fatalf("update was not applied after repairing the chain: server=%q err=%v", got, err)
	}
}

func TestServerTLSConfigReloaderRejectsPartiallyInvalidTrustBundle(t *testing.T) {
	caFile, certFile, keyFile := tlsFilePaths(t)

	firstCA := newTestCA(t, "first-ca")
	firstServerCert, firstServerKey := firstCA.issue(t, "server-one")
	firstClientCert, firstClientKey := firstCA.issue(t, "client-one")
	writeTLSFiles(t, caFile, certFile, keyFile, firstCA.pem, firstServerCert, firstServerKey)

	reloader, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nil, nil, tls.VersionTLS12, TLSReloadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := reloader.tlsConfig()

	secondCA := newTestCA(t, "second-ca")
	secondClientCert, secondClientKey := secondCA.issue(t, "client-two")
	invalidCandidate := append(append([]byte(nil), secondCA.pem...), malformedCertificatePEM()...)
	if err := os.WriteFile(caFile, invalidCandidate, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloader.material.reload(); err == nil {
		t.Fatal("partially invalid trust bundle unexpectedly reloaded")
	}
	if got, err := serverCertificateCommonName(tlsConfig, firstCA.clientConfig(t, firstClientCert, firstClientKey)); err != nil || got != "server-one" {
		t.Fatalf("last valid server trust was not retained: server=%q err=%v", got, err)
	}
	if _, err := serverCertificateCommonName(tlsConfig, firstCA.clientConfig(t, secondClientCert, secondClientKey)); err == nil {
		t.Fatal("server accepted a client from the partially loaded CA bundle")
	}

	if err := os.WriteFile(caFile, secondCA.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloader.material.reload(); err != nil {
		t.Fatal(err)
	}
	if got, err := serverCertificateCommonName(tlsConfig, firstCA.clientConfig(t, secondClientCert, secondClientKey)); err != nil || got != "server-one" {
		t.Fatalf("server trust did not recover after repairing the bundle: server=%q err=%v", got, err)
	}
}

func TestReloadingServerTLSConfigWatchesProjectedFiles(t *testing.T) {
	if !watchTLSFiles {
		t.Skip("TLS files are polled on this platform")
	}
	dir := t.TempDir()
	caFile, certFile, keyFile := tlsFilePathsIn(dir)

	firstCA := newTestCA(t, "first-ca")
	firstServerCert, firstServerKey := firstCA.issue(t, "server-one")
	firstClientCert, firstClientKey := firstCA.issue(t, "client-one")
	writeProjectedTLSFiles(t, dir, "generation-1", firstCA.pem, firstServerCert, firstServerKey)

	var failures atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tlsConfig, err := GetReloadingServerTLSConfig(ctx, caFile, certFile, keyFile, []string{"h2"}, nil, tls.VersionTLS12, TLSReloadHooks{
		OnFailure: func() { failures.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := serverCertificateCommonName(tlsConfig, firstCA.clientConfig(t, firstClientCert, firstClientKey)); err != nil || got != "server-one" {
		t.Fatalf("initial handshake server = %q, err=%v", got, err)
	}

	secondCA := newTestCA(t, "second-ca")
	secondClientCert, secondClientKey := secondCA.issue(t, "client-two")
	clientConfig := secondCA.clientConfig(t, secondClientCert, secondClientKey)
	// Repeat the rotation: a reload often overlaps the ..data swap, and that
	// must not be reported as a failure.
	for generation := 2; generation <= 100; generation++ {
		commonName := fmt.Sprintf("server-%d", generation)
		serverCert, serverKey := secondCA.issue(t, commonName)
		writeProjectedTLSFiles(t, dir, fmt.Sprintf("generation-%d", generation), secondCA.pem, serverCert, serverKey)
		if err := waitForServerCertificate(tlsConfig, clientConfig, commonName); err != nil {
			t.Fatal(err)
		}
	}
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d reloads failed during projected rotations", n)
	}
}

func TestTLSMaterialReloaderRenewsWatches(t *testing.T) {
	if !watchTLSFiles {
		t.Skip("TLS files are polled on this platform")
	}
	r := &tlsMaterialReloader{
		caFile:         filepath.Join(t.TempDir(), "ca.crt"),
		reloadInterval: time.Millisecond,
		reloadCh:       make(chan struct{}, 1),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go r.watch(ctx)
	// Nothing changes the directory, so only a new registration requests a reload.
	for range 3 {
		select {
		case <-r.reloadCh:
		case <-ctx.Done():
			t.Fatal("watch did not register the directory again")
		}
	}
}

func TestServerTLSConfigReloaderRejectsEmptyConfiguredClientCA(t *testing.T) {
	t.Run("initial load", func(t *testing.T) {
		caFile, certFile, keyFile := tlsFilePaths(t)

		ca := newTestCA(t, "ca")
		serverCert, serverKey := ca.issue(t, "server")
		writeTLSFiles(t, caFile, certFile, keyFile, nil, serverCert, serverKey)

		if _, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nil, nil, tls.VersionTLS12, TLSReloadHooks{}); err == nil {
			t.Fatal("empty configured client CA unexpectedly loaded")
		}
	})

	t.Run("reload", func(t *testing.T) {
		caFile, certFile, keyFile := tlsFilePaths(t)

		ca := newTestCA(t, "ca")
		serverCert, serverKey := ca.issue(t, "server")
		clientCert, clientKey := ca.issue(t, "client")
		writeTLSFiles(t, caFile, certFile, keyFile, ca.pem, serverCert, serverKey)

		reloader, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nil, nil, tls.VersionTLS12, TLSReloadHooks{})
		if err != nil {
			t.Fatal(err)
		}
		tlsConfig := reloader.tlsConfig()

		if err := os.WriteFile(caFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := reloader.material.reload(); err == nil {
			t.Fatal("empty configured client CA unexpectedly reloaded")
		}
		if reloader.currentTLSConfig().ClientAuth != tls.RequireAndVerifyClientCert {
			t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", reloader.currentTLSConfig().ClientAuth)
		}
		if got, err := serverCertificateCommonName(tlsConfig, ca.clientConfig(t, clientCert, clientKey)); err != nil || got != "server" {
			t.Fatalf("last valid configuration was not retained: server=%q err=%v", got, err)
		}
	})
}

func TestServerTLSConfigReloaderRetriesPartialUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		caFile, certFile, keyFile := tlsFilePaths(t)
		ca := newTestCA(t, "ca")
		firstCert, firstKey := ca.issue(t, "server-one")
		writeTLSFiles(t, caFile, certFile, keyFile, ca.pem, firstCert, firstKey)

		successes := make(chan struct{}, 1)
		failures := make(chan struct{}, 1)
		reloader, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nil, nil, tls.VersionTLS12, TLSReloadHooks{
			OnSuccess: func() { successes <- struct{}{} },
			OnFailure: func() { failures <- struct{}{} },
		})
		if err != nil {
			t.Fatal(err)
		}
		<-successes // Initial load.
		initial := reloader.material.current.Load()

		secondCert, secondKey := ca.issue(t, "server-two")
		if err := os.WriteFile(certFile, secondCert, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		reloader.material.reloadInterval = time.Hour
		// Run without a watcher so only the retry can observe the repaired key.
		go reloader.material.run(ctx)
		reloader.material.requestReload()
		select {
		case <-failures:
		case <-ctx.Done():
			t.Fatal("partial update did not invoke OnFailure")
		}
		if reloader.material.current.Load() != initial {
			t.Fatal("partial update replaced the last valid material")
		}
		reloader.material.requestReload()
		synctest.Wait()
		select {
		case <-failures:
			t.Fatal("file notification triggered a retry during backoff")
		default:
		}

		if err := os.WriteFile(keyFile, secondKey, 0o600); err != nil {
			t.Fatal(err)
		}
		select {
		case <-successes:
		case <-ctx.Done():
			t.Fatal("retry did not invoke OnSuccess after repairing the key")
		}
		clientCert, clientKey := ca.issue(t, "client")
		if got, err := serverCertificateCommonName(reloader.tlsConfig(), ca.clientConfig(t, clientCert, clientKey)); err != nil || got != "server-two" {
			t.Fatalf("retry did not publish the repaired material: server=%q err=%v", got, err)
		}
	})
}

func TestServerTLSConfigReloaderReportsMissingFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		caFile, certFile, keyFile := tlsFilePaths(t)
		ca := newTestCA(t, "ca")
		cert, key := ca.issue(t, "server")
		writeTLSFiles(t, caFile, certFile, keyFile, ca.pem, cert, key)

		failures := make(chan struct{}, 1)
		reloader, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nil, nil, tls.VersionTLS12, TLSReloadHooks{
			OnFailure: func() {
				select {
				case failures <- struct{}{}:
				default:
				}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(keyFile); err != nil {
			t.Fatal(err)
		}
		// A rotation can remove a file for a moment, so a missing file first
		// counts as a change.
		if err := reloader.material.reload(); !errors.Is(err, errTLSFileChanged) {
			t.Fatalf("reload with a missing key = %v, want %v", err, errTLSFileChanged)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		reloader.material.reloadInterval = time.Hour
		go reloader.material.run(ctx)
		reloader.material.requestReload()
		// The retry reports the file that is still missing.
		select {
		case <-failures:
		case <-ctx.Done():
			t.Fatal("a missing key was not reported as a failure")
		}
	})
}

func TestServerTLSConfigReloaderPollingFallback(t *testing.T) {
	caFile, certFile, keyFile := tlsFilePaths(t)

	firstCA := newTestCA(t, "first-ca")
	firstServerCert, firstServerKey := firstCA.issue(t, "server-one")
	writeTLSFiles(t, caFile, certFile, keyFile, firstCA.pem, firstServerCert, firstServerKey)

	reloader, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nil, nil, tls.VersionTLS12, TLSReloadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reloader.material.reloadInterval = 10 * time.Millisecond
	// Without the file watcher, only polling can reload.
	go reloader.material.run(ctx)

	secondCA := newTestCA(t, "second-ca")
	secondServerCert, secondServerKey := secondCA.issue(t, "server-two")
	secondClientCert, secondClientKey := secondCA.issue(t, "client-two")
	writeTLSFiles(t, caFile, certFile, keyFile, secondCA.pem, secondServerCert, secondServerKey)
	if err := waitForServerCertificate(reloader.tlsConfig(), secondCA.clientConfig(t, secondClientCert, secondClientKey), "server-two"); err != nil {
		t.Fatal(err)
	}
}

func TestServerTLSConfigWithoutClientCA(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")

	ca := newTestCA(t, "ca")
	serverCert, serverKey := ca.issue(t, "server")
	clientCert, clientKey := ca.issue(t, "client")
	writeTLSFiles(t, filepath.Join(dir, "unused-ca.crt"), certFile, keyFile, ca.pem, serverCert, serverKey)

	reloader, err := newServerTLSConfigReloader("", certFile, keyFile, nil, nil, tls.VersionTLS12, TLSReloadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if reloader.currentTLSConfig().ClientAuth != tls.NoClientCert {
		t.Fatalf("ClientAuth = %v, want NoClientCert", reloader.currentTLSConfig().ClientAuth)
	}
	if got, err := serverCertificateCommonName(reloader.tlsConfig(), ca.clientConfig(t, clientCert, clientKey)); err != nil || got != "server" {
		t.Fatalf("server without client CA = %q, err=%v", got, err)
	}
}

func TestServerTLSConfigReloaderPreservesListenerSettings(t *testing.T) {
	caFile, certFile, keyFile := tlsFilePaths(t)

	ca := newTestCA(t, "ca")
	serverCert, serverKey := ca.issue(t, "server")
	writeTLSFiles(t, caFile, certFile, keyFile, ca.pem, serverCert, serverKey)

	nextProtos := []string{"h2", "custom"}
	cipherSuites := []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}
	reloader, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nextProtos, cipherSuites, tls.VersionTLS13, TLSReloadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := reloader.tlsConfig()

	configForClient, err := tlsConfig.GetConfigForClient(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(configForClient.NextProtos, nextProtos) {
		t.Fatalf("NextProtos = %v, want %v", configForClient.NextProtos, nextProtos)
	}
	if !slices.Equal(configForClient.CipherSuites, cipherSuites) {
		t.Fatalf("CipherSuites = %v, want %v", configForClient.CipherSuites, cipherSuites)
	}
	if configForClient.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %v, want %v", configForClient.MinVersion, tls.VersionTLS13)
	}
}

func clientCertificateCommonName(clientCredentials credentials.TransportCredentials, serverConfig *tls.Config) (string, error) {
	clientRaw, serverRaw := net.Pipe()
	type serverResult struct {
		commonName string
		err        error
	}
	serverDone := make(chan serverResult, 1)
	go func() {
		server := tls.Server(serverRaw, serverConfig)
		defer server.Close()
		if err := server.Handshake(); err != nil {
			serverDone <- serverResult{err: err}
			return
		}
		serverDone <- serverResult{commonName: server.ConnectionState().PeerCertificates[0].Subject.CommonName}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, clientErr := clientCredentials.ClientHandshake(ctx, "proxy.test", clientRaw)
	_ = clientRaw.Close()
	result := <-serverDone
	if clientErr != nil {
		return "", clientErr
	}
	return result.commonName, result.err
}

func serverCertificateCommonName(serverConfig, clientConfig *tls.Config) (string, error) {
	clientRaw, serverRaw := net.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		server := tls.Server(serverRaw, serverConfig)
		defer serverRaw.Close()
		serverDone <- server.Handshake()
	}()

	client := tls.Client(clientRaw, clientConfig)
	defer clientRaw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientErr := client.HandshakeContext(ctx)
	_ = clientRaw.Close()
	serverErr := <-serverDone
	if clientErr != nil {
		return "", clientErr
	}
	if serverErr != nil {
		return "", serverErr
	}
	return client.ConnectionState().PeerCertificates[0].Subject.CommonName, nil
}

func waitForServerCertificate(serverConfig, clientConfig *tls.Config, commonName string) error {
	var lastErr error
	err := wait.PollUntilContextTimeout(context.Background(), 20*time.Millisecond, 5*time.Second, true, func(context.Context) (bool, error) {
		got, err := serverCertificateCommonName(serverConfig, clientConfig)
		switch {
		case err != nil:
			lastErr = err
		case got != commonName:
			lastErr = fmt.Errorf("server certificate common name = %q, want %q", got, commonName)
		default:
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("server TLS configuration did not reload: %v", lastErr)
	}
	return nil
}

// tlsFilePaths returns the CA, certificate and key paths inside a fresh
// temporary directory.
func tlsFilePaths(t *testing.T) (caFile, certFile, keyFile string) {
	t.Helper()
	return tlsFilePathsIn(t.TempDir())
}

func tlsFilePathsIn(dir string) (caFile, certFile, keyFile string) {
	return filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
}

func writeTLSFiles(t *testing.T, caFile, certFile, keyFile string, caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	files := []struct {
		path string
		data []byte
	}{
		{path: caFile, data: caPEM},
		{path: certFile, data: certPEM},
		{path: keyFile, data: keyPEM},
	}
	for _, file := range files {
		if err := os.WriteFile(file.path, file.data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeProjectedTLSFiles mirrors how kubelet projects a Secret volume: files
// are symlinks through a ..data link that is swapped atomically per generation.
func writeProjectedTLSFiles(t *testing.T, dir, generation string, caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	generationDir := filepath.Join(dir, generation)
	if err := os.Mkdir(generationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTLSFiles(
		t,
		filepath.Join(generationDir, "ca.crt"),
		filepath.Join(generationDir, "tls.crt"),
		filepath.Join(generationDir, "tls.key"),
		caPEM,
		certPEM,
		keyPEM,
	)

	dataLink := filepath.Join(dir, "..data")
	if _, err := os.Lstat(dataLink); os.IsNotExist(err) {
		if err := os.Symlink(generation, dataLink); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"ca.crt", "tls.crt", "tls.key"} {
			if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
		return
	} else if err != nil {
		t.Fatal(err)
	}

	previousGeneration, err := os.Readlink(dataLink)
	if err != nil {
		t.Fatal(err)
	}
	temporaryLink := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(generation, temporaryLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporaryLink, dataLink); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, previousGeneration)); err != nil {
		t.Fatal(err)
	}
}

type testCA struct {
	certificate *x509.Certificate
	privateKey  *ecdsa.PrivateKey
	pem         []byte
}

func newTestCA(t *testing.T, commonName string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := certutil.NewSelfSignedCACert(certutil.Config{CommonName: commonName}, key)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{
		certificate: certificate,
		privateKey:  key,
		pem:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}),
	}
}

// issue returns a PEM-encoded leaf certificate and key usable both as a client
// and as a serving certificate for "proxy.test".
func (ca testCA) issue(t *testing.T, commonName string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     []string{"proxy.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &key.PublicKey, ca.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func (ca testCA) serverConfig(t *testing.T, certPEM, keyPEM []byte) *tls.Config {
	t.Helper()
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs, err := certutil.NewPoolFromBytes(ca.pem)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		NextProtos:   []string{"h2"},
	}
}

func (ca testCA) clientConfig(t *testing.T, certPEM, keyPEM []byte) *tls.Config {
	t.Helper()
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	rootCAs, err := certutil.NewPoolFromBytes(ca.pem)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      rootCAs,
		Certificates: []tls.Certificate{certificate},
		ServerName:   "proxy.test",
	}
}
