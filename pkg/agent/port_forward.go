/*
Copyright 2021 The Kubernetes Authors.

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

package agent

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

// PortMapping represents the mapping between a local port and a remote
// destination (host:port).
type PortMapping struct {
	LocalPort  int
	RemoteHost string
	RemotePort int
}

func (pm PortMapping) String() string {
	return fmt.Sprintf("%d:%s:%d", pm.LocalPort, pm.RemoteHost, pm.RemotePort)
}

// Parse the string representation of the port mapping in the form
// <local_port>:<remote_host>:<remote_port> and fills the corresponding fields
// or return an error if the representation is not valid.
func (pm *PortMapping) Parse(s string) error {
	i := strings.Index(s, ":")
	if i < 0 || i == len(s)-1 {
		return fmt.Errorf("malformed port mapping %q, expected format: <local_port>:<remote_host>:<remote_port>", s)
	}
	rawLocPort := s[:i]
	localPort, err := strconv.Atoi(rawLocPort)
	if err != nil {
		return fmt.Errorf("error occurred while parsing local port: %v", err)
	}
	h, p, err := net.SplitHostPort(s[i+1:])
	if err != nil {
		return fmt.Errorf("error occurred while splitting remote host and port: %v", err)
	}
	remotePort, err := strconv.Atoi(p)
	if err != nil {
		return fmt.Errorf("error occurred while parsing remote port: %v", err)
	}
	// fill the fields at the end to avoid partially modifying the PortMapping
	pm.LocalPort = localPort
	pm.RemoteHost = h
	pm.RemotePort = remotePort
	return nil
}

// PortForwarder is capable of forwarding a local connection to a remote
// destination using a tunnel established between agent and proxy.
type PortForwarder struct {
	*ClientSet
	ListenHost string
}

// Serve accepts incoming TCP connections on the local port and forwards it to
// the remote destination defined by given the port mapping.
func (pf *PortForwarder) Serve(ctx context.Context, pm PortMapping) error {
	lc := net.ListenConfig{}
	listenAddr := net.JoinHostPort(pf.ListenHost, strconv.Itoa(pm.LocalPort))
	klog.V(1).InfoS("starting control plane proxy", "listen-address", listenAddr)
	// Only TCP is supported at the moment.
	listener, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return err
	}
	go func() {
		for {
			klog.V(2).InfoS("listening for connections", "listen-address", listenAddr)
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					klog.ErrorS(err, "Error occurred while waiting for connections")
				}
			} else {
				// Serve each connection in a dedicated goroutine
				go func() {
					if err := pf.handleConnection("tcp", net.JoinHostPort(pm.RemoteHost, strconv.Itoa(pm.RemotePort)), conn); err != nil {
						if err := conn.Close(); err != nil {
							klog.ErrorS(err, "Error while closing connection")
						}
						klog.ErrorS(err, "Error occurred while handling connection")
					}
				}()
			}
		}
	}()
	return nil
}
