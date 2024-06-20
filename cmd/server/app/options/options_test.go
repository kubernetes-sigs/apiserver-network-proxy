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
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

/*
 * TestDefaultServerOptions is intended to ensure we do not make a backward incompatible
 * change to the default flag values for the ANP server.
 */
func TestDefaultServerOptions(t *testing.T) {
	defaultServerOptions := NewProxyRunOptions()
	assertDefaultValue(t, "ServerCert", defaultServerOptions.ServerCert, "")
	assertDefaultValue(t, "ServerKey", defaultServerOptions.ServerKey, "")
	assertDefaultValue(t, "ServerCaCert", defaultServerOptions.ServerCaCert, "")
	assertDefaultValue(t, "ClusterCert", defaultServerOptions.ClusterCert, "")
	assertDefaultValue(t, "ClusterKey", defaultServerOptions.ClusterKey, "")
	assertDefaultValue(t, "ClusterCaCert", defaultServerOptions.ClusterCaCert, "")
	assertDefaultValue(t, "Mode", defaultServerOptions.Mode, "grpc")
	assertDefaultValue(t, "UdsName", defaultServerOptions.UdsName, "")
	assertDefaultValue(t, "DeleteUDSFile", defaultServerOptions.DeleteUDSFile, true)
	assertDefaultValue(t, "ServerPort", defaultServerOptions.ServerPort, 8090)
	assertDefaultValue(t, "ServerBindAddress", defaultServerOptions.ServerBindAddress, "")
	assertDefaultValue(t, "AgentPort", defaultServerOptions.AgentPort, 8091)
	assertDefaultValue(t, "AgentBindAddress", defaultServerOptions.AgentBindAddress, "")
	assertDefaultValue(t, "HealthPort", defaultServerOptions.HealthPort, 8092)
	assertDefaultValue(t, "HealthBindAddress", defaultServerOptions.HealthBindAddress, "")
	assertDefaultValue(t, "AdminPort", defaultServerOptions.AdminPort, 8095)
	assertDefaultValue(t, "AdminBindAddress", defaultServerOptions.AdminBindAddress, "127.0.0.1")
	assertDefaultValue(t, "KeepaliveTime", defaultServerOptions.KeepaliveTime, 1*time.Hour)
	assertDefaultValue(t, "FrontendKeepaliveTime", defaultServerOptions.FrontendKeepaliveTime, 1*time.Hour)
	assertDefaultValue(t, "EnableProfiling", defaultServerOptions.EnableProfiling, false)
	assertDefaultValue(t, "EnableContentionProfiling", defaultServerOptions.EnableContentionProfiling, false)
	assertDefaultValue(t, "ServerCount", defaultServerOptions.ServerCount, uint(1))
	assertDefaultValue(t, "AgentNamespace", defaultServerOptions.AgentNamespace, "")
	assertDefaultValue(t, "AgentServiceAccount", defaultServerOptions.AgentServiceAccount, "")
	assertDefaultValue(t, "KubeconfigPath", defaultServerOptions.KubeconfigPath, "")
	assertDefaultValue(t, "KubeconfigQPS", defaultServerOptions.KubeconfigQPS, float32(0))
	assertDefaultValue(t, "KubeconfigBurst", defaultServerOptions.KubeconfigBurst, 0)
	assertDefaultValue(t, "AuthenticationAudience", defaultServerOptions.AuthenticationAudience, "")
	assertDefaultValue(t, "ProxyStrategies", defaultServerOptions.ProxyStrategies, "default")
	assertDefaultValue(t, "CipherSuites", defaultServerOptions.CipherSuites, make([]string, 0))
	assertDefaultValue(t, "XfrChannelSize", defaultServerOptions.XfrChannelSize, 10)

}

func assertDefaultValue(t *testing.T, fieldName string, actual, expected interface{}) {
	t.Helper()
	assert.IsType(t, expected, actual, "For field %s, got the wrong type.", fieldName)
	assert.Equal(t, expected, actual, "For field %s, got the wrong value.", fieldName)
}

