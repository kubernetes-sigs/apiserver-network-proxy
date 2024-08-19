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
package agent

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent/metrics"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"

	coordinationv1api "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1lister "k8s.io/client-go/listers/coordination/v1"
)

// A ServerLeaseCounter counts leases in the k8s apiserver to determine the
// current proxy server count.
type ServerLeaseCounter struct {
	leaseLister   coordinationv1lister.LeaseLister
	selector      labels.Selector
	fallbackCount int
	pc            clock.PassiveClock
}

// NewServerLeaseCounter creates a server counter that counts valid leases that match the label
// selector and provides the fallback count (initially 0) if this fails.
func NewServerLeaseCounter(pc clock.PassiveClock, leaseLister coordinationv1lister.LeaseLister, labelSelector labels.Selector, leaseNamespace string) *ServerLeaseCounter {
	return &ServerLeaseCounter{
		leaseLister:   leaseLister,
		selector:      labelSelector,
		fallbackCount: 0,
		pc:            pc,
	}
}

// Count counts the number of leases in the apiserver matching the provided
// label selector.
//
// In the event that no valid leases are found or lease listing fails, the
// fallback count is returned. This fallback count is updated upon successful
// discovery of valid leases.
func (lc *ServerLeaseCounter) Count(ctx context.Context) int {
	// Since the number of proxy servers is generally small (1-10), we opted against
	// using a LIST and WATCH pattern and instead list all leases on each call.
	// TODO: Switch to an informer-based system and use events rather than a polling loop.
	start := time.Now()
	defer func() {
		latency := time.Now().Sub(start)
		metrics.Metrics.ObserveLeaseListLatency(latency)
	}()
	leases, err := lc.leaseLister.List(lc.selector)
	if err != nil {
		klog.Errorf("Could not list leases to update server count, using fallback count (%v): %v", lc.fallbackCount, err)

		return lc.fallbackCount
	}

	metrics.Metrics.ObserveLeaseList(200, "")

	count := 0
	for _, lease := range leases {
		if util.IsLeaseValid(lc.pc, *lease) {
			count++
		}
	}

	// Ensure returned count is always at least 1.
	if count == 0 {
		klog.Warningf("No valid leases found, assuming server count of 1")
		count = 1
	}

	if count != lc.fallbackCount {
		lc.fallbackCount = count
	}

	return count
}

func NewLeaseInformerWithMetrics(client kubernetes.Interface, namespace string, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				obj, err := client.CoordinationV1().Leases(namespace).List(context.TODO(), options)
				if err != nil {
					klog.Errorf("Could not list leases: %v", err)

					var apiStatus apierrors.APIStatus
					if errors.As(err, &apiStatus) {
						status := apiStatus.Status()
						metrics.Metrics.ObserveLeaseList(int(status.Code), string(status.Reason))
					} else {
						klog.Errorf("Lease list error could not be logged to metrics as it is not an APIStatus: %v", err)
					}
					return nil, err
				}

				metrics.Metrics.ObserveLeaseList(200, "")
				return obj, nil
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				obj, err := client.CoordinationV1().Leases(namespace).Watch(context.TODO(), options)
				if err != nil {
					klog.Errorf("Could not watch leases: %v", err)

					var apiStatus apierrors.APIStatus
					if errors.As(err, &apiStatus) {
						status := apiStatus.Status()
						metrics.Metrics.ObserveLeaseWatch(int(status.Code), string(status.Reason))
					} else {
						klog.Errorf("Lease watch error could not be logged to metrics as it is not an APIStatus: %v", err)
					}
					return nil, err
				}

				metrics.Metrics.ObserveLeaseWatch(200, "")
				return obj, nil
			},
		},
		&coordinationv1api.Lease{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
}
