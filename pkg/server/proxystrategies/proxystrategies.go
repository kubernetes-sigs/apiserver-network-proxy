package proxystrategies

import (
	"fmt"
	"strings"
)

type ProxyStrategy int

const (
	// With this strategy the Proxy Server will randomly pick a backend from
	// the current healthy backends to establish the tunnel over which to
	// forward requests.
	ProxyStrategyDefault ProxyStrategy = iota + 1
	// With this strategy the Proxy Server will pick a backend that has the same
	// associated host as the request.Host to establish the tunnel.
	ProxyStrategyDestHost
	// ProxyStrategyDefaultRoute will only forward traffic to agents that have explicity advertised
	// they serve the default route through an agent identifier. Typically used in combination with destHost
	ProxyStrategyDefaultRoute
)

func (ps ProxyStrategy) String() string {
	switch ps {
	case ProxyStrategyDefault:
		return "default"
	case ProxyStrategyDestHost:
		return "destHost"
	case ProxyStrategyDefaultRoute:
		return "defaultRoute"
	}
	panic(fmt.Sprintf("unhandled ProxyStrategy: %d", ps))
}

func ParseProxyStrategy(s string) (ProxyStrategy, error) {
	switch s {
	case ProxyStrategyDefault.String():
		return ProxyStrategyDefault, nil
	case ProxyStrategyDestHost.String():
		return ProxyStrategyDestHost, nil
	case ProxyStrategyDefaultRoute.String():
		return ProxyStrategyDefaultRoute, nil
	default:
		return 0, fmt.Errorf("unknown proxy strategy: %s", s)
	}
}

// GenProxyStrategiesFromStr generates the list of proxy strategies from the
// comma-seperated string, i.e., destHost.
func ParseProxyStrategies(proxyStrategies string) ([]ProxyStrategy, error) {
	var result []ProxyStrategy

	strs := strings.Split(proxyStrategies, ",")
	for _, s := range strs {
		if len(s) == 0 {
			continue
		}
		ps, err := ParseProxyStrategy(s)
		if err != nil {
			return nil, err
		}
		result = append(result, ps)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("proxy strategies cannot be empty")
	}
	return result, nil
}
