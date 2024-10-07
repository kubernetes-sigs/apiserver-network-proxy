/*
Copyright 2024 The Kubernetes Authors.

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

package server

import (
	"math/rand"
	"slices"
	"sync"
	"time"
)

// BackendStorage is an interface for an in-memory storage of backend
// streams.
//
// A key may be associated with multiple Backend objects. For example,
// a given agent can have multiple re-connects in flight, and multiple
// agents could share a common host identifier.
type BackendStorage interface {
	// AddBackend registers a backend, and returns the new number of backends in this storage.
	AddBackend(keys []string, backend *Backend) int
	// RemoveBackend removes a backend, and returns the new number of backends in this storage.
	RemoveBackend(keys []string, backend *Backend) int
	// GetBackend selects a backend by key.
	GetBackend(key string) (*Backend, error)
	// RandomBackend selects a random backend.
	RandomBackend() (*Backend, error)
	// NumKeys returns the distinct count of backend keys in this storage.
	NumKeys() int
}

// DefaultBackendStorage is the default BackendStorage
type DefaultBackendStorage struct {
	mu sync.RWMutex //protects the following
	// A map from key to grpc connections.
	// For a given "backends []Backend", ProxyServer prefers backends[0] to send
	// traffic, because backends[1:] are more likely to be closed
	// by the agent to deduplicate connections to the same server.
	backends map[string][]*Backend
	// Cache of backends keys, to efficiently select random.
	backendKeys []string

	random *rand.Rand
}

var _ BackendStorage = &DefaultBackendStorage{}

// NewDefaultBackendStorage returns a DefaultBackendStorage
func NewDefaultBackendStorage() *DefaultBackendStorage {
	// Set an explicit value, so that the metric is emitted even when
	// no agent ever successfully connects.
	return &DefaultBackendStorage{
		backends: make(map[string][]*Backend),
		random:   rand.New(rand.NewSource(time.Now().UnixNano())),
	} /* #nosec G404 */
}

func (s *DefaultBackendStorage) AddBackend(keys []string, backend *Backend) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		if key == "" {
			continue
		}
		_, ok := s.backends[key]
		if ok {
			if !slices.Contains(s.backends[key], backend) {
				s.backends[key] = append(s.backends[key], backend)
			}
			continue
		}
		s.backends[key] = []*Backend{backend}
		s.backendKeys = append(s.backendKeys, key)
	}
	return len(s.backends)
}

func (s *DefaultBackendStorage) RemoveBackend(keys []string, backend *Backend) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		if key == "" {
			continue
		}
		backends, ok := s.backends[key]
		if !ok {
			continue
		}
		for i, b := range backends {
			if b == backend {
				s.backends[key] = slices.Delete(backends, i, i+1)
			}
		}
		if len(s.backends[key]) == 0 {
			delete(s.backends, key)
			s.backendKeys = slices.DeleteFunc(s.backendKeys, func(k string) bool {
				return k == key
			})
		}
	}
	return len(s.backends)
}

func (s *DefaultBackendStorage) GetBackend(key string) (*Backend, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bes, exist := s.backends[key]
	if exist && len(bes) > 0 {
		// always return the first connection to an agent, because the agent
		// will close later connections if there are multiple.
		return bes[0], nil
	}
	return nil, &ErrNotFound{}
}

func (s *DefaultBackendStorage) RandomBackend() (*Backend, error) {
	// Lock and not RLock, because s.random is not safe for concurrent use.
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.backends) == 0 {
		return nil, &ErrNotFound{}
	}
	key := s.backendKeys[s.random.Intn(len(s.backendKeys))]
	// always return the first connection to an agent, because the agent
	// will close later connections if there are multiple.
	return s.backends[key][0], nil
}

func (s *DefaultBackendStorage) NumKeys() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.backends)
}
