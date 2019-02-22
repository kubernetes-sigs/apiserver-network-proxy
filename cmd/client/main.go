package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"io"
	"io/ioutil"
	"os"

	"github.com/anfernee/proxy-service/pkg/agent/client"
	"github.com/golang/glog"
	"github.com/spf13/cobra"
)

func main() {
	client := &Client{}
	o := newGrpcProxyClientOptions()
	command := newGrpcProxyClientCommand(client, o)
	flags := command.Flags()
	flags.AddFlagSet(o.Flags())
	if err := command.Execute(); err != nil {
		glog.Errorf( "error: %v\n", err)
		os.Exit(1)
	}
}

type GrpcProxyClientOptions struct {
	clientCert string
	clientKey  string
	caCert     string
}

func (o *GrpcProxyClientOptions) Flags() *pflag.FlagSet {
	flags := pflag.NewFlagSet("proxy", pflag.ContinueOnError)
	flags.StringVar(&o.clientCert, "clientCert", o.clientCert, "If non-empty secure communication with this cert.")
	flags.StringVar(&o.clientKey, "clientKey", o.clientKey, "If non-empty secure communication with this key.")
	flags.StringVar(&o.caCert, "caCert", o.caCert, "If non-empty the CAs we use to validate clients.")
	return flags
}

func (o *GrpcProxyClientOptions) Print() {
	glog.Warningf("ServerCert set to \"%s\".\n", o.clientCert)
	glog.Warningf("ServerKey set to \"%s\".\n", o.clientKey)
	glog.Warningf("CACert set to \"%s\".\n", o.caCert)
}

func (o *GrpcProxyClientOptions) Validate() error {
	if o.clientKey != "" {
		if _, err := os.Stat(o.clientKey); os.IsNotExist(err) {
			return err
		}
		if o.clientCert == "" {
			return fmt.Errorf("cannot have server cert empty when server key is set to \"%s\"", o.clientKey)
		}
	}
	if o.clientCert != "" {
		if _, err := os.Stat(o.clientCert); os.IsNotExist(err) {
			return err
		}
		if o.clientKey == "" {
			return fmt.Errorf("cannot have server key empty when server cert is set to \"%s\"", o.clientCert)
		}
	}
	if o.caCert != "" {
		if _, err := os.Stat(o.caCert); os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func newGrpcProxyClientOptions() *GrpcProxyClientOptions {
	o := GrpcProxyClientOptions{
		clientCert: "",
		clientKey:  "",
		caCert:     "",
	}
	return &o
}

func newGrpcProxyClientCommand(c *Client, o *GrpcProxyClientOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "proxy-client",
		Long: `A gRPC proxy Client, primarily used to test the Kubernetes gRPC Proxy Server.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return c.run(o)
		},
	}

	return cmd
}

type Client struct {

}

func (c *Client) run(o *GrpcProxyClientOptions) error {
	o.Print()
	if err := o.Validate(); err != nil {
		return err
	}

	// Run remote simple http service on server side as
	// "python -m SimpleHTTPServer"

	clientCert, err := tls.LoadX509KeyPair(o.clientCert, o.clientKey)
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

	transportCreds := credentials.NewTLS(&tls.Config{
		ServerName:   "127.0.0.1",
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      certPool,
	})

	dialOption := grpc.WithTransportCredentials(transportCreds)
	tunnel, err := client.CreateGrpcTunnel("localhost:8090", dialOption)
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


