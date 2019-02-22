package main

import (
	"fmt"
	"google.golang.org/grpc/credentials"
	"net"
	"os"

	"google.golang.org/grpc"

	"crypto/tls"
	"crypto/x509"
	"github.com/anfernee/proxy-service/pkg/agent/agentserver"
	"github.com/anfernee/proxy-service/proto/agent"
	"github.com/golang/glog"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"io/ioutil"
)

func main() {
	proxy := &Proxy{}
	o := newProxyRunOptions()
	command := newProxyCommand(proxy, o)
	flags := command.Flags()
	flags.AddFlagSet(o.Flags())
	if err := command.Execute(); err != nil {
		glog.Errorf( "error: %v\n", err)
		os.Exit(1)
	}
}

type ProxyRunOptions struct {
	serverCert string
	serverKey  string
	caCert     string
}

func (o *ProxyRunOptions) Flags() *pflag.FlagSet {
	flags := pflag.NewFlagSet("proxy", pflag.ContinueOnError)
	flags.StringVar(&o.serverCert, "serverCert", o.serverCert, "If non-empty secure communication with this cert.")
	flags.StringVar(&o.serverKey, "serverKey", o.serverKey, "If non-empty secure communication with this key.")
	flags.StringVar(&o.caCert, "caCert", o.caCert, "If non-empty the CAs we use to validate clients.")
	return flags
}

func (o *ProxyRunOptions) Print() {
	glog.Warningf("ServerCert set to \"%s\".\n", o.serverCert)
	glog.Warningf("ServerKey set to \"%s\".\n", o.serverKey)
	glog.Warningf("CACert set to \"%s\".\n", o.caCert)
}

func (o *ProxyRunOptions) Validate() error {
	if o.serverKey != "" {
		if _, err := os.Stat(o.serverKey); os.IsNotExist(err) {
			return err
		}
		if o.serverCert == "" {
			return fmt.Errorf("cannot have server cert empty when server key is set to \"%s\"", o.serverKey)
		}
	}
	if o.serverCert != "" {
		if _, err := os.Stat(o.serverCert); os.IsNotExist(err) {
			return err
		}
		if o.serverKey == "" {
			return fmt.Errorf("cannot have server key empty when server cert is set to \"%s\"", o.serverCert)
		}
	}
	if o.caCert != "" {
		if _, err := os.Stat(o.caCert); os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func newProxyRunOptions() *ProxyRunOptions {
	o := ProxyRunOptions{
		serverCert: "",
		serverKey: "",
		caCert: "",
	}
	return &o
}

func newProxyCommand(p *Proxy, o *ProxyRunOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "proxy",
		Long: `A gRPC proxy server, receives requests from the API server and forwards to the agent.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return p.run(o)
		},
	}

	return cmd
}

type Proxy struct {

}

func (p *Proxy) run(o *ProxyRunOptions) error {
	o.Print()
	if err := o.Validate(); err != nil {
		return err
	}
	server := agentserver.NewProxyServer()

	proxyCert, err := tls.LoadX509KeyPair(o.serverCert, o.serverKey)
	if err != nil {
		return err
	}
	certPool := x509.NewCertPool()
	caCert, err := ioutil.ReadFile(o.caCert)
	if err != nil {
		return err
	}
	ok := certPool.AppendCertsFromPEM(caCert)
	if !ok {
		return fmt.Errorf("failed to append CA cert to the cert pool")
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", 8090))
	if err != nil {
		return err
	}
	tlsConfig := &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{proxyCert},
		ClientCAs:    certPool,
	}
	serverOption := grpc.Creds(credentials.NewTLS(tlsConfig))
	grpcServer := grpc.NewServer(serverOption)

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
