/*
Copyright 2019 The Kubernetes Authors.

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
	"fmt"
	"net/url"
	"strconv"
)

const (
	ServerCount      = "serverCount"
	ServerID         = "serverID"
	AgentID          = "agentID"
	AgentIdentifiers = "agentIdentifiers"
	// AuthenticationTokenContextKey will be used as a key to store authentication tokens in grpc call
	// (https://tools.ietf.org/html/rfc6750#section-2.1)
	AuthenticationTokenContextKey = "Authorization"

	// AuthenticationTokenContextSchemePrefix has a prefix for auth token's content.
	// (https://tools.ietf.org/html/rfc6750#section-2.1)
	AuthenticationTokenContextSchemePrefix = "Bearer "

	// UserAgent is used to provide the client information in a proxy request
	UserAgent = "user-agent"
)

// Identifiers stores agent identifiers that will be used by the server when
// choosing agents
type Identifiers struct {
	IPv4         []string
	IPv6         []string
	Host         []string
	CIDR         []string
	DefaultRoute bool
}

type IdentifierType string

const (
	IPv4         IdentifierType = "ipv4"
	IPv6         IdentifierType = "ipv6"
	Host         IdentifierType = "host"
	CIDR         IdentifierType = "cidr"
	UID          IdentifierType = "uid"
	DefaultRoute IdentifierType = "default-route"
)

// GenAgentIdentifiers generates an Identifiers based on the input string, the
// input string should be a comma-seprated list with each item in the format
// of <IdentifierType>=<address>
func GenAgentIdentifiers(addrs string) (Identifiers, error) {
	var agentIDs Identifiers
	decoded, err := url.ParseQuery(addrs)
	if err != nil {
		return agentIDs, fmt.Errorf("fail to parse url encoded string: %v", err)
	}
	for idType, ids := range decoded {
		switch IdentifierType(idType) {
		case IPv4:
			agentIDs.IPv4 = append(agentIDs.IPv4, ids...)
		case IPv6:
			agentIDs.IPv6 = append(agentIDs.IPv6, ids...)
		case Host:
			agentIDs.Host = append(agentIDs.Host, ids...)
		case CIDR:
			agentIDs.CIDR = append(agentIDs.CIDR, ids...)
		case DefaultRoute:
			defaultRouteIdentifier, err := strconv.ParseBool(ids[0])
			if err == nil && defaultRouteIdentifier {
				agentIDs.DefaultRoute = true
			}
		default:
			return agentIDs, fmt.Errorf("Unknown address type: %s", idType)
		}
	}
	return agentIDs, nil
}
