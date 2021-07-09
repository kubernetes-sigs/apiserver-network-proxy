package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/klog/v2"

	"sigs.k8s.io/apiserver-network-proxy/cmd/agent/app/options"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent"
	"sigs.k8s.io/apiserver-network-proxy/pkg/features"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

func NewAgentCommand(a *Agent, o *options.GrpcProxyAgentOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "agent",
		Long: `A gRPC agent, Connects to the proxy and then allows traffic to be forwarded to it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.run(o)
		},
	}

	return cmd
}

type Agent struct {
}

func (a *Agent) run(o *options.GrpcProxyAgentOptions) error {
	o.Print()
	if err := o.Validate(); err != nil {
		return fmt.Errorf("failed to validate agent options with %v", err)
	}

	ctx := context.Background()
	var err error
	var cs *agent.ClientSet
	if cs, err = a.runProxyConnection(ctx, o); err != nil {
		return fmt.Errorf("failed to run proxy connection with %v", err)
	}

	if features.DefaultMutableFeatureGate.Enabled(features.NodeToMasterTraffic) {
		if err := a.runControlPlaneProxy(ctx, o, cs); err != nil {
			return fmt.Errorf("failed to start listening with %v", err)
		}
	}

	if err := a.runHealthServer(o); err != nil {
		return fmt.Errorf("failed to run health server with %v", err)
	}

	if err := a.runAdminServer(o); err != nil {
		return fmt.Errorf("failed to run admin server with %v", err)
	}

	<-ctx.Done()

	return nil
}

func (a *Agent) runProxyConnection(ctx context.Context, o *options.GrpcProxyAgentOptions) (*agent.ClientSet, error) {
	var tlsConfig *tls.Config
	var err error
	if tlsConfig, err = util.GetClientTLSConfig(o.CaCert, o.AgentCert, o.AgentKey, o.ProxyServerHost, o.AlpnProtos); err != nil {
		return nil, err
	}
	dialOption := grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))
	cc := o.ClientSetConfig(dialOption)
	cs := cc.NewAgentClientSet(ctx.Done())
	cs.Serve()

	return cs, nil
}

func (a *Agent) runControlPlaneProxy(ctx context.Context, o *options.GrpcProxyAgentOptions, cs *agent.ClientSet) error {
	pf := &agent.PortForwarder{
		ClientSet:  cs,
		ListenHost: o.BindAddress,
	}
	klog.V(1).Infof("Exposing apiserver: %s", &o.ApiServerMapping)
	return pf.Serve(ctx, agent.PortMapping(o.ApiServerMapping))
}

func (a *Agent) runHealthServer(o *options.GrpcProxyAgentOptions) error {
	livenessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok")
	})
	readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok")
	})

	muxHandler := http.NewServeMux()
	muxHandler.Handle("/metrics", promhttp.Handler())
	muxHandler.HandleFunc("/healthz", livenessHandler)
	// "/ready" is deprecated but being maintained for backward compatibility
	muxHandler.HandleFunc("/ready", readinessHandler)
	muxHandler.HandleFunc("/readyz", readinessHandler)
	healthServer := &http.Server{
		Addr:           fmt.Sprintf(":%d", o.HealthServerPort),
		Handler:        muxHandler,
		MaxHeaderBytes: 1 << 20,
	}

	go func() {
		err := healthServer.ListenAndServe()
		if err != nil {
			klog.ErrorS(err, "health server could not listen")
		}
		klog.V(0).Infoln("Health server stopped listening")
	}()

	return nil
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
		http.Redirect(w, r, fmt.Sprintf("%s:%d%s", host, o.HealthServerPort, r.URL.Path), http.StatusMovedPermanently)
	}))
	if o.EnableProfiling {
		muxHandler.HandleFunc("/debug/pprof", util.RedirectTo("/debug/pprof/"))
		muxHandler.HandleFunc("/debug/pprof/", pprof.Index)
		if o.EnableContentionProfiling {
			runtime.SetBlockProfileRate(1)
		}
	}

	adminServer := &http.Server{
		Addr:           fmt.Sprintf("127.0.0.1:%d", o.AdminServerPort),
		Handler:        muxHandler,
		MaxHeaderBytes: 1 << 20,
	}

	go func() {
		err := adminServer.ListenAndServe()
		if err != nil {
			klog.ErrorS(err, "admin server could not listen")
		}
		klog.V(0).Infoln("Admin server stopped listening")
	}()

	return nil
}
