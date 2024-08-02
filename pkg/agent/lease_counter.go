package agent

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent/metrics"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
)

// A ServerLeaseCounter counts leases in the k8s apiserver to determine the
// current proxy server count.
type ServerLeaseCounter struct {
	leaseClient   coordinationv1.LeaseInterface
	selector      labels.Selector
	fallbackCount int
}

// NewServerLeaseCounter creates a server counter that counts valid leases that match the label
// selector and provides the fallback count (initially 0) if this fails.
func NewServerLeaseCounter(k8sClient kubernetes.Interface, labelSelector labels.Selector) *ServerLeaseCounter {
	return &ServerLeaseCounter{
		leaseClient:   k8sClient.CoordinationV1().Leases(""),
		selector:      labelSelector,
		fallbackCount: 0,
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
	start := time.Now()
	defer func() {
		latency := time.Now().Sub(start)
		metrics.Metrics.ObserveLeaseListLatency(latency)
	}()
	leases, err := lc.leaseClient.List(ctx, metav1.ListOptions{LabelSelector: lc.selector.String()})
	if err != nil {
		klog.Errorf("could not list leases to update server count, using fallback count (%v): %v", lc.fallbackCount, err)

		var apiStatus apierrors.APIStatus
		if errors.As(err, &apiStatus) {
			status := apiStatus.Status()
			metrics.Metrics.ObserveLeaseList(int(status.Code), string(status.Reason))
		} else {
			klog.Errorf("error could not be logged to metrics as it is not an APIStatus: %v", err)
		}

		return lc.fallbackCount
	}

	metrics.Metrics.ObserveLeaseList(200, "")

	count := 0
	for _, lease := range leases.Items {
		if util.IsLeaseValid(lease) {
			count++
		} else {
		}
	}

	if count != lc.fallbackCount {
		lc.fallbackCount = count
	}

	return count
}
