package main

import (
	"io"
	"os"

	"github.com/anfernee/proxy-service/pkg/agent/client"
	"github.com/golang/glog"
	"github.com/spf13/cobra"
)

func main() {
	client := &Client{}
	command := newGrpcProxyClientCommand(client)
	if err := command.Execute(); err != nil {
		glog.Errorf( "error: %v\n", err)
		os.Exit(1)
	}
}


func newGrpcProxyClientCommand(c *Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "proxy-client",
		Long: `A gRPC proxy Client, primarily used to test the Kubernetes gRPC Proxy Server.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return c.run()
		},
	}

	return cmd
}

type Client struct {

}

func (c *Client) run() error {
	// Run remote simple http service on server side as
	// "python -m SimpleHTTPServer"

	tunnel, err := client.CreateGrpcTunnel("localhost:8090")
	if err != nil {
		return err
	}

	conn, err := tunnel.Dial("tcp", "localhost:8000")
	if err != nil {
		return err
	}

	_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"))
	if err != nil {
		return err
	}

	var buf [1 << 12]byte

	for {
		n, err := conn.Read(buf[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		glog.Info(string(buf[:n]))
	}
	return nil
}


