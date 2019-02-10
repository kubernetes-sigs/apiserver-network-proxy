package server

import (
	"context"
	"log"
	"net"
	"sync/atomic"

	proxy "github.com/anfernee/proxy-service/proto"
)

type ProxyServer struct {
	nextStreamID int32
	conns        map[int32]net.Conn
}

func NewProxyServer() *ProxyServer {
	return &ProxyServer{
		conns: make(map[int32]net.Conn),
	}
}

func (s *ProxyServer) Connect(stream proxy.ProxyService_ConnectServer) error {
	data, err := stream.Recv()
	if err != nil {
		return err
	}

	var conn net.Conn
	var ok bool
	if conn, ok = s.conns[data.StreamID]; !ok {
		// TODO: use metadata.MD?
		return stream.Send(&proxy.Data{Error: "stream id not found"})
	}

	closeCh := make(chan struct{})

	go read(conn, stream, closeCh)
	go write(conn, stream, closeCh)

	<-closeCh
	return nil
}

func read(conn net.Conn, stream proxy.ProxyService_ConnectServer, closeCh chan struct{}) {
	defer func() {
		close(closeCh)
	}()

	var buf [1 << 12]byte

	for {
		select {
		case <-closeCh:
			return
		default:
		}

		n, err := conn.Read(buf[:])
		if err != nil {
			log.Printf("conn read error: %v", err)
			return
		}

		log.Printf("read data: %v", string(buf[:n]))

		err = stream.Send(&proxy.Data{Data: buf[:n]})
		if err != nil {
			log.Printf("stream send error: %v", err)
			return
		}
	}
}

func write(conn net.Conn, stream proxy.ProxyService_ConnectServer, closeCh chan struct{}) {
	defer func() {
		close(closeCh)
	}()

	for {
		select {
		case <-closeCh:
			return
		default:
		}

		data, err := stream.Recv()
		if err != nil {
			log.Printf("stream recv error: %v", err)
			return
		}

		log.Printf("read data from stream: %v", string(data.Data))

		pos := 0
		for {
			n, err := conn.Write(data.Data[pos:])
			if n > 0 {
				pos += n
				continue
			}
			if err != nil {
				log.Printf("conn write error: %v", err)
				return
			}
		}
	}
}

func (s *ProxyServer) Dial(ctx context.Context, req *proxy.DialRequest) (*proxy.DialResponse, error) {
	conn, err := net.Dial(req.Protocol, req.Address)
	if err != nil {
		return &proxy.DialResponse{Error: err.Error()}, nil
	}

	sid := atomic.AddInt32(&s.nextStreamID, 1)
	s.conns[sid] = conn

	return &proxy.DialResponse{StreamID: sid}, nil
}
