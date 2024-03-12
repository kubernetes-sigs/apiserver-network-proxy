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
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	runpprof "runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"k8s.io/klog/v2"

	"sigs.k8s.io/apiserver-network-proxy/cmd/agent/app/options"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

const ReadHeaderTimeout = 60 * time.Second

func NewAgentCommand(a *Agent, o *options.GrpcProxyAgentOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "agent",
		Long: `A gRPC agent, Connects to the proxy and then allows traffic to be forwarded to it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			drainCh, stopCh := SetupSignalHandler()
			return a.Run(o, drainCh, stopCh)
		},
	}

	return cmd
}

type Agent struct {
	adminServer  *http.Server
	healthServer *http.Server

	cs *agent.ClientSet
}

func (a *Agent) Run(o *options.GrpcProxyAgentOptions, drainCh, stopCh <-chan struct{}) error {
	o.Print()
	if err := o.Validate(); err != nil {
		return fmt.Errorf("failed to validate agent options with %v", err)
	}

	cs, err := a.runProxyConnection(o, drainCh, stopCh)
	if err != nil {
		return fmt.Errorf("failed to run proxy connection with %v", err)
	}
	a.cs = cs

	if err := a.runHealthServer(o, cs); err != nil {
		return fmt.Errorf("failed to run health server with %v", err)
	}
	defer a.healthServer.Close()

	if err := a.runAdminServer(o); err != nil {
		return fmt.Errorf("failed to run admin server with %v", err)
	}
	defer a.adminServer.Close()

	<-stopCh
	klog.V(1).Infoln("Shutting down agent.")

	return nil
}

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

func SetupSignalHandler() (drainCh, stopCh <-chan struct{}) {
	drain := make(chan struct{})
	stop := make(chan struct{})
	c := make(chan os.Signal, 2)
	signal.Notify(c, shutdownSignals...)
	labels := runpprof.Labels(
		"core", "signalHandler",
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) { handleSignals(c, drain, stop) })

	return drain, stop
}

func handleSignals(signalCh chan os.Signal, drainCh, stopCh chan struct{}) {
	s := <-signalCh
	klog.V(2).InfoS("Received first signal", "signal", s)
	close(drainCh)
	s = <-signalCh
	klog.V(2).InfoS("Received second signal", "signal", s)
	close(stopCh)
}

func (a *Agent) runProxyConnection(o *options.GrpcProxyAgentOptions, drainCh, stopCh <-chan struct{}) (*agent.ClientSet, error) {
	var tlsConfig *tls.Config
	var err error
	if tlsConfig, err = util.GetClientTLSConfig(o.CaCert, o.AgentCert, o.AgentKey, o.ProxyServerHost, o.AlpnProtos); err != nil {
		return nil, err
	}
	dialOptions := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                o.KeepaliveTime,
			PermitWithoutStream: true,
		}),
	}
	cc := o.ClientSetConfig(dialOptions...)
	cs := cc.NewAgentClientSet(drainCh, stopCh)
	cs.Serve()

	return cs, nil
}

func (a *Agent) runHealthServer(o *options.GrpcProxyAgentOptions, cs agent.ReadinessManager) error {
	livenessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok")
	})

	checks := []agent.HealthChecker{agent.Ping, agent.NewServerConnected(cs)}
	readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var failedChecks []string
		var individualCheckOutput bytes.Buffer
		for _, check := range checks {
			if err := check.Check(r); err != nil {
				fmt.Fprintf(&individualCheckOutput, "[-]%s failed: %v\n", check.Name(), err)
				failedChecks = append(failedChecks, check.Name())
			} else {
				fmt.Fprintf(&individualCheckOutput, "[+]%s ok\n", check.Name())
			}
		}

		// Always be verbose if the check has failed
		if len(failedChecks) > 0 {
			klog.V(0).Infof("%s check failed: \n%v", strings.Join(failedChecks, ","), individualCheckOutput.String())
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, individualCheckOutput.String())
			return
		}

		if _, found := r.URL.Query()["verbose"]; !found {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "ok")
			return
		}

		fmt.Fprintf(&individualCheckOutput, "check passed\n")

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, individualCheckOutput.String())
	})

	muxHandler := http.NewServeMux()
	muxHandler.Handle("/metrics", promhttp.Handler())
	muxHandler.HandleFunc("/healthz", livenessHandler)
	// "/ready" is deprecated but being maintained for backward compatibility
	muxHandler.HandleFunc("/ready", readinessHandler)
	muxHandler.HandleFunc("/readyz", readinessHandler)
	a.healthServer = &http.Server{
		Addr:              net.JoinHostPort(o.HealthServerHost, strconv.Itoa(o.HealthServerPort)),
		Handler:           muxHandler,
		MaxHeaderBytes:    1 << 20,
		ReadHeaderTimeout: ReadHeaderTimeout,
	}

	labels := runpprof.Labels(
		"core", "healthListener",
		"port", strconv.Itoa(o.HealthServerPort),
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) { a.serveHealth(a.healthServer) })

	return nil
}

func (a *Agent) serveHealth(healthServer *http.Server) {
	err := healthServer.ListenAndServe()
	if err != nil {
		klog.ErrorS(err, "health server could not listen")
	}
	klog.V(0).Infoln("Health server stopped listening")
}

func (a *Agent) runAdminServer(o *options.GrpcProxyAgentOptions) error {
	muxHandler := http.NewServeMux()
	muxHandler.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		// The port number may be omitted if the admin server is running on port
		// 80, the default port for HTTP
		if err != nil {
			host = r.Host
		}
		dest := *r.URL
		dest.Host = net.JoinHostPort(host, strconv.Itoa(o.HealthServerPort))
		http.Redirect(w, r, dest.String(), http.StatusMovedPermanently)
	}))
	if o.EnableProfiling {
		muxHandler.HandleFunc("/debug/pprof", util.RedirectTo("/debug/pprof/"))
		muxHandler.HandleFunc("/debug/pprof/", pprof.Index)
		muxHandler.HandleFunc("/debug/pprof/profile", pprof.Profile)
		muxHandler.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		muxHandler.HandleFunc("/debug/pprof/trace", pprof.Trace)
		if o.EnableContentionProfiling {
			runtime.SetBlockProfileRate(1)
		}
	}

	a.adminServer = &http.Server{
		Addr:              net.JoinHostPort(o.AdminBindAddress, strconv.Itoa(o.AdminServerPort)),
		Handler:           muxHandler,
		MaxHeaderBytes:    1 << 20,
		ReadHeaderTimeout: ReadHeaderTimeout,
	}

	labels := runpprof.Labels(
		"core", "adminListener",
		"port", strconv.Itoa(o.AdminServerPort),
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) { a.serveAdmin(a.adminServer) })

	return nil
}

func (a *Agent) serveAdmin(adminServer *http.Server) {
	err := adminServer.ListenAndServe()
	if err != nil {
		klog.ErrorS(err, "admin server could not listen")
	}
	klog.V(0).Infoln("Admin server stopped listening")
}

// ClientSet exposes internal state for testing.
func (a *Agent) ClientSet() *agent.ClientSet {
	return a.cs
}
