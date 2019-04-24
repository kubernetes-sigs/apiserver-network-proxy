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

package client

import (
	"errors"
	"io"
	"net"
	"time"

	"github.com/anfernee/proxy-service/proto/agent"
	"github.com/golang/glog"
)

type conn struct {
	stream agent.ProxyService_ProxyClient
	connID int64
	readCh chan []byte
	rdata  []byte
}

var _ net.Conn = &conn{}

func (c *conn) Write(data []byte) (n int, err error) {
	req := &agent.Packet{
		Type: agent.PacketType_DATA,
		Payload: &agent.Packet_Data{
			Data: &agent.Data{
				ConnectID: c.connID,
				Data:      data,
			},
		},
	}

	glog.Infof("[tracing] send req %+v", req)

	err = c.stream.Send(req)
	if err != nil {
		return 0, err
	}
	return len(data), err
}

func (c *conn) Read(b []byte) (n int, err error) {
	var data []byte

	if c.rdata != nil {
		data = c.rdata
	} else {
		data = <-c.readCh
	}

	if data == nil {
		return 0, io.EOF
	}

	if len(data) > len(b) {
		copy(b, data[:len(b)])
		c.rdata = data[len(b):]
		return len(b), nil
	}

	c.rdata = nil
	copy(b, data)

	return len(data), nil
}

func (c *conn) LocalAddr() net.Addr {
	return nil
}

func (c *conn) RemoteAddr() net.Addr {
	return nil
}

func (c *conn) SetDeadline(t time.Time) error {
	return errors.New("not implemented")
}

func (c *conn) SetReadDeadline(t time.Time) error {
	return errors.New("not implemented")
}

func (c *conn) SetWriteDeadline(t time.Time) error {
	return errors.New("not implemented")
}

func (c *conn) Close() error {
	glog.Info("conn.Close()")
	return nil
}

// WriteBuffer writes to read buffer for connection to read from
func (c *conn) WriteBuffer(data []byte) (int, error) {
	// c.bufch <- data
	return len(data), nil
}
