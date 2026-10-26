/*
Copyright 2026 The Kubernetes Authors.

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

// Command test-webhook is an always-allow validating admission webhook that
// measures its own request handling and connection churn, so that the
// kube-apiserver's view of the same calls can be compared against it.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	durationBuckets = []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

	handlerDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "knp_webhook",
		Name:      "handler_duration_seconds",
		Help:      "Time from handler entry (headers parsed) to response written, body read included.",
		Buckets:   durationBuckets,
	})
	bodyReadDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "knp_webhook",
		Name:      "body_read_duration_seconds",
		Help:      "Time spent reading the AdmissionReview body.",
		Buckets:   durationBuckets,
	})
	requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "knp_webhook",
		Name:      "requests_total",
		Help:      "Admission requests by result.",
	}, []string{"result"})
	connsAccepted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "knp_webhook",
		Name:      "connections_accepted_total",
		Help:      "TCP connections accepted on the webhook port.",
	})
	connsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "knp_webhook",
		Name:      "connections_active",
		Help:      "TCP connections currently open on the webhook port.",
	})
)

type reviewResponse struct {
	UID     string `json:"uid"`
	Allowed bool   `json:"allowed"`
}

type review struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Request    *struct {
		UID string `json:"uid"`
	} `json:"request,omitempty"`
	Response *reviewResponse `json:"response,omitempty"`
}

func validate(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() { handlerDuration.Observe(time.Since(start).Seconds()) }()

	body, err := io.ReadAll(r.Body)
	bodyReadDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		requests.WithLabelValues("body_error").Inc()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var in review
	if err := json.Unmarshal(body, &in); err != nil || in.Request == nil {
		requests.WithLabelValues("decode_error").Inc()
		http.Error(w, "bad AdmissionReview", http.StatusBadRequest)
		return
	}
	out := review{
		APIVersion: in.APIVersion,
		Kind:       in.Kind,
		Response:   &reviewResponse{UID: in.Request.UID, Allowed: true},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		requests.WithLabelValues("write_error").Inc()
		return
	}
	requests.WithLabelValues("allowed").Inc()
}

func main() {
	addr := flag.String("addr", ":8443", "webhook listen address (TLS)")
	metricsAddr := flag.String("metrics-addr", ":9090", "metrics listen address (plain HTTP)")
	cert := flag.String("tls-cert", "/certs/tls.crt", "TLS certificate")
	key := flag.String("tls-key", "/certs/tls.key", "TLS key")
	flag.Parse()

	prometheus.MustRegister(handlerDuration, bodyReadDuration, requests, connsAccepted, connsActive)

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		metricsSrv := &http.Server{Addr: *metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		log.Fatal(metricsSrv.ListenAndServe())
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/validate", validate)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ConnState: func(_ net.Conn, s http.ConnState) {
			switch s {
			case http.StateNew:
				connsAccepted.Inc()
				connsActive.Inc()
			case http.StateClosed, http.StateHijacked:
				connsActive.Dec()
			}
		},
	}
	log.Printf("webhook listening on %s, metrics on %s", *addr, *metricsAddr)
	log.Fatal(srv.ListenAndServeTLS(*cert, *key))
}
