/*
Copyright 2024 The Kubernetes Authors.

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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestServerCountMetric tests the server count metric.
func TestServerCountMetric(t *testing.T) {
	// Reset metrics to ensure clean state
	Metrics.Reset()

	// Test setting server count
	expectedCount := 3
	Metrics.SetServerCount(expectedCount)

	// Verify the metric value
	actualCount := testutil.ToFloat64(Metrics.serverCount)
	if actualCount != float64(expectedCount) {
		t.Errorf("Expected server count %d, got %f", expectedCount, actualCount)
	}

	// Test updating server count
	newCount := 5
	Metrics.SetServerCount(newCount)
	actualCount = testutil.ToFloat64(Metrics.serverCount)
	if actualCount != float64(newCount) {
		t.Errorf("Expected updated server count %d, got %f", newCount, actualCount)
	}
}

// TestServerCountMetricRegistration tests the registration of the server count metric.
func TestServerCountMetricRegistration(t *testing.T) {
	// Verify the metric is properly registered
	registry := prometheus.NewRegistry()
	registry.MustRegister(Metrics.serverCount)

	// Set a value and check if it's accessible
	Metrics.SetServerCount(2)
	actualCount := testutil.ToFloat64(Metrics.serverCount)
	if actualCount != 2.0 {
		t.Errorf("Expected server count 2, got %f", actualCount)
	}
}
