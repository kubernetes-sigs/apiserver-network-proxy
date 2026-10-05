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

// Command webhook-load creates ConfigMaps with dryRun=All so that the
// validating webhook is invoked without persisting anything. It prints a
// per-interval summary of client-observed latency and response codes.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

type sample struct {
	d    time.Duration
	code int
}

func main() {
	server := flag.String("server", "https://127.0.0.1:6443", "apiserver URL")
	ca := flag.String("ca", "", "CA cert file")
	cert := flag.String("cert", "", "client cert file")
	key := flag.String("key", "", "client key file")
	ns := flag.String("namespace", "webhook-load", "namespace for dry-run ConfigMaps")
	workers := flag.Int("workers", 4, "concurrent workers")
	rate := flag.Float64("rate", 0, "requests per second per worker (0 = unlimited)")
	interval := flag.Duration("interval", 5*time.Second, "summary interval")
	flag.Parse()

	caPEM, err := os.ReadFile(*ca)
	if err != nil {
		log.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	pair, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12},
			MaxIdleConnsPerHost: *workers,
		},
	}
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/configmaps?dryRun=All", *server, *ns)

	var mu sync.Mutex
	var samples []sample
	for i := 0; i < *workers; i++ {
		go func(i int) {
			var tick <-chan time.Time
			if *rate > 0 {
				ticker := time.NewTicker(time.Duration(float64(time.Second) / *rate))
				defer ticker.Stop()
				tick = ticker.C
			}
			n := 0
			for {
				if tick != nil {
					<-tick
				}
				n++
				body := fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"load-%d-%d"},"data":{"k":"v"}}`, i, n)
				start := time.Now()
				resp, err := client.Post(url, "application/json", bytes.NewReader([]byte(body)))
				code := 0
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					code = resp.StatusCode
				}
				mu.Lock()
				samples = append(samples, sample{time.Since(start), code})
				mu.Unlock()
			}
		}(i)
	}

	report := time.NewTicker(*interval)
	defer report.Stop()
	for range report.C {
		mu.Lock()
		s := samples
		samples = nil
		mu.Unlock()
		ts := time.Now().UTC().Format("15:04:05")
		if len(s) == 0 {
			fmt.Printf("%s n=0\n", ts)
			continue
		}
		sort.Slice(s, func(i, j int) bool { return s[i].d < s[j].d })
		codes := map[int]int{}
		var sum time.Duration
		for _, x := range s {
			codes[x.code]++
			sum += x.d
		}
		q := func(p float64) time.Duration { return s[int(float64(len(s)-1)*p)].d }
		fmt.Printf("%s n=%d rps=%.0f mean=%s p50=%s p99=%s max=%s codes=%v\n",
			ts, len(s), float64(len(s))/interval.Seconds(),
			sum/time.Duration(len(s)), q(.5), q(.99), s[len(s)-1].d, codes)
	}
}
