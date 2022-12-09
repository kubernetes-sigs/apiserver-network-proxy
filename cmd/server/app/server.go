/*
Copyright 2022 The Kubernetes Authors.

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

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	netpprof "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	runpprof "runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"sigs.k8s.io/apiserver-network-proxy/cmd/server/app/options"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

var udsListenerLock sync.Mutex

const ReadHeaderTimeout = 60 * time.Second

func NewProxyCommand(p *Proxy, o *options.ProxyRunOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "proxy",
		Long: `A gRPC proxy server, receives requests from the API server and forwards to the agent.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return p.run(o)
		},
	}

	return cmd
}

func tlsCipherSuites(cipherNames []string) []uint16 {
	// return nil, so use default cipher list
	if len(cipherNames) == 0 {
		return nil
	}

	acceptedCiphers := util.GetAcceptedCiphers()
	ciphersIntSlice := make([]uint16, 0)
	for _, cipher := range cipherNames {
		ciphersIntSlice = append(ciphersIntSlice, acceptedCiphers[cipher])
	}
	return ciphersIntSlice
}

type Proxy struct {
}

type StopFunc func()

func (p *Proxy) run(o *options.ProxyRunOptions) error {
	o.Print()
	if err := o.Validate(); err != nil {
		return fmt.Errorf("failed to validate server options with %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var k8sClient *kubernetes.Clientset
	if o.AgentNamespace != "" {
		config, err := clientcmd.BuildConfigFromFlags("", o.KubeconfigPath)
		if err != nil {
			return fmt.Errorf("failed to load kubernetes client config: %v", err)
		}

		if o.KubeconfigQPS != 0 {
			klog.V(1).Infof("Setting k8s client QPS: %v", o.KubeconfigQPS)
			config.QPS = o.KubeconfigQPS
		}
		if o.KubeconfigBurst != 0 {
			klog.V(1).Infof("Setting k8s client Burst: %v", o.KubeconfigBurst)
			config.Burst = o.KubeconfigBurst
		}
		k8sClient, err = kubernetes.NewForConfig(config)
		if err != nil {
			return fmt.Errorf("failed to create kubernetes clientset: %v", err)
		}
	}

	authOpt := &server.AgentTokenAuthenticationOptions{
		Enabled:                o.AgentNamespace != "",
		AgentNamespace:         o.AgentNamespace,
		AgentServiceAccount:    o.AgentServiceAccount,
		KubernetesClient:       k8sClient,
		AuthenticationAudience: o.AuthenticationAudience,
	}
	klog.V(1).Infoln("Starting frontend server for client connections.")
	ps, err := server.GenProxyStrategiesFromStr(o.ProxyStrategies)
	if err != nil {
		return err
	}
	server := server.NewProxyServer(o.ServerID, ps, int(o.ServerCount), authOpt)

	frontendStop, err := p.runFrontendServer(ctx, o, server)
	if err != nil {
		return fmt.Errorf("failed to run the frontend server: %v", err)
	}

	klog.V(1).Infoln("Starting agent server for tunnel connections.")
	err = p.runAgentServer(o, server)
	if err != nil {
		return fmt.Errorf("failed to run the agent server: %v", err)
	}
	klog.V(1).Infoln("Starting admin server for debug connections.")
	err = p.runAdminServer(o, server)
	if err != nil {
		return fmt.Errorf("failed to run the admin server: %v", err)
	}
	klog.V(1).Infoln("Starting health server for healthchecks.")
	err = p.runHealthServer(o, server)
	if err != nil {
		return fmt.Errorf("failed to run the health server: %v", err)
	}

	stopCh := SetupSignalHandler()
	<-stopCh
	klog.V(1).Infoln("Shutting down server.")

	if frontendStop != nil {
		frontendStop()
	}

	return nil
}

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

func SetupSignalHandler() (stopCh <-chan struct{}) {
	stop := make(chan struct{})
	c := make(chan os.Signal, 2)
	signal.Notify(c, shutdownSignals...)
	labels := runpprof.Labels(
		"core", "signalHandler",
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) { handleSignals(c, stop) })

	return stop
}

func handleSignals(signalCh chan os.Signal, stopCh chan struct{}) {
	<-signalCh
	close(stopCh)
	<-signalCh
	os.Exit(1) // second signal. Exit directly.
}

func getUDSListener(ctx context.Context, udsName string) (net.Listener, error) {
	udsListenerLock.Lock()
	defer udsListenerLock.Unlock()
	oldUmask := syscall.Umask(0007)
	defer syscall.Umask(oldUmask)
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "unix", udsName)
	if err != nil {
		return nil, fmt.Errorf("failed to listen(unix) name %s: %v", udsName, err)
	}
	return lis, nil
}

func (p *Proxy) runFrontendServer(ctx context.Context, o *options.ProxyRunOptions, server *server.ProxyServer) (StopFunc, error) {
	if o.UdsName != "" {
		return p.runUDSFrontendServer(ctx, o, server)
	}
	return p.runMTLSFrontendServer(ctx, o, server)
}

func (p *Proxy) runUDSFrontendServer(ctx context.Context, o *options.ProxyRunOptions, s *server.ProxyServer) (StopFunc, error) {
	if o.DeleteUDSFile {
		if err := os.Remove(o.UdsName); err != nil && !os.IsNotExist(err) {
			klog.ErrorS(err, "failed to delete file", "file", o.UdsName)
		}
	}
	var stop StopFunc
	if o.Mode == "grpc" {
		frontendServerOptions := []grpc.ServerOption{
			grpc.KeepaliveParams(keepalive.ServerParameters{Time: o.FrontendKeepaliveTime}),
		}
		grpcServer := grpc.NewServer(frontendServerOptions...)
		client.RegisterProxyServiceServer(grpcServer, s)
		lis, err := getUDSListener(ctx, o.UdsName)
		if err != nil {
			return nil, fmt.Errorf("failed to get uds listener: %v", err)
		}
		labels := runpprof.Labels(
			"core", "udsGrpcFrontend",
			"udsFile", o.UdsName,
		)
		go runpprof.Do(context.Background(), labels, func(context.Context) { grpcServer.Serve(lis) })
		stop = grpcServer.GracefulStop
	} else {
		// http-connect
		server := &http.Server{
			ReadHeaderTimeout: ReadHeaderTimeout,
			Handler: &server.Tunnel{
				Server: s,
			},
		}
		stop = func() {
			err := server.Shutdown(ctx)
			klog.ErrorS(err, "error shutting down server")
		}
		labels := runpprof.Labels(
			"core", "udsHttpFrontend",
			"udsFile", o.UdsName,
		)
		go runpprof.Do(context.Background(), labels, func(context.Context) {
			udsListener, err := getUDSListener(ctx, o.UdsName)
			if err != nil {
				klog.ErrorS(err, "failed to get uds listener")
			}
			defer func() {
				err := udsListener.Close()
				klog.ErrorS(err, "failed to close uds listener")
			}()
			err = server.Serve(udsListener)
			if err != nil {
				klog.ErrorS(err, "failed to serve uds requests")
			}
		})
	}

	return stop, nil
}

func (p *Proxy) getTLSConfig(caFile, certFile, keyFile, cipherSuites string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load X509 key pair %s and %s: %v", certFile, keyFile, err)
	}

	cipherSuiteIDs := tlsCipherSuites(strings.Split(cipherSuites, ","))

	if caFile == "" {
		return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, CipherSuites: cipherSuiteIDs}, nil
	}

	certPool := x509.NewCertPool()
	caCert, err := ioutil.ReadFile(filepath.Clean(caFile))
	if err != nil {
		return nil, fmt.Errorf("failed to read cluster CA cert %s: %v", caFile, err)
	}
	ok := certPool.AppendCertsFromPEM(caCert)
	if !ok {
		return nil, fmt.Errorf("failed to append cluster CA cert to the cert pool")
	}

	tlsConfig := &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    certPool,
		MinVersion:   tls.VersionTLS12,
		CipherSuites: cipherSuiteIDs,
	}

	return tlsConfig, nil
}

func (p *Proxy) runMTLSFrontendServer(ctx context.Context, o *options.ProxyRunOptions, s *server.ProxyServer) (StopFunc, error) {
	var stop StopFunc

	var tlsConfig *tls.Config
	var err error
	if tlsConfig, err = p.getTLSConfig(o.ServerCaCert, o.ServerCert, o.ServerKey, o.CipherSuites); err != nil {
		return nil, err
	}

	addr := net.JoinHostPort(o.ServerBindAddress, strconv.Itoa(o.ServerPort))

	if o.Mode == "grpc" {
		frontendServerOptions := []grpc.ServerOption{
			grpc.Creds(credentials.NewTLS(tlsConfig)),
			grpc.KeepaliveParams(keepalive.ServerParameters{Time: o.FrontendKeepaliveTime}),
		}
		grpcServer := grpc.NewServer(frontendServerOptions...)
		client.RegisterProxyServiceServer(grpcServer, s)
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("failed to listen on %s: %v", addr, err)
		}
		labels := runpprof.Labels(
			"core", "mtlsGrpcFrontend",
			"port", strconv.FormatUint(uint64(o.ServerPort), 10),
		)
		go runpprof.Do(context.Background(), labels, func(context.Context) { grpcServer.Serve(lis) })
		stop = grpcServer.GracefulStop
	} else {
		// http-connect
		server := &http.Server{
			ReadHeaderTimeout: ReadHeaderTimeout,
			Addr:              addr,
			TLSConfig:         tlsConfig,
			Handler: &server.Tunnel{
				Server: s,
			},
			TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
		}
		stop = func() {
			err := server.Shutdown(ctx)
			if err != nil {
				klog.ErrorS(err, "failed to shutdown server")
			}
		}
		labels := runpprof.Labels(
			"core", "mtlsHttpFrontend",
			"port", strconv.FormatUint(uint64(o.ServerPort), 10),
		)
		go runpprof.Do(context.Background(), labels, func(context.Context) {
			err := server.ListenAndServeTLS("", "") // empty files defaults to tlsConfig
			if err != nil {
				klog.ErrorS(err, "failed to listen on frontend port")
			}
		})
	}

	return stop, nil
}

func (p *Proxy) runAgentServer(o *options.ProxyRunOptions, server *server.ProxyServer) error {
	var tlsConfig *tls.Config
	var err error
	if tlsConfig, err = p.getTLSConfig(o.ClusterCaCert, o.ClusterCert, o.ClusterKey, o.CipherSuites); err != nil {
		return err
	}

	addr := net.JoinHostPort(o.AgentBindAddress, strconv.Itoa(o.AgentPort))
	agentServerOptions := []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: o.KeepaliveTime}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
	}
	grpcServer := grpc.NewServer(agentServerOptions...)
	agent.RegisterAgentServiceServer(grpcServer, server)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %v", addr, err)
	}
	labels := runpprof.Labels(
		"core", "agentListener",
		"port", strconv.FormatUint(uint64(o.AgentPort), 10),
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) { grpcServer.Serve(lis) })

	return nil
}

func (p *Proxy) runAdminServer(o *options.ProxyRunOptions, server *server.ProxyServer) error {
	muxHandler := http.NewServeMux()
	muxHandler.Handle("/metrics", promhttp.Handler())
	if o.EnableProfiling {
		muxHandler.HandleFunc("/debug/pprof", util.RedirectTo("/debug/pprof/"))
		muxHandler.HandleFunc("/debug/pprof/", netpprof.Index)
		if o.EnableContentionProfiling {
			runtime.SetBlockProfileRate(1)
		}
	}
	adminServer := &http.Server{
		Addr:           net.JoinHostPort(o.AdminBindAddress, strconv.Itoa(o.AdminPort)),
		Handler:        muxHandler,
		MaxHeaderBytes: 1 << 20,
	}

	labels := runpprof.Labels(
		"core", "adminListener",
		"port", strconv.FormatUint(uint64(o.AdminPort), 10),
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) {
		err := adminServer.ListenAndServe()
		if err != nil {
			klog.ErrorS(err, "admin server could not listen")
		}
		klog.V(1).Infoln("Admin server stopped listening")
	})

	return nil
}

func (p *Proxy) runHealthServer(o *options.ProxyRunOptions, server *server.ProxyServer) error {
	livenessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok")
	})
	readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ready, msg := server.Readiness.Ready()
		if ready {
			w.WriteHeader(200)
			fmt.Fprintf(w, "ok")
			return
		}
		w.WriteHeader(500)
		fmt.Fprintf(w, msg)
	})

	muxHandler := http.NewServeMux()
	muxHandler.HandleFunc("/healthz", livenessHandler)
	// "/ready" is deprecated but being maintained for backward compatibility
	muxHandler.HandleFunc("/ready", readinessHandler)
	muxHandler.HandleFunc("/readyz", readinessHandler)
	healthServer := &http.Server{
		Addr:              net.JoinHostPort(o.HealthBindAddress, strconv.Itoa(o.HealthPort)),
		Handler:           muxHandler,
		MaxHeaderBytes:    1 << 20,
		ReadHeaderTimeout: ReadHeaderTimeout,
	}

	labels := runpprof.Labels(
		"core", "healthListener",
		"port", strconv.FormatUint(uint64(o.HealthPort), 10),
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) {
		err := healthServer.ListenAndServe()
		if err != nil {
			klog.ErrorS(err, "health server could not listen")
		}
		klog.V(1).Infoln("Health server stopped listening")
	})

	return nil
}
