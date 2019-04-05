package main

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/credentials"
	"net"
	"net/http"
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
	// flag.CommandLine.Parse(os.Args[1:])
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
	// Certificate setup for securing communication to the "client" i.e. the Kube API Server.
	serverCert    string
	serverKey     string
	serverCaCert  string
	// Certificate setup for securing communication to the "agent" i.e. the managed cluster.
	clusterCert   string
	clusterKey    string
	clusterCaCert string
	// Flag to switch between gRPC and HTTP Connect
	mode string
	// Port we listen for server connections on.
	serverPort uint
	// Port we listen for agent connections on.
	agentPort uint
	// Port we listen for admin connections on.
	adminPort uint
}

func (o *ProxyRunOptions) Flags() *pflag.FlagSet {
	flags := pflag.NewFlagSet("proxy", pflag.ContinueOnError)
	flags.StringVar(&o.serverCert, "serverCert", o.serverCert, "If non-empty secure communication with this cert.")
	flags.StringVar(&o.serverKey, "serverKey", o.serverKey, "If non-empty secure communication with this key.")
	flags.StringVar(&o.serverCaCert, "serverCaCert", o.serverCaCert, "If non-empty the CA we use to validate KAS clients.")
	flags.StringVar(&o.clusterCert, "clusterCert", o.clusterCert, "If non-empty secure communication with this cert.")
	flags.StringVar(&o.clusterKey, "clusterKey", o.clusterKey, "If non-empty secure communication with this key.")
	flags.StringVar(&o.clusterCaCert, "clusterCaCert", o.clusterCaCert, "If non-empty the CA we use to validate Agent clients.")
	flags.StringVar(&o.mode, "mode", "grpc", "Mode can be either 'grpc' or 'http-connect'.")
	flags.UintVar(&o.serverPort, "serverPort", 8090, "Port we listen for server connections on.")
	flags.UintVar(&o.agentPort,"agentPort", 8091, "Port we listen for agent connections on.")
	flags.UintVar(&o.adminPort,"adminPort", 8092, "Port we listen for admin connections on.")
	return flags
}

func (o *ProxyRunOptions) Print() {
	glog.Warningf("ServerCert set to %q.\n", o.serverCert)
	glog.Warningf("ServerKey set to %q.\n", o.serverKey)
	glog.Warningf("ServerCACert set to %q.\n", o.serverCaCert)
	glog.Warningf("ClusterCert set to %q.\n", o.clusterCert)
	glog.Warningf("ClusterKey set to %q.\n", o.clusterKey)
	glog.Warningf("ClusterCACert set to %q.\n", o.clusterCaCert)
	glog.Warningf("Mode set to %q.\n", o.mode)
	glog.Warningf("Server port set to %d.\n", o.serverPort)
	glog.Warningf("Agent port set to %d.\n", o.agentPort)
	glog.Warningf("Admin port set to %d.\n", o.adminPort)
}

func (o *ProxyRunOptions) Validate() error {
	if o.serverKey != "" {
		if _, err := os.Stat(o.serverKey); os.IsNotExist(err) {
			return err
		}
		if o.serverCert == "" {
			return fmt.Errorf("cannot have server cert empty when server key is set to %q", o.serverKey)
		}
	}
	if o.serverCert != "" {
		if _, err := os.Stat(o.serverCert); os.IsNotExist(err) {
			return err
		}
		if o.serverKey == "" {
			return fmt.Errorf("cannot have server key empty when server cert is set to %q", o.serverCert)
		}
	}
	if o.serverCaCert != "" {
		if _, err := os.Stat(o.serverCaCert); os.IsNotExist(err) {
			return err
		}
	}
	if o.clusterKey != "" {
		if _, err := os.Stat(o.clusterKey); os.IsNotExist(err) {
			return err
		}
		if o.clusterCert == "" {
			return fmt.Errorf("cannot have cluster cert empty when cluster key is set to %q", o.clusterKey)
		}
	}
	if o.clusterCert != "" {
		if _, err := os.Stat(o.clusterCert); os.IsNotExist(err) {
			return err
		}
		if o.clusterKey == "" {
			return fmt.Errorf("cannot have cluster key empty when cluster cert is set to %q", o.clusterCert)
		}
	}
	if o.clusterCaCert != "" {
		if _, err := os.Stat(o.clusterCaCert); os.IsNotExist(err) {
			return err
		}
	}
	if o.mode != "grpc" && o.mode != "http-connect" {
		return fmt.Errorf("mode must be set to either 'grpc' or 'http-connect' not %q", o.mode)
	}
	if o.serverPort > 49151 {
		return fmt.Errorf("please do not try to use ephemeral port %d for the server port", o.serverPort)
	}
	if o.agentPort > 49151 {
		return fmt.Errorf("please do not try to use ephemeral port %d for the agent port", o.agentPort)
	}
	if o.adminPort > 49151 {
		return fmt.Errorf("please do not try to use ephemeral port %d for the admin port", o.adminPort)
	}
	if o.serverPort < 1024 {
		return fmt.Errorf("please do not try to use reserved port %d for the server port", o.serverPort)
	}
	if o.agentPort < 1024 {
		return fmt.Errorf("please do not try to use reserved port %d for the agent port", o.agentPort)
	}
	if o.adminPort < 1024 {
		return fmt.Errorf("please do not try to use reserved port %d for the admin port", o.adminPort)
	}
	return nil
}

