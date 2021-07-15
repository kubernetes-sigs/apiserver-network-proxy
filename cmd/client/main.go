/*
Copyright 2019 The Kubernetes Authors.

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

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"golang.org/x/net/http2"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/klog/v2"

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

func main() {
	client := &Client{}
	o := newGrpcProxyClientOptions()
	command := newGrpcProxyClientCommand(client, o)
	flags := command.Flags()
	flags.AddFlagSet(o.Flags())
	local := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	klog.InitFlags(local)
	err := local.Set("v", "4")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error setting klog flags: %v", err)
	}
	local.VisitAll(func(fl *flag.Flag) {
		fl.Name = util.Normalize(fl.Name)
		flags.AddGoFlag(fl)
	})
	if err := command.Execute(); err != nil {
		klog.Errorf("error: %v\n", err)
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}

type GrpcProxyClientOptions struct {
	clientCert              string
	clientKey               string
	caCert                  string
	requestProto            string
	requestPath             string
	requestHost             string
	requestPort             int
	proxyHost               string
	proxyPort               int
	proxyUdsName            string
	mode                    string
	userAgent               string
	testRequests            int
	testDelaySec            int
	afterDelaySec           int
	closeIdleConn           bool
	enableHTTP2HealthChecks bool
}

func (o *GrpcProxyClientOptions) Flags() *pflag.FlagSet {
	flags := pflag.NewFlagSet("proxy", pflag.ContinueOnError)
	flags.StringVar(&o.clientCert, "client-cert", o.clientCert, "If non-empty secure communication with this cert.")
	flags.StringVar(&o.clientKey, "client-key", o.clientKey, "If non-empty secure communication with this key.")
	flags.StringVar(&o.caCert, "ca-cert", o.caCert, "If non-empty the CAs we use to validate clients.")
	flags.StringVar(&o.requestProto, "request-proto", o.requestProto, "The protocol for the request to send through the proxy.")
	flags.StringVar(&o.requestPath, "request-path", o.requestPath, "The url request to send through the proxy.")
	flags.StringVar(&o.requestHost, "request-host", o.requestHost, "The host of the request server.")
	flags.IntVar(&o.requestPort, "request-port", o.requestPort, "The port the request server is listening on.")
	flags.StringVar(&o.proxyHost, "proxy-host", o.proxyHost, "The host of the proxy server.")
	flags.IntVar(&o.proxyPort, "proxy-port", o.proxyPort, "The port the proxy server is listening on.")
	flags.StringVar(&o.proxyUdsName, "proxy-uds", o.proxyUdsName, "The UDS name to connect to.")
	flags.StringVar(&o.mode, "mode", o.mode, "Mode can be either 'grpc' or 'http-connect'.")
	flags.StringVar(&o.userAgent, "user-agent", o.userAgent, "User agent to pass to the proxy server")
	flags.IntVar(&o.testRequests, "test-requests", o.testRequests, "The number of times to send the request.")
	flags.IntVar(&o.testDelaySec, "test-delay", o.testDelaySec, "The delay in seconds between sending requests.")
	flags.IntVar(&o.afterDelaySec, "after-delay", o.afterDelaySec, "The delay in seconds after sending requests.")
	flags.BoolVar(&o.closeIdleConn, "close-idle-conn", o.closeIdleConn, "Controls if the client calls closeIdleConnections.")
	flags.BoolVar(&o.enableHTTP2HealthChecks, "enable-http2-healthchecks", o.enableHTTP2HealthChecks, "enables health checks on http2 connections made by the client.")

	return flags
}

func (o *GrpcProxyClientOptions) Print() {
	klog.V(1).Infof("ClientCert set to %q.\n", o.clientCert)
	klog.V(1).Infof("ClientKey set to %q.\n", o.clientKey)
	klog.V(1).Infof("CACert set to %q.\n", o.caCert)
	klog.V(1).Infof("RequestProto set to %q.\n", o.requestProto)
	klog.V(1).Infof("RequestPath set to %q.\n", o.requestPath)
	klog.V(1).Infof("RequestHost set to %q.\n", o.requestHost)
	klog.V(1).Infof("RequestPort set to %d.\n", o.requestPort)
	klog.V(1).Infof("ProxyHost set to %q.\n", o.proxyHost)
	klog.V(1).Infof("ProxyPort set to %d.\n", o.proxyPort)
	klog.V(1).Infof("ProxyUdsName set to %q.\n", o.proxyUdsName)
	klog.V(1).Infof("TestRequests set to %q.\n", o.testRequests)
	klog.V(1).Infof("TestDelaySec set to %d.\n", o.testDelaySec)
	klog.V(1).Infof("AfterDelaySec set to %d.\n", o.afterDelaySec)
	klog.V(1).Infof("CloseIdleConn set to %t.\n", o.closeIdleConn)
}

func (o *GrpcProxyClientOptions) Validate() error {
	if o.clientKey != "" {
		if _, err := os.Stat(o.clientKey); os.IsNotExist(err) {
			return err
		}
		if o.clientCert == "" {
			return fmt.Errorf("cannot have client cert empty when client key is set to %q", o.clientKey)
		}
	}
	if o.clientCert != "" {
		if _, err := os.Stat(o.clientCert); os.IsNotExist(err) {
			return err
		}
		if o.clientKey == "" {
			return fmt.Errorf("cannot have client key empty when client cert is set to %q", o.clientCert)
		}
	}
	if o.caCert != "" {
		if _, err := os.Stat(o.caCert); os.IsNotExist(err) {
			return err
		}
	}
	if o.requestProto != "http" && o.requestProto != "https" {
		return fmt.Errorf("request protocol must be set to either 'http' or 'https' not %q", o.requestProto)
	}
	if o.mode != "grpc" && o.mode != "http-connect" {
		return fmt.Errorf("mode must be set to either 'grpc' or 'http-connect' not %q", o.mode)
	}
	if o.requestPort > 49151 {
		return fmt.Errorf("please do not try to use ephemeral port %d for the request server port", o.requestPort)
	}
	if o.proxyPort > 49151 {
		return fmt.Errorf("please do not try to use ephemeral port %d for the proxy server port", o.proxyPort)
	}
	if o.proxyPort < 1024 && o.proxyUdsName == "" {
		return fmt.Errorf("please do not try to use reserved port %d for the proxy server port", o.proxyPort)
	}
	if o.proxyUdsName != "" {
		if o.proxyHost != "" {
			return fmt.Errorf("please do set proxy host when using UDS")
		}
		if o.proxyPort != 0 {
			return fmt.Errorf("please do set proxy server port to 0 not %d when using UDS", o.proxyPort)
		}
		if o.clientKey != "" || o.clientCert != "" || o.caCert != "" {
			return fmt.Errorf("please do set cert materials when using UDS, key = %s, cert = %s, CA = %s",
				o.clientKey, o.clientCert, o.caCert)
		}
	}
	if o.testRequests < 1 {
		return fmt.Errorf("please do not ask for fewer than 1 test request(%d)", o.testRequests)
	}
	if o.testDelaySec < 0 {
		return fmt.Errorf("please do not ask for less than a 0 second delay(%d)", o.testDelaySec)
	}
	if o.afterDelaySec < 0 {
		return fmt.Errorf("please do not ask for less than a 0 second delay(%d)", o.afterDelaySec)
	}
	return nil
}

func newGrpcProxyClientOptions() *GrpcProxyClientOptions {
	o := GrpcProxyClientOptions{
		clientCert:              "",
		clientKey:               "",
		caCert:                  "",
		requestProto:            "http",
		requestPath:             "success",
		requestHost:             "localhost",
		requestPort:             8000,
		proxyHost:               "localhost",
		proxyPort:               8090,
		proxyUdsName:            "",
		mode:                    "grpc",
		userAgent:               "test-client",
		testRequests:            1,
		testDelaySec:            0,
		afterDelaySec:           0,
		closeIdleConn:           true,
		enableHTTP2HealthChecks: true,
	}
	return &o
}

func newGrpcProxyClientCommand(c *Client, o *GrpcProxyClientOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "proxy-client",
		Long: `A gRPC proxy Client, primarily used to test the Kubernetes gRPC Proxy Server.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
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
		return fmt.Errorf("failed to validate proxy client options, got %v", err)
	}

	// Run remote simple http service on server side as
	// "python -m SimpleHTTPServer"

	// The apiserver network proxy does not support reusing the same dialer for multiple connections.
	// With HTTP 1.1 Connection reuse, network proxy should support subsequent requests with the original connection
	// as long as the connection has not been closed. This means that this connection/client cannot be shared.
	// golang's default IdleConnTimeout is 90 seconds (https://golang.org/src/net/http/transport.go?s=13608:13676)
	// k/k abides by this default.
	//
	// When running the test-client with a delay parameter greater than 90s (even when no agent restart is done)
	// --test-requests=2 --test-delay=100 the second request will fail 100% of the time because currently network proxy
	// explicitly closes the tunnel when a close request is sent to to the inner TCP connection.
	// We do this as there is no way for us to know whether a tunnel will be reused.
	// So to prevent leaking too many tunnel connections,
	// we explicitly close the tunnel on the first CLOSE_RSP we obtain from the inner TCP connection
	// (https://github.com/kubernetes-sigs/apiserver-network-proxy/blob/master/konnectivity-client/pkg/client/client.go#L137).
	var wg sync.WaitGroup
	ch := make(chan error, o.testRequests)

	for i := 1; i <= o.testRequests; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			dialer, err := c.getDialer(o)
			if err != nil {
				ch <- fmt.Errorf("failed to get dialer for client, got %v", err)
				return
			}
			transport := &http.Transport{
				DialContext: dialer,
			}
			if o.enableHTTP2HealthChecks {
				err = configureHTTP2Transport(transport)
				if err != nil {
					klog.V(1).Error(err, "error initializing HTTP2 health checking parameters. Using default transport.")
				}
			}
			client := &http.Client{
				Transport: transport,
			}
			if o.closeIdleConn {
				defer client.CloseIdleConnections()
			}

			err = c.makeRequest(o, client)
			if err != nil {
				ch <- err
				return
			}

			if i != o.testRequests {
				klog.V(1).InfoS("Waiting for next connection test.", "seconds", o.testDelaySec, "iteration", i, "test requests", o.testRequests)
				wait := time.Duration(o.testDelaySec) * time.Second
				time.Sleep(wait)
			}
			ch <- nil
		}()
	}
	wg.Wait()
	close(ch)
	if o.afterDelaySec > 0 {
		wait := time.Duration(o.afterDelaySec) * time.Second
		time.Sleep(wait)
	}
	// collect up and return errors
	errs := []error{}
	for err := range ch {
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 1 {
		klog.Errorf("Failed request number %d", len(errs))
		return fmt.Errorf("Requests failed: %d", len(errs))
	} else if len(errs) == 1 {
		klog.Errorf("Failed request %v", errs[0])
		return errs[0]
	}

	return nil
}

func readIdleTimeoutSeconds() int {
	ret := 30
	// User can set the readIdleTimeout to 0 to disable the HTTP/2
	// connection health check.
	if s := os.Getenv("HTTP2_READ_IDLE_TIMEOUT_SECONDS"); len(s) > 0 {
		i, err := strconv.Atoi(s)
		if err != nil {
			klog.Warningf("Illegal HTTP2_READ_IDLE_TIMEOUT_SECONDS(%q): %v."+
				" Default value %d is used", s, err, ret)
			return ret
		}
		ret = i
	}
	return ret
}

func pingTimeoutSeconds() int {
	ret := 15
	if s := os.Getenv("HTTP2_PING_TIMEOUT_SECONDS"); len(s) > 0 {
		i, err := strconv.Atoi(s)
		if err != nil {
			klog.Warningf("Illegal HTTP2_PING_TIMEOUT_SECONDS(%q): %v."+
				" Default value %d is used", s, err, ret)
			return ret
		}
		ret = i
	}
	return ret
}

func configureHTTP2Transport(t *http.Transport) error {
	t2, err := http2.ConfigureTransports(t)
	if err != nil {
		return err
	}
	// The following enables the HTTP/2 connection health check added in
	// https://github.com/golang/net/pull/55. The health check detects and
	// closes broken transport layer connections. Without the health check,
	// a broken connection can linger too long, e.g., a broken TCP
	// connection will be closed by the Linux kernel after 13 to 30 minutes
	// by default, which caused
	// https://github.com/kubernetes/client-go/issues/374 and
	// https://github.com/kubernetes/kubernetes/issues/87615.
	t2.ReadIdleTimeout = time.Duration(readIdleTimeoutSeconds()) * time.Second
	t2.PingTimeout = time.Duration(pingTimeoutSeconds()) * time.Second
	return nil
}

func (c *Client) makeRequest(o *GrpcProxyClientOptions, client *http.Client) error {
	requestURL := fmt.Sprintf("%s://%s:%d/%s", o.requestProto, o.requestHost, o.requestPort, o.requestPath)
	request, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request %s to send, got %v", requestURL, err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("failed to send request to client, got %v", err)
	}
	defer func() {
		err := response.Body.Close()
		if err != nil {
			klog.Errorf("Failed to close connection, got %v", err)
		}
	}() // TODO: proxy server should handle the case where Body isn't closed.

	data, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("failed to read response from client, got %v", err)
	}
	klog.V(4).Infof("HTML Response:\n%s\n", string(data))
	return nil
}

func (c *Client) getDialer(o *GrpcProxyClientOptions) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	if o.proxyUdsName != "" {
		return c.getUDSDialer(o)
	}
	return c.getMTLSDialer(o)
}

func (c *Client) getUDSDialer(o *GrpcProxyClientOptions) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	ctx := context.Background()

	var proxyConn net.Conn
	var err error
	// Setup signal handler
	ch := make(chan os.Signal, 1)
	signal.Notify(ch)

	go func() {
		<-ch
		if proxyConn != nil {
			err := proxyConn.Close()
			klog.ErrorS(err, "connection closed")
		}
	}()

	switch o.mode {
	case "grpc":
		dialOption := grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			// Ignoring addr and timeout arguments:
			// addr - comes from the closure
			// timeout - is turned off as this is test code and eases debugging.
			c, err := net.DialTimeout("unix", o.proxyUdsName, 0)
			if err != nil {
				klog.ErrorS(err, "failed to create connection to uds", "name", o.proxyUdsName)
			}
			return c, err
		})
		tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, o.proxyUdsName, dialOption, grpc.WithInsecure(), grpc.WithUserAgent(o.userAgent))
		if err != nil {
			return nil, fmt.Errorf("failed to create tunnel %s, got %v", o.proxyUdsName, err)
		}

		requestAddress := fmt.Sprintf("%s:%d", o.requestHost, o.requestPort)
		proxyConn, err = tunnel.DialContext(ctx, "tcp", requestAddress)
		if err != nil {
			return nil, fmt.Errorf("failed to dial request %s, got %v", requestAddress, err)
		}
	case "http-connect":
		requestAddress := fmt.Sprintf("%s:%d", o.requestHost, o.requestPort)

		proxyConn, err = net.Dial("unix", o.proxyUdsName)
		if err != nil {
			return nil, fmt.Errorf("dialing proxy %q failed: %v", o.proxyUdsName, err)
		}
		fmt.Fprintf(proxyConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\n\r\n", requestAddress, "127.0.0.1", o.userAgent)
		br := bufio.NewReader(proxyConn)
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			return nil, fmt.Errorf("reading HTTP response from CONNECT to %s via uds proxy %s failed: %v",
				requestAddress, o.proxyUdsName, err)
		}
		if res.StatusCode != 200 {
			return nil, fmt.Errorf("proxy error from %s while dialing %s: %v", o.proxyUdsName, requestAddress, res.Status)
		}

		// It's safe to discard the bufio.Reader here and return the
		// original TCP conn directly because we only use this for
		// TLS, and in TLS the client speaks first, so we know there's
		// no unbuffered data. But we can double-check.
		if br.Buffered() > 0 {
			return nil, fmt.Errorf("unexpected %d bytes of buffered data from CONNECT uds proxy %q",
				br.Buffered(), o.proxyUdsName)
		}
	default:
		return nil, fmt.Errorf("failed to process mode %s", o.mode)
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return proxyConn, nil
	}, nil
}

func (c *Client) getMTLSDialer(o *GrpcProxyClientOptions) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	ctx := context.Background()
	tlsConfig, err := util.GetClientTLSConfig(o.caCert, o.clientCert, o.clientKey, o.proxyHost, nil)
	if err != nil {
		return nil, err
	}

	var proxyConn net.Conn

	// Setup signal handler
	ch := make(chan os.Signal, 1)
	signal.Notify(ch)

	go func() {
		<-ch
		if proxyConn != nil {
			err := proxyConn.Close()
			klog.ErrorS(err, "connection closed")
		}
	}()

	switch o.mode {
	case "grpc":
		transportCreds := credentials.NewTLS(tlsConfig)
		dialOption := grpc.WithTransportCredentials(transportCreds)
		serverAddress := fmt.Sprintf("%s:%d", o.proxyHost, o.proxyPort)
		tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, serverAddress, dialOption)
		if err != nil {
			return nil, fmt.Errorf("failed to create tunnel %s, got %v", serverAddress, err)
		}

		requestAddress := fmt.Sprintf("%s:%d", o.requestHost, o.requestPort)
		proxyConn, err = tunnel.DialContext(ctx, "tcp", requestAddress)
		if err != nil {
			return nil, fmt.Errorf("failed to dial request %s, got %v", requestAddress, err)
		}
	case "http-connect":
		proxyAddress := fmt.Sprintf("%s:%d", o.proxyHost, o.proxyPort)
		requestAddress := fmt.Sprintf("%s:%d", o.requestHost, o.requestPort)

		proxyConn, err = tls.Dial("tcp", proxyAddress, tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("dialing proxy %q failed: %v", proxyAddress, err)
		}
		fmt.Fprintf(proxyConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", requestAddress, "127.0.0.1")
		br := bufio.NewReader(proxyConn)
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			return nil, fmt.Errorf("reading HTTP response from CONNECT to %s via proxy %s failed: %v",
				requestAddress, proxyAddress, err)
		}
		if res.StatusCode != 200 {
			return nil, fmt.Errorf("proxy error from %s while dialing %s: %v", proxyAddress, requestAddress, res.Status)
		}

		// It's safe to discard the bufio.Reader here and return the
		// original TCP conn directly because we only use this for
		// TLS, and in TLS the client speaks first, so we know there's
		// no unbuffered data. But we can double-check.
		if br.Buffered() > 0 {
			return nil, fmt.Errorf("unexpected %d bytes of buffered data from CONNECT proxy %q",
				br.Buffered(), proxyAddress)
		}
	default:
		return nil, fmt.Errorf("failed to process mode %s", o.mode)
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return proxyConn, nil
	}, nil
}
