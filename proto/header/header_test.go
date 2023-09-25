/*
Copyright 2023 The Kubernetes Authors.

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

package header

import (
	"reflect"
	"testing"
)

func TestGenAgentIdentifiers(t *testing.T) {
	testCases := []struct {
		desc   string
		idents string

		want    Identifiers
		wantErr bool
	}{
		{
			desc:    "invalid url encoding",
			idents:  ";",
			wantErr: true,
		},
		{
			desc:   "invalid identifier type",
			idents: "invalid=1.2.3.4",
			want:   Identifiers{},
		},
		{
			desc:   "ipv4",
			idents: "ipv4=1.2.3.4",
			want: Identifiers{
				IPv4: []string{"1.2.3.4"},
			},
		},
		{
			desc:   "ipv6",
			idents: "ipv6=100::",
			want: Identifiers{
				IPv6: []string{"100::"},
			},
		},
		{
			desc:   "host",
			idents: "host=node1.mydomain.com",
			want: Identifiers{
				Host: []string{"node1.mydomain.com"},
			},
		},
		{
			desc:   "cidr",
			idents: "cidr=127.0.0.1/16",
			want: Identifiers{
				CIDR: []string{"127.0.0.1/16"},
			},
		},
		{
			desc:   "default route true",
			idents: "default-route=true",
			want: Identifiers{
				DefaultRoute: true,
			},
		},
		{
			desc:   "default route false",
			idents: "default-route=false",
			want:   Identifiers{},
		},
		{
			desc:   "default route invalid",
			idents: "default-route=invalid",
			want:   Identifiers{},
		},
		{
			desc: "multiple default route",
			// The first entry is used.
			idents: "default-route=true&default-route=false",
			want: Identifiers{
				DefaultRoute: true,
			},
		},
		{
			desc:   "success with multiple",
			idents: "host=localhost&host=node1.mydomain.com&cidr=10.0.0.0/8&cidr=100::/64&ipv4=1.2.3.4&ipv4=5.6.7.8&ipv6=100::&ipv6=100::1&default-route=true",
			want: Identifiers{
				IPv4:         []string{"1.2.3.4", "5.6.7.8"},
				IPv6:         []string{"100::", "100::1"},
				Host:         []string{"localhost", "node1.mydomain.com"},
				CIDR:         []string{"10.0.0.0/8", "100::/64"},
				DefaultRoute: true,
			},
		},
		{
			// UID is passed via header AgentID, not AgentIdentifers.
			desc:   "ignore IdentifierType UID",
			idents: "uid=value",
			want:   Identifiers{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := GenAgentIdentifiers(tc.idents)
			if (err != nil) != tc.wantErr {
				t.Errorf("GenAgentIdentifiers got err %q; wantErr = %t", err, tc.wantErr)
			}
			if !reflect.DeepEqual(tc.want, got) {
				t.Errorf("expected %v, got %v", tc.want, got)
			}
		})
	}
}
