package main

import (
	"fmt"
	"net"

	"google.golang.org/grpc"

	"github.com/anfernee/proxy-service/pkg/agent/agentserver"
	"github.com/anfernee/proxy-service/proto/agent"
	"github.com/golang/glog"
	"os"
	"github.com/spf13/cobra"
)

func main() {
	proxy := &Proxy{}
	command := newProxyCommand(proxy)
	if err := command.Execute(); err != nil {
		glog.Errorf( "error: %v\n", err)
		os.Exit(1)
	}
}


func newProxyCommand(p *Proxy) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "proxy",
		Long: `A gRPC proxy server, receives requests from the API server and forwards to the agent.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return p.run()
		},
	}

	return cmd
}

type Proxy struct {

}

func (p *Proxy) run() error {
	server := agentserver.NewProxyServer()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", 8090))
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer()

	agent.RegisterProxyServiceServer(grpcServer, server)
	go grpcServer.Serve(lis)

	lis2, err := net.Listen("tcp", fmt.Sprintf(":%d", 8091))
	if err != nil {
		return err
	}

	grpcServer2 := grpc.NewServer()

	agent.RegisterAgentServiceServer(grpcServer2, server)
	go grpcServer2.Serve(lis2)

	stopCh := make(chan struct{})
	<-stopCh

	return nil
}
