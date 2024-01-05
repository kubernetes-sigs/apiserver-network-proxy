/*
Copyright 2023 The Kubernetes Authors.

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

package framework

import (
	"fmt"
	"net/http"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

func readIntGauge(metricsURL, metricName string) (int, error) {
	metrics, err := scrapeMetrics(metricsURL)
	if err != nil {
		return 0, err
	}

	metric, found := metrics[metricName]
	if !found {
		return 0, fmt.Errorf("missing %s metric", metricName)
	}

	vals := metric.GetMetric()
	if len(vals) == 0 {
		return 0, fmt.Errorf("missing value for %s metric", metricName)
	}

	return int(vals[0].GetGauge().GetValue()), nil
}

func scrapeMetrics(metricsURL string) (map[string]*dto.MetricFamily, error) {
	resp, err := http.Get(metricsURL)
	if err != nil {
		return nil, fmt.Errorf("failed to scrape metrics from %s: %w", metricsURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected %q status scraping metrics from %s", resp.Status, metricsURL)
	}

	var parser expfmt.TextParser
	mf, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse metrics: %w", err)
	}
	return mf, nil
}
