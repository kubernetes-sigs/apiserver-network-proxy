package tests

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	clientproto "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

type simpleServer struct {
	receivedSecondReq chan struct{}
}

// ServeHTTP blocks the response to the request whose body is "1" until a
// request whose body is "2" is handled.
func (s *simpleServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	bytes, err := ioutil.ReadAll(req.Body)
	if err != nil {
		w.Write([]byte(err.Error()))
	}
	if string(bytes) == "2" {
		close(s.receivedSecondReq)
		w.Write([]byte("2"))
	}
	if string(bytes) == "1" {
		<-s.receivedSecondReq
		w.Write([]byte("1"))
	}
}

// TODO: test http-connect as well.
func getTestClient(front string, t *testing.T) *http.Client {
	tunnel, err := client.CreateSingleUseGrpcTunnel(front, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}

	return &http.Client{
		Transport: &http.Transport{
			Dial: tunnel.Dial,
		},
		Timeout: 2 * time.Second,
	}
}

type backend struct {
	mu      sync.Mutex // mu protects conn
	conn    agent.AgentService_ConnectServer
	agentID string
}

func (b *backend) Send(p *clientproto.Packet) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn.Send(p)
}

func (b *backend) Context() context.Context {
	// TODO: does Context require lock protection?
	return b.conn.Context()
}

func (b *backend) AgentID() string {
	return b.agentID
}

func newBackend(agentID string, conn agent.AgentService_ConnectServer) *backend {
	return &backend{agentID: agentID, conn: conn}
}

// singleTimeManager makes sure that a backend only serves one request.
type singleTimeManager struct {
	mu       sync.Mutex
	backends map[string][]*backend
	used     map[string]struct{}
	agentIDs []string
}

func (s *singleTimeManager) AddBackend(agentID string, conn agent.AgentService_ConnectServer) {
	klog.Infof("register Backend %v for agentID %s", conn, agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.backends[agentID]
	if ok {
		for _, v := range s.backends[agentID] {
			if v.conn == conn {
				klog.Warningf("this should not happen. Adding existing connection %v for agentID %s", conn, agentID)
				return
			}
		}
		s.backends[agentID] = append(s.backends[agentID], newBackend(agentID, conn))
		return
	}
	s.backends[agentID] = []*backend{newBackend(agentID, conn)}
}

func (s *singleTimeManager) RemoveBackend(agentID string, conn agent.AgentService_ConnectServer) {
	klog.Infof("remove Backend %v for agentID %s", conn, agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	backends, ok := s.backends[agentID]
	if !ok {
		klog.Warningf("can't find agentID %s in the backends", agentID)
		return
	}
	var found bool
	for i, c := range backends {
		if c.conn == conn {
			s.backends[agentID] = append(s.backends[agentID][:i], s.backends[agentID][i+1:]...)
			if i == 0 && len(s.backends[agentID]) != 0 {
				klog.Warningf("this should not happen. Removed connection %v that is not the first connection, remaining connections are %v", conn, s.backends[agentID])
			}
			found = true
		}
	}
	if len(s.backends[agentID]) == 0 {
		delete(s.backends, agentID)
		for i := range s.agentIDs {
			if s.agentIDs[i] == agentID {
				s.agentIDs[i] = s.agentIDs[len(s.agentIDs)-1]
				s.agentIDs = s.agentIDs[:len(s.agentIDs)-1]
				break
			}
		}
	}
	if !found {
		klog.Errorf("can't find conn %v for agentID %s in the backends", conn, agentID)
	}
}

func (s *singleTimeManager) Backend() (server.Backend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, backends := range s.backends {
		for i, v := range backends {
			idx := fmt.Sprintf("%s%d", k, i)
			if _, ok := s.used[idx]; !ok {
				s.used[idx] = struct{}{}
				return v, nil
			}
		}
	}
	return nil, fmt.Errorf("cannot find backend to a new agent")
}

func newSingleTimeGetter(m *server.DefaultBackendManager) *singleTimeManager {
	return &singleTimeManager{
		used:     make(map[string]struct{}),
		backends: make(map[string][]*backend),
	}
}

var _ server.BackendManager = &singleTimeManager{}

func TestConcurrentClientRequest(t *testing.T) {
	s := httptest.NewServer(&simpleServer{receivedSecondReq: make(chan struct{})})
	defer s.Close()

	proxy, ps, cleanup, err := runGRPCProxyServerWithServerCount(1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ps.BackendManager = newSingleTimeGetter(server.NewDefaultBackendManager())

	stopCh := make(chan struct{})
	defer close(stopCh)
	// Run two agents
	runAgent(proxy.agent, stopCh)
	runAgent(proxy.agent, stopCh)

	client1 := getTestClient(proxy.front, t)
	client2 := getTestClient(proxy.front, t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r, err := client1.Post(s.URL, "text/plain", bytes.NewBufferString("1"))
		if err != nil {
			t.Error(err)
			return
		}
		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		r.Body.Close()

		if string(data) != "1" {
			t.Errorf("expect %v; got %v", "1", string(data))
		}
	}()
	// give client1 some time to establish the connection.
	time.Sleep(1 * time.Second)
	go func() {
		defer wg.Done()
		r, err := client2.Post(s.URL, "text/plain", bytes.NewBufferString("2"))
		if err != nil {
			t.Error(err)
			return
		}
		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		r.Body.Close()

		if string(data) != "2" {
			t.Errorf("expect %v; got %v", "2", string(data))
		}
	}()
	wg.Wait()
}
