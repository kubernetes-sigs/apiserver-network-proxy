/*
Copyright 2020 The Kubernetes Authors.

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
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

func main() {
	testServer := &TestServer{}
	o := newTestServerRunOptions()
	command := newTestServerCommand(testServer, o)
	flags := command.Flags()
	flags.AddFlagSet(o.Flags())
	local := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	klog.InitFlags(local)
	local.VisitAll(func(fl *flag.Flag) {
		fl.Name = util.Normalize(fl.Name)
		flags.AddGoFlag(fl)
	})
	if err := command.Execute(); err != nil {
		klog.Errorf("error: %v\n", err)
		klog.Flush()
		os.Exit(1)
	}
}

type TestServerRunOptions struct {
	// Port we listen for server connections on.
	serverPort uint
}

func (o *TestServerRunOptions) Flags() *pflag.FlagSet {
	flags := pflag.NewFlagSet("proxy-server", pflag.ContinueOnError)
	flags.UintVar(&o.serverPort, "server-port", o.serverPort, "Port we listen for server connections on. Set to 0 for UDS.")
	return flags
}

func (o *TestServerRunOptions) Print() {
	klog.Warningf("Server port set to %d.\n", o.serverPort)
}

func (o *TestServerRunOptions) Validate() error {
	if o.serverPort > 49151 {
		return fmt.Errorf("please do not try to use ephemeral port %d for the server port", o.serverPort)
	}
	if o.serverPort < 1024 {
		return fmt.Errorf("please do not try to use reserved port %d for the server port", o.serverPort)
	}

	return nil
}

func newTestServerRunOptions() *TestServerRunOptions {
	o := TestServerRunOptions{
		serverPort: 8000,
	}
	return &o
}

func newTestServerCommand(p *TestServer, o *TestServerRunOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "test http server",
		Long: `A test http server, url determines behavior for certain tests.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return p.run(o)
		},
	}

	return cmd
}

type TestServer struct {
}

type StopFunc func()

func (p *TestServer) run(o *TestServerRunOptions) error {
	o.Print()
	if err := o.Validate(); err != nil {
		return fmt.Errorf("failed to validate server options with %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	klog.Info("Starting test http server for client requests.")
	testStop, err := p.runTestServer(ctx, o)
	if err != nil {
		return fmt.Errorf("failed to run the test server: %v", err)
	}

	stopCh := SetupSignalHandler()
	<-stopCh
	klog.Info("Shutting down server.")

	if testStop != nil {
		testStop()
	}

	return nil
}

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

func SetupSignalHandler() (stopCh <-chan struct{}) {
	stop := make(chan struct{})
	c := make(chan os.Signal, 2)
	signal.Notify(c, shutdownSignals...)
	go func() {
		<-c
		close(stop)
		<-c
		os.Exit(1) // second signal. Exit directly.
	}()

	return stop
}

func returnSuccess(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintf(w, "<!DOCTYPE html>\n<html>\n    <head>\n        <title>Success</title>\n    </head>\n    <body>\n        <p>The success test page!</p>\n    </body>\n</html>")
}

func returnError(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

func closeNoResponse(w http.ResponseWriter, _ *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "webserver doesn't support hijacking", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = conn.Close()
	if err != nil {
		klog.ErrorS(err, "failed to close connection")
	}
}

func sleepReturnSuccess(w http.ResponseWriter, req *http.Request) {
	sleepArr := req.URL.Query()["time"]
	sleepInt := 10
	var err error
	if len(sleepArr) == 1 {
		sleepStr := sleepArr[0]
		sleepInt, err = strconv.Atoi(sleepStr)
		if err != nil {
			sleepInt = 10
			klog.Warningf("Failed to parse sleep time (%s), default to %d, got error %v.", sleepStr, sleepInt, err)
		}
	} else {
		klog.Warningf("Sleep time(%v) was length %d defaulting to sleep %d.", sleepArr, len(sleepArr), sleepInt)
	}
	sleep := time.Duration(sleepInt) * time.Second
	time.Sleep(sleep)
	returnSuccess(w, req)
}

func (p *TestServer) runTestServer(ctx context.Context, o *TestServerRunOptions) (StopFunc, error) {
	muxHandler := http.NewServeMux()
	muxHandler.HandleFunc("/success", returnSuccess)
	muxHandler.HandleFunc("/sleep", sleepReturnSuccess)
	muxHandler.HandleFunc("/error", returnError)
	muxHandler.HandleFunc("/close", closeNoResponse)
	server := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", o.serverPort),
		Handler:           muxHandler,
		MaxHeaderBytes:    1 << 20,
		ReadHeaderTimeout: 60 * time.Second,
	}

	go func() {
		err := server.ListenAndServe()
		if err != nil {
			klog.Warningf("HTTP test server received %v.\n", err)
		}
		klog.Warningf("HTTP test server stopped listening\n")
	}()

	stop := func() {
		err := server.Shutdown(ctx)
		if err != nil {
			klog.ErrorS(err, "failed to shutdown server")
		}
	}
	return stop, nil
}