func TestValidate(t *testing.T) {
	for desc, tc := range map[string]struct {
		field    string
		value    interface{}
		expected error
	}{
		"default": {
			field:    "",
			value:    nil,
			expected: nil,
		},
		"ReservedServerPort": {
			field:    "ServerPort",
			value:    1023,
			expected: fmt.Errorf("please do not try to use reserved port 1023 for the server port"),
		},
		"StartValidServerPort": {
			field:    "ServerPort",
			value:    1024,
			expected: nil,
		},
		"EndValidServerPort": {
			field:    "ServerPort",
			value:    49151,
			expected: nil,
		},
		"StartEphemeralServerPort": {
			field:    "ServerPort",
			value:    49152,
			expected: fmt.Errorf("please do not try to use ephemeral port 49152 for the server port"),
		},
		"ReservedAdminPort": {
			field:    "AdminPort",
			value:    1023,
			expected: fmt.Errorf("please do not try to use reserved port 1023 for the admin port"),
		},
		"StartValidAdminPort": {
			field:    "AdminPort",
			value:    1024,
			expected: nil,
		},
		"EndValidAdminPort": {
			field:    "AdminPort",
			value:    49151,
			expected: nil,
		},
		"StartEphemeralAdminPort": {
			field:    "AdminPort",
			value:    49152,
			expected: fmt.Errorf("please do not try to use ephemeral port 49152 for the admin port"),
		},
		"ReservedHealthPort": {
			field:    "HealthPort",
			value:    1023,
			expected: fmt.Errorf("please do not try to use reserved port 1023 for the health port"),
		},
		"StartValidHealthPort": {
			field:    "HealthPort",
			value:    1024,
			expected: nil,
		},
		"EndValidHealthPort": {
			field:    "HealthPort",
			value:    49151,
			expected: nil,
		},
		"StartEphemeralHealthPort": {
			field:    "HealthPort",
			value:    49152,
			expected: fmt.Errorf("please do not try to use ephemeral port 49152 for the health port"),
		},
		"CommaSparatedCipherSuites": {
			field:    "CipherSuites",
			value:    "TLS_AES_256_GCM_SHA384,TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA",
			expected: nil,
		},
		"Empty proxy strategies": {
			field:    "ProxyStrategies",
			value:    "",
			expected: fmt.Errorf("ProxyStrategies cannot be empty"),
		},
		"Invalid proxy strategies": {
			field:    "ProxyStrategies",
			value:    "invalid",
			expected: fmt.Errorf("invalid proxy strategies: unknown proxy strategy: invalid"),
		},
		"ZeroXfrChannelSize": {
			field:    "XfrChannelSize",
			value:    0,
			expected: fmt.Errorf("channel size 0 must be greater than 0"),
		},
		"NegativeXfrChannelSize": {
			field:    "XfrChannelSize",
			value:    -10,
			expected: fmt.Errorf("channel size -10 must be greater than 0"),
		},
	} {
		t.Run(desc, func(t *testing.T) {
			testServerOptions := NewProxyRunOptions()
			if tc.field == "CipherSuites" {
				testServerOptions.Flags().Set("cipher-suites", tc.value.(string))
			} else if tc.field != "" {
				rv := reflect.ValueOf(testServerOptions)
				rv = rv.Elem()
				fv := rv.FieldByName(tc.field)
				switch reflect.TypeOf(tc.value).Kind() {
				case reflect.String:
					svalue := tc.value.(string)
					fv.SetString(svalue)
				case reflect.Int:
					ivalue := tc.value.(int)
					fv.SetInt(int64(ivalue))
				}
			}
			actual := testServerOptions.Validate()
			assert.IsType(t, tc.expected, actual, "Validation for field %s, got the wrong type.", tc.field)
			assert.Equal(t, tc.expected, actual, "Validation for field %s, got the wrong value.", tc.field)
		})
	}
}
