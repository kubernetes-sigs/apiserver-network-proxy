/*
Copyright 2020 The Kubernetes Authors.

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
	"crypto/tls"
	"testing"
)

const (
	failed  = "\u2717"
	succeed = "\u2713"
)

func TestRemovePortFromHost(t *testing.T) {
	tests := []struct {
		name     string
		origHost string
		expect   string
	}{
		{"Domain&Port", "localhost:8080", "localhost"},
		{"Domain", "localhost", "localhost"},
		{"IPv4&Port", "192.168.0.1:8080", "192.168.0.1"},
		{"ShortestIPv6", "::", "::"},
		{"IPv6", "9878::7675:1292:9183:7562", "9878::7675:1292:9183:7562"},
		{"IPv6&Port", "[alsk:1204:1020::1292]:8080", "alsk:1204:1020::1292"},
		{"FQDN", " www.kubernetes.test:8080", " www.kubernetes.test"},
	}

	for _, tt := range tests {
		st := tt
		tf := func(t *testing.T) {
			t.Parallel()
			t.Logf("\tTestCase: %s", st.name)
			{
				get := RemovePortFromHost(st.origHost)
				if get != st.expect {
					t.Fatalf("\t%s\texpect %v, but get %v", failed, st.expect, get)
				}
				t.Logf("\t%s\texpect %v, get %v", succeed, st.expect, get)

			}
		}
		t.Run(st.name, tf)
	}
}

func TestGetTLSVersion(t *testing.T) {
	tests := []struct {
		name        string
		versionName string
		expected    uint16
		expectError bool
	}{
		{"EmptyDefaultsToTLS12", "", tls.VersionTLS12, false},
		{"VersionTLS10", "VersionTLS10", tls.VersionTLS10, false},
		{"VersionTLS11", "VersionTLS11", tls.VersionTLS11, false},
		{"VersionTLS12", "VersionTLS12", tls.VersionTLS12, false},
		{"VersionTLS13", "VersionTLS13", tls.VersionTLS13, false},
		{"InvalidVersion", "VersionTLS99", 0, true},
	}

	for _, tt := range tests {
		st := tt
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			got, err := GetTLSVersion(st.versionName)
			if st.expectError {
				if err == nil {
					t.Fatalf("\t%s\texpected error for input %q, but got none", failed, st.versionName)
				}
				t.Logf("\t%s\tgot expected error: %v", succeed, err)
				return
			}
			if err != nil {
				t.Fatalf("\t%s\tunexpected error for input %q: %v", failed, st.versionName, err)
			}
			if got != st.expected {
				t.Fatalf("\t%s\texpect %v, but got %v", failed, st.expected, got)
			}
			t.Logf("\t%s\texpect %v, got %v", succeed, st.expected, got)
		})
	}
}
