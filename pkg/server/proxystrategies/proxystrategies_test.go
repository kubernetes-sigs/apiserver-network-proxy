package proxystrategies

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProxyStrategy(t *testing.T) {
	for desc, tc := range map[string]struct {
		input     ProxyStrategy
		want      string
		wantPanic string
	}{
		"default": {
			input: ProxyStrategyDefault,
			want:  "default",
		},
		"destHost": {
			input: ProxyStrategyDestHost,
			want:  "destHost",
		},
		"defaultRoute": {
			input: ProxyStrategyDefaultRoute,
			want:  "defaultRoute",
		},
		"unrecognized": {
			input:     ProxyStrategy(0),
			wantPanic: "unhandled ProxyStrategy: 0",
		},
	} {
		t.Run(desc, func(t *testing.T) {
			if tc.wantPanic != "" {
				assert.PanicsWithValue(t, tc.wantPanic, func() {
					_ = tc.input.String()
				})
			} else {
				got := tc.input.String()
				if got != tc.want {
					t.Errorf("ProxyStrategy.String(): got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestParseProxyStrategy(t *testing.T) {
	for desc, tc := range map[string]struct {
		input   string
		want    ProxyStrategy
		wantErr error
	}{
		"empty": {
			input:   "",
			wantErr: fmt.Errorf("unknown proxy strategy: "),
		},
		"unrecognized": {
			input:   "unrecognized",
			wantErr: fmt.Errorf("unknown proxy strategy: unrecognized"),
		},
		"default": {
			input: "default",
			want:  ProxyStrategyDefault,
		},
		"destHost": {
			input: "destHost",
			want:  ProxyStrategyDestHost,
		},
		"defaultRoute": {
			input: "defaultRoute",
			want:  ProxyStrategyDefaultRoute,
		},
	} {
		t.Run(desc, func(t *testing.T) {
			got, err := ParseProxyStrategy(tc.input)
			assert.Equal(t, tc.wantErr, err, "ParseProxyStrategy(%s): got error %q, want %v", tc.input, err, tc.wantErr)
			if got != tc.want {
				t.Errorf("ParseProxyStrategy(%s): got %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseProxyStrategies(t *testing.T) {
	for desc, tc := range map[string]struct {
		input   string
		want    []ProxyStrategy
		wantErr error
	}{
		"empty": {
			input:   "",
			wantErr: fmt.Errorf("proxy strategies cannot be empty"),
		},
		"unrecognized": {
			input:   "unrecognized",
			wantErr: fmt.Errorf("unknown proxy strategy: unrecognized"),
		},
		"default": {
			input: "default",
			want:  []ProxyStrategy{ProxyStrategyDefault},
		},
		"destHost": {
			input: "destHost",
			want:  []ProxyStrategy{ProxyStrategyDestHost},
		},
		"defaultRoute": {
			input: "defaultRoute",
			want:  []ProxyStrategy{ProxyStrategyDefaultRoute},
		},
		"duplicate": {
			input: "destHost,defaultRoute,defaultRoute,default",
			want:  []ProxyStrategy{ProxyStrategyDestHost, ProxyStrategyDefaultRoute, ProxyStrategyDefaultRoute, ProxyStrategyDefault},
		},
		"multiple": {
			input: "destHost,defaultRoute,default",
			want:  []ProxyStrategy{ProxyStrategyDestHost, ProxyStrategyDefaultRoute, ProxyStrategyDefault},
		},
	} {
		t.Run(desc, func(t *testing.T) {
			got, err := ParseProxyStrategies(tc.input)
			assert.Equal(t, tc.wantErr, err, "ParseProxyStrategies(%s): got error %q, want %v", tc.input, err, tc.wantErr)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseProxyStrategies(%s): got %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
