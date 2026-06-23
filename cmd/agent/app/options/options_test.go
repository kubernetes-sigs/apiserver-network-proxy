/*
Copyright 2022 The Kubernetes Authors.

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

package options

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

/*
 * TestDefaultServerOptions is intended to ensure we do not make a backward incompatible
 * change to the default flag values for the ANP agent.
 */
func TestDefaultServerOptions(t *testing.T) {
	defaultAgentOptions := NewGrpcProxyAgentOptions()
	assertDefaultValue(t, "AgentCert", defaultAgentOptions.AgentCert, "")
	assertDefaultValue(t, "AgentKey", defaultAgentOptions.AgentKey, "")
	assertDefaultValue(t, "CaCert", defaultAgentOptions.CaCert, "")
	assertDefaultValue(t, "ProxyServerHost", defaultAgentOptions.ProxyServerHost, "127.0.0.1")
	assertDefaultValue(t, "ProxyServerPort", defaultAgentOptions.ProxyServerPort, 8091)
	assertDefaultValue(t, "HealthServerHost", defaultAgentOptions.HealthServerHost, "")
	assertDefaultValue(t, "HealthServerPort", defaultAgentOptions.HealthServerPort, 8093)
	assertDefaultValue(t, "AdminBindAddress", defaultAgentOptions.AdminBindAddress, "127.0.0.1")
	assertDefaultValue(t, "AdminServerPort", defaultAgentOptions.AdminServerPort, 8094)
	assertDefaultValue(t, "EnableProfiling", defaultAgentOptions.EnableProfiling, false)
	assertDefaultValue(t, "EnableContentionProfiling", defaultAgentOptions.EnableContentionProfiling, false)
	assertDefaultValue(t, "AgentIdentifiers", defaultAgentOptions.AgentIdentifiers, "")
	assertDefaultValue(t, "SyncInterval", defaultAgentOptions.SyncInterval, 1*time.Second)
	assertDefaultValue(t, "ProbeInterval", defaultAgentOptions.ProbeInterval, 1*time.Second)
	assertDefaultValue(t, "SyncIntervalCap", defaultAgentOptions.SyncIntervalCap, 10*time.Second)
	assertDefaultValue(t, "KeepaliveTime", defaultAgentOptions.KeepaliveTime, 1*time.Hour)
	assertDefaultValue(t, "ServiceAccountTokenPath", defaultAgentOptions.ServiceAccountTokenPath, "")
	assertDefaultValue(t, "WarnOnChannelLimit", defaultAgentOptions.WarnOnChannelLimit, false)
	assertDefaultValue(t, "SyncForever", defaultAgentOptions.SyncForever, false)
	assertDefaultValue(t, "XfrChannelSize", defaultAgentOptions.XfrChannelSize, 150)
	assertDefaultValue(t, "CountServerLeases", defaultAgentOptions.CountServerLeases, false)
	assertDefaultValue(t, "LeaseNamespace", defaultAgentOptions.LeaseNamespace, "kube-system")
	assertDefaultValue(t, "LeaseLabelSelector", defaultAgentOptions.LeaseLabelSelector.String(), "k8s-app=konnectivity-server")
	assertDefaultValue(t, "ServerCountSource", defaultAgentOptions.ServerCountSource, "default")
	assertDefaultValue(t, "KubeconfigPath", defaultAgentOptions.KubeconfigPath, "")
	assertDefaultValue(t, "APIContentType", defaultAgentOptions.APIContentType, "application/vnd.kubernetes.protobuf")
}

func assertDefaultValue(t *testing.T, fieldName string, actual, expected interface{}) {
	t.Helper()
	assert.IsType(t, expected, actual, "For field %s, got the wrong type.", fieldName)
	assert.Equal(t, expected, actual, "For field %s, got the wrong value.", fieldName)
}

