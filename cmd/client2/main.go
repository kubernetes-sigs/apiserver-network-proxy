package main

import (
	"fmt"
	"io"

	// "github.com/anfernee/proxy-service/proto/agent"
	"github.com/anfernee/proxy-service/pkg/agent/client"
)

func main() {
	// Run remote simple http service on server side as
	// "python -m SimpleHTTPServer"

	tunnel, err := client.CreateGrpcTunnel("localhost:8090")
	if err != nil {
		panic(err)
	}

	conn, err := tunnel.Dial("tcp", "localhost:8000")
	if err != nil {
		panic(err)
	}

	_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"))
	if err != nil {
		panic(err)
	}

	var buf [1 << 12]byte

	for {
		n, err := conn.Read(buf[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
		fmt.Println(string(buf[:n]))
	}
}
