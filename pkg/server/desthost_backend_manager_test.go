package server

import (
	"context"
	"testing"

	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

type fakeAgent struct {
	ctx context.Context
	agent.AgentService_ConnectServer
}

func (fa *fakeAgent) Context() context.Context {
	return fa.ctx
}

const contextNameKey key = iota

func newNamedBackend(name string) *backend {
	return &backend{
		conn: &fakeAgent{
			ctx: context.WithValue(context.Background(), contextNameKey, name),
		},
	}
}

func TestBackend(t *testing.T) {

	testCases := []struct {
		name                string
		destHost            string
		backends            map[string][]*backend
		expectedBackendName string
	}{
		{
			name:     "Literal match",
			destHost: "sts.amazonaws.com",
			backends: map[string][]*backend{
				"sts.amazonaws.com": {newNamedBackend("sts.amazonaws.com")},
			},
			expectedBackendName: "sts.amazonaws.com",
		},
		{
			name:     "Wildcard match",
			destHost: "sts.amazonaws.com",
			backends: map[string][]*backend{
				".amazonaws.com": {newNamedBackend(".amazonaws.com")},
			},
			expectedBackendName: ".amazonaws.com",
		},
		{
			name:     "Both literal and wildcard match, literal match takes precedence",
			destHost: "sts.amazonaws.com",
			backends: map[string][]*backend{
				"sts.amazonaws.com": {newNamedBackend("sts.amazonaws.com")},
				".amazonaws.com":    {newNamedBackend(".amazonaws.com")},
			},
			expectedBackendName: "sts.amazonaws.com",
		},
		{
			name:     "No wildcard match when entry doesn't start with dot",
			destHost: "foo-bar.com",
			backends: map[string][]*backend{
				"bar.com": {newNamedBackend("bar.com")},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			request := context.WithValue(context.Background(), destHost, tc.destHost)
			mgr := NewDestHostBackendManager()
			mgr.backends = tc.backends

			matchExpected := tc.expectedBackendName != ""
			match, err := mgr.Backend(request)
			hadMatch := err == nil
			if matchExpected != hadMatch {
				t.Fatalf("expected a match: %t, got a match: %t", matchExpected, hadMatch)
			}
			if !matchExpected {
				return
			}

			matchName := match.Context().Value(contextNameKey).(string)
			if matchName != tc.expectedBackendName {
				t.Errorf("expected to get backend %s, got %s", tc.expectedBackendName, matchName)
			}
		})
	}
}
