/*
Copyright 2026 The Kubernetes Authors.

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
	"testing"

	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
)

func TestBackendRecvChannelOccupancy(t *testing.T) {
	tests := []struct {
		name     string
		capacity int
		packets  int
		want     float64
	}{
		{name: "unset", capacity: -1, want: 0},
		{name: "unbuffered", capacity: 0, want: 0},
		{name: "empty", capacity: 4, want: 0},
		{name: "partially occupied", capacity: 4, packets: 1, want: 0.25},
		{name: "full", capacity: 4, packets: 4, want: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var recvCh chan *client.Packet
			if test.capacity >= 0 {
				recvCh = make(chan *client.Packet, test.capacity)
			}
			for i := 0; i < test.packets; i++ {
				recvCh <- &client.Packet{}
			}

			backend := &Backend{recvCh: recvCh}
			if got := backend.RecvChannelOccupancy(); got != test.want {
				t.Fatalf("RecvChannelOccupancy() = %v, want %v", got, test.want)
			}
		})
	}
}
