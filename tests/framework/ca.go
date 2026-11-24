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

package framework

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	certutil "k8s.io/client-go/util/cert"
)

// TestCA is a self-signed CA for issuing short-lived test certificates.
type TestCA struct {
	PEM []byte

	certificate *x509.Certificate
	privateKey  *ecdsa.PrivateKey
}

func NewTestCA(t testing.TB, commonName string) *TestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := certutil.NewSelfSignedCACert(certutil.Config{CommonName: commonName}, key)
	if err != nil {
		t.Fatal(err)
	}
	return &TestCA{
		PEM:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}),
		certificate: certificate,
		privateKey:  key,
	}
}

// IssueServing returns a PEM-encoded serving certificate and key for hosts,
// which may be DNS names or IP addresses.
func (ca *TestCA) IssueServing(t testing.TB, commonName string, hosts ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	template := &x509.Certificate{
		Subject:     pkix.Name{CommonName: commonName},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	return ca.issue(t, template)
}

// IssueClient returns a PEM-encoded client certificate and key.
func (ca *TestCA) IssueClient(t testing.TB, commonName string) (certPEM, keyPEM []byte) {
	t.Helper()
	return ca.issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: commonName},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
}

func (ca *TestCA) issue(t testing.TB, template *x509.Certificate) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template.SerialNumber = serial
	template.NotBefore = time.Now().Add(-time.Hour)
	template.NotAfter = time.Now().Add(24 * time.Hour)
	template.KeyUsage = x509.KeyUsageDigitalSignature

	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &key.PublicKey, ca.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
