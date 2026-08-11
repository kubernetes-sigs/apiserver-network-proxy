//go:build unix

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
	"bytes"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestTLSMaterialReloaderPreservesConfiguredPaths(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "archive")
	generation := filepath.Join(archive, "generation")
	if err := os.MkdirAll(generation, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "current")
	if err := os.Symlink(generation, link); err != nil {
		t.Fatal(err)
	}
	caFile, certFile, keyFile := tlsFilePathsIn(archive)
	ca := newTestCA(t, "ca")
	cert, key := ca.issue(t, "server")
	writeTLSFiles(t, caFile, certFile, keyFile, ca.pem, cert, key)

	// The kernel resolves current before moving up to its parent.
	reloader, err := newTLSMaterialReloader(link+"/../ca.crt", link+"/../tls.crt", link+"/../tls.key", TLSReloadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reloader.current.Load().certificatePEM, cert) {
		t.Fatal("did not load the configured certificate")
	}
	dirs, err := reloader.watchDirectories()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !dirs.Has(want) {
		t.Fatalf("watch directories %v do not include %q", dirs, want)
	}
}

func TestTLSMaterialReloaderRejectsFIFO(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := syscall.Mkfifo(caFile, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	// Nothing writes to the FIFO, so a blocking open would never return.
	go func() {
		_, err := newTLSMaterialReloader(caFile, "", "", TLSReloadHooks{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("loaded TLS material from a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loading a FIFO did not return")
	}
}

func TestServerTLSConfigReloaderRejectsMixedProjectedGenerations(t *testing.T) {
	dir := t.TempDir()
	caFile, certFile, keyFile := tlsFilePathsIn(dir)
	firstCA := newTestCA(t, "first-ca")
	firstCert, firstKey := firstCA.issue(t, "server-one")
	firstClientCert, firstClientKey := firstCA.issue(t, "client-one")
	writeProjectedTLSFiles(t, dir, "generation-1", firstCA.pem, firstCert, firstKey)
	reloader, err := newServerTLSConfigReloader(caFile, certFile, keyFile, nil, nil, tls.VersionTLS12, TLSReloadHooks{})
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := reloader.tlsConfig()
	secondCA := newTestCA(t, "second-ca")
	secondCert, secondKey := secondCA.issue(t, "server-two")
	secondClientCert, secondClientKey := secondCA.issue(t, "client-two")

	read := func(path string) tlsFile {
		t.Helper()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		file := tlsFile{path: path}
		if err := file.readFrom(f); err != nil {
			t.Fatal(err)
		}
		return file
	}
	cert, key := read(certFile), read(keyFile)
	writeProjectedTLSFiles(t, dir, "generation-2", secondCA.pem, secondCert, secondKey)
	if err := reloader.material.publish(cert, key, read(caFile)); !errors.Is(err, errTLSFileChanged) {
		t.Fatalf("publishing files read across a Secret update: got %v, want %v", err, errTLSFileChanged)
	}

	if got, err := serverCertificateCommonName(tlsConfig, firstCA.clientConfig(t, firstClientCert, firstClientKey)); err != nil || got != "server-one" {
		t.Fatalf("last valid identity and trust were not retained: server=%q err=%v", got, err)
	}

	if err := reloader.material.reload(); err != nil {
		t.Fatal(err)
	}
	if got, err := serverCertificateCommonName(tlsConfig, secondCA.clientConfig(t, secondClientCert, secondClientKey)); err != nil || got != "server-two" {
		t.Fatalf("complete Secret generation was not loaded: server=%q err=%v", got, err)
	}
}