func newProxyRunOptions() *ProxyRunOptions {
	o := ProxyRunOptions{
		serverCert: "",
		serverKey: "",
		serverCaCert: "",
		clusterCert: "",
		clusterKey: "",
		clusterCaCert: "",
		mode: "grpc",
		serverPort: 8090,
		agentPort: 8091,
		adminPort: 8092,
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

	glog.Info("Starting master server for client connections.")
	err := p.runMasterServer(o, server)
	if err != nil {
		return err
	}

	glog.Info("Starting agent server for tunnel connections.")
	err = p.runAgentServer(o, server)
	if err != nil {
		return err
	}

	glog.Info("Starting admin server for debug connections.")
	err = p.runAdminServer(o, server)
	if err != nil {
		return err
	}

	stopCh := make(chan struct{})
	<-stopCh

	return nil
}

func (p *Proxy) runMasterServer(o *ProxyRunOptions, server *agentserver.ProxyServer) error {
	proxyCert, err := tls.LoadX509KeyPair(o.serverCert, o.serverKey)
	if err != nil {
		return err
	}
	certPool := x509.NewCertPool()
	caCert, err := ioutil.ReadFile(o.serverCaCert)
	if err != nil {
		return err
	}
	ok := certPool.AppendCertsFromPEM(caCert)
	if !ok {
		return fmt.Errorf("failed to append master CA cert to the cert pool")
	}

	tlsConfig := &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{proxyCert},
		ClientCAs:    certPool,
	}
	addr := fmt.Sprintf(":%d", o.serverPort)

	if o.mode == "grpc" {
		serverOption := grpc.Creds(credentials.NewTLS(tlsConfig))
		grpcServer := grpc.NewServer(serverOption)
		agent.RegisterProxyServiceServer(grpcServer, server)
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		go grpcServer.Serve(lis)
	} else {
		go func() {
			// http-connect
			server := &http.Server{
				Addr:         addr,
				TLSConfig:    tlsConfig,
				Handler:      &agentserver.Tunnel{
					Server: server,
				},
				TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
			}
			err := server.ListenAndServeTLS("", "") // empty files defaults to tlsConfig
			if err != nil {
				glog.Errorf("failed to listen on master port %v", err)
			}
		}()
	}

	return nil
}

func (p *Proxy) runAgentServer(o *ProxyRunOptions, server *agentserver.ProxyServer) error {
	clusterCert, err := tls.LoadX509KeyPair(o.clusterCert, o.clusterKey)
	if err != nil {
		return err
	}
	certPool := x509.NewCertPool()
	caCert, err := ioutil.ReadFile(o.clusterCaCert)
	if err != nil {
		return err
	}
	ok := certPool.AppendCertsFromPEM(caCert)
	if !ok {
		return fmt.Errorf("failed to append cluster CA cert to the cert pool")
	}
	tlsConfig := &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{clusterCert},
		ClientCAs:    certPool,
	}
	addr := fmt.Sprintf(":%d", o.agentPort)

	serverOption := grpc.Creds(credentials.NewTLS(tlsConfig))
	grpcServer := grpc.NewServer(serverOption)
	agent.RegisterAgentServiceServer(grpcServer, server)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go grpcServer.Serve(lis)

	return nil
}

func (p *Proxy) runAdminServer(o *ProxyRunOptions, server *agentserver.ProxyServer) error {
	livenessHandler := http.HandlerFunc(func (w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok")
	})
	readinessHandler := http.HandlerFunc(func (w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok")
	})
	metricsHandler := http.HandlerFunc(func (w http.ResponseWriter, r *http.Request) {
		prometheus.Handler().ServeHTTP(w, r)
	})

	muxHandler := http.NewServeMux()
	muxHandler.HandleFunc("/healthz", livenessHandler)
	muxHandler.HandleFunc("/ready", readinessHandler)
	muxHandler.HandleFunc("/metrics", metricsHandler)
	adminServer := &http.Server{
		Addr:           fmt.Sprintf("127.0.0.1:%d", o.adminPort),
		Handler:        muxHandler,
		MaxHeaderBytes: 1 << 20,
	}

	go func() {
		err := adminServer.ListenAndServe()
		if err != nil {
			glog.Warningf("health server received %v.\n", err)
		}
		glog.Warningf("Health server stopped listening\n")
	}()

	return nil
}

