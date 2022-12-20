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

package testing

import (
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client/metrics"
)

const (
	clientDialFailureHeader = `
# HELP konnectivity_network_proxy_client_dial_failure_total Number of dial failures observed, by reason (example: remote endpoint error)
# TYPE konnectivity_network_proxy_client_dial_failure_total counter`
	clientDialFailureSample = `konnectivity_network_proxy_client_dial_failure_total{reason="%s"} %d`

	clientConnsHeader = `
# HELP konnectivity_network_proxy_client_client_connections Number of open client connections, by status (Example: dialing)
# TYPE konnectivity_network_proxy_client_client_connections gauge`
	clientConnsSample = `konnectivity_network_proxy_client_client_connections{status="%s"} %d`
)

func ExpectClientDialFailures(expected map[metrics.DialFailureReason]int) error {
	expect := clientDialFailureHeader + "\n"
	for r, v := range expected {
		expect += fmt.Sprintf(clientDialFailureSample+"\n", r, v)
	}
	return ExpectMetric(metrics.Namespace, metrics.Subsystem, "dial_failure_total", expect)
}

func ExpectClientDialFailure(reason metrics.DialFailureReason, count int) error {
	return ExpectClientDialFailures(map[metrics.DialFailureReason]int{reason: count})
}

func ExpectClientConnections(expected map[metrics.ClientConnectionStatus]int) error {
	expect := clientConnsHeader + "\n"
	for r, v := range expected {
		expect += fmt.Sprintf(clientConnsSample+"\n", r, v)
	}
	return ExpectMetric(metrics.Namespace, metrics.Subsystem, "client_connections", expect)
}

func ExpectClientConnection(status metrics.ClientConnectionStatus, count int) error {
	return ExpectClientConnections(map[metrics.ClientConnectionStatus]int{status: count})
}

func ExpectMetric(namespace, subsystem, name, expected string) error {
	fqName := prometheus.BuildFQName(namespace, subsystem, name)
	return promtest.GatherAndCompare(prometheus.DefaultGatherer, strings.NewReader(expected), fqName)
}