func TestValidate(t *testing.T) {
	for desc, tc := range map[string]struct {
		fieldMap map[string]any
		expected string
	}{
		"default": {
			fieldMap: map[string]any{},
			expected: "",
		},
		"ZeroProxyServerPort": {
			fieldMap: map[string]any{"proxy-server-port": 0},
			expected: "proxy server port 0 must be greater than 0",
		},
		"NegativeProxyServerPort": {
			fieldMap: map[string]any{"proxy-server-port": -1},
			expected: "proxy server port -1 must be greater than 0",
		},
		"ReservedProxyServerPort": {
			fieldMap: map[string]any{"proxy-server-port": 1023},
			expected: "", //TODO: "please do not try to use reserved port 1023 for the proxy server port",
		},
		"StartValidProxyServerPort": {
			fieldMap: map[string]any{"proxy-server-port": 1024},
			expected: "",
		},
		"EndValidProxyServerPort": {
			fieldMap: map[string]any{"proxy-server-port": 49151},
			expected: "",
		},
		"StartEphemeralProxyServerPort": {
			fieldMap: map[string]any{"proxy-server-port": 49152},
			expected: "", //TODO: "please do not try to use ephemeral port 49152 for the proxy server port",
		},
		"ZeroHealthServerPort": {
			fieldMap: map[string]any{"health-server-port": 0},
			expected: "health server port 0 must be greater than 0",
		},
		"NegativeHealthServerPort": {
			fieldMap: map[string]any{"health-server-port": -1},
			expected: "health server port -1 must be greater than 0",
		},
		"ReservedHealthServerPort": {
			fieldMap: map[string]any{"health-server-port": 1023},
			expected: "", //TODO: "please do not try to use reserved port 1023 for the health server port",
		},
		"StartValidHealthServerPort": {
			fieldMap: map[string]any{"health-server-port": 1024},
			expected: "",
		},
		"EndValidHealthServerPort": {
			fieldMap: map[string]any{"health-server-port": 49151},
			expected: "",
		},
		"StartEphemeralHealthServerPort": {
			fieldMap: map[string]any{"health-server-port": 49152},
			expected: "", //TODO: "please do not try to use ephemeral port 49152 for the health server port",
		},
		"ZeroAdminServerPort": {
			fieldMap: map[string]any{"admin-server-port": 0},
			expected: "admin server port 0 must be greater than 0",
		},
		"NegativeAdminServerPort": {
			fieldMap: map[string]any{"admin-server-port": -1},
			expected: "admin server port -1 must be greater than 0",
		},
		"ReservedAdminServerPort": {
			fieldMap: map[string]any{"admin-server-port": 1023},
			expected: "", //TODO: "please do not try to use reserved port 1023 for the health port",
		},
		"StartValidAdminServerPort": {
			fieldMap: map[string]any{"admin-server-port": 1024},
			expected: "",
		},
		"EndValidAdminServerPort": {
			fieldMap: map[string]any{"admin-server-port": 49151},
			expected: "",
		},
		"StartEphemeralAdminServerPort": {
			fieldMap: map[string]any{"admin-server-port": 49152},
			expected: "", //TODO: "please do not try to use ephemeral port 49152 for the health port",
		},
		"ContentionProfilingRequiresProfiling": {
			fieldMap: map[string]any{
				"enable-contention-profiling": true,
				"enable-profiling":            false,
			},
			expected: "if --enable-contention-profiling is set, --enable-profiling must also be set",
		},
		"ZeroXfrChannelSize": {
			fieldMap: map[string]any{"xfr-channel-size": 0},
			expected: "channel size 0 must be greater than 0",
		},
		"NegativeXfrChannelSize": {
			fieldMap: map[string]any{"xfr-channel-size": -10},
			expected: "channel size -10 must be greater than 0",
		},
		"ServerCountSource": {
			fieldMap: map[string]any{"server-count-source": "foobar"},
			expected: "--server-count-source must be one of '', 'default', 'max', got foobar",
		},
		"LeaseLabelValid": {
			fieldMap: map[string]any{
				"lease-label": "k8s-app=konnectivity-server",
			},
			expected: "",
		},
		"LeaseLabelEmpty": {
			fieldMap: map[string]any{
				"lease-label": "",
			},
			expected: "",
		},
		"LeaseLabelInvalidFormat": {
			fieldMap: map[string]any{
				"lease-label": "*notavalidlabel*",
			},
			expected: `invalid argument "*notavalidlabel*" for "--lease-label" flag`,
		},
		"LeaseLabelMultipleValid": {
			fieldMap: map[string]any{
				"lease-label": "k8s-app=konnectivity-server,component=proxy",
			},
			expected: "",
		},
		"LeaseLabelSelectorValid": {
			fieldMap: map[string]any{
				"lease-label-selector": "k8s-app=konnectivity-server",
			},
			expected: "",
		},
		"LeaseLabelSelectorEmpty": {
			fieldMap: map[string]any{
				"lease-label-selector": "",
			},
			expected: "",
		},
		"LeaseLabelSelectorInvalidFormat": {
			fieldMap: map[string]any{
				"lease-label-selector": "*notavalidlabel*",
			},
			expected: `invalid argument "*notavalidlabel*" for "--lease-label-selector" flag`,
		},
		"LeaseLabelSelectorMultipleValid": {
			fieldMap: map[string]any{
				"lease-label-selector": "k8s-app=konnectivity-server,component=proxy",
			},
			expected: "",
		},
	} {
		t.Run(desc, func(t *testing.T) {
			testAgentOptions := NewGrpcProxyAgentOptions()
			args := make([]string, 0, len(tc.fieldMap))
			for field, value := range tc.fieldMap {
				args = append(args, "--"+field+"="+fmt.Sprint(value))
			}

			actual := testAgentOptions.Flags().Parse(args)
			if actual == nil {
				actual = testAgentOptions.Validate()
			}

			if tc.expected == "" {
				assert.NoError(t, actual)
			} else if assert.Errorf(t, actual, "Expected message: %s", tc.expected) {
				assert.ErrorContains(t, actual, tc.expected)
			}
		})
	}
}
