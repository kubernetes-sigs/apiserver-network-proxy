package client

import (
	"errors"
	"net"
	"time"

	"github.com/anfernee/proxy-service/proto/agent"
	"github.com/golang/glog"
)

type conn struct {
	stream agent.ProxyService_ProxyClient
	connID int64
}

var _ net.Conn = &conn{}

func (c *conn) Write(data []byte) (n int, err error) {
	return 0, nil
}

func (c *conn) Read(b []byte) (n int, err error) {
	var data []byte

	for {
		pkt, err := c.stream.Recv()
		if err != nil {
			return 0, err
		}

		switch pkt.Type {
		case agent.PacketType_DATA:

		default:
			continue
		}
	}

	/*
		if c.rdata != nil {
			data = c.rdata
		} else {
			data = <-c.bufch
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
	*/

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
