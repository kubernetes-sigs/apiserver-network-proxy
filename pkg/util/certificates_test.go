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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	certutil "k8s.io/client-go/util/cert"
)

func TestGetClientTLSConfigCABundleValidation(t *testing.T) {
	caPEM := newTestCAPEM(t)
	nonCertificateBlock := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a private key")})

	testCases := []struct {
		name    string
		bundle  []byte
		wantErr bool
	}{
		{name: "valid bundle", bundle: caPEM},
		{name: "non-certificate block is ignored", bundle: append(append([]byte(nil), caPEM...), nonCertificateBlock...)},
		{name: "malformed certificate block is rejected", bundle: append(append([]byte(nil), caPEM...), malformedCertificatePEM()...), wantErr: true},
		// Data that never forms a valid PEM block is skipped rather than
		// rejected, the same boundary as kube-apiserver's dynamic CA reload.
		{name: "truncated trailing block is skipped", bundle: append(append([]byte(nil), caPEM...), []byte("-----BEGIN CERTIFICATE-----\nMIIBtruncated\n")...)},
		{name: "block with corrupted base64 is skipped", bundle: append(append([]byte(nil), caPEM...), []byte("-----BEGIN CERTIFICATE-----\n!!!!\n-----END CERTIFICATE-----\n")...)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			caFile := filepath.Join(t.TempDir(), "ca.crt")
			if err := os.WriteFile(caFile, tc.bundle, 0o600); err != nil {
				t.Fatal(err)
			}
			tlsConfig, err := GetClientTLSConfig(caFile, "", "", "proxy.test", nil)
			if tc.wantErr {
				if err == nil {
					t.Fatal("partially invalid CA bundle unexpectedly loaded")
				}
				if !strings.Contains(err.Error(), caFile) {
					t.Fatalf("error %q does not identify the CA file %q", err, caFile)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tlsConfig.RootCAs == nil {
				t.Fatal("tlsConfig has no RootCAs")
			}
		})
	}
}

func newTestCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := certutil.NewSelfSignedCACert(certutil.Config{CommonName: "ca"}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
}

func malformedCertificatePEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a DER-encoded certificate")})
}
