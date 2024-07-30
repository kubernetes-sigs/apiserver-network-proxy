package agent

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	coordinationv1api "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
)

var timeNow = time.Now

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
	leases, err := lc.leaseClient.List(ctx, metav1.ListOptions{LabelSelector: lc.selector.String()})
	if err != nil {
		klog.Errorf("could not list leases to update server count, using fallback count (%v): %v", lc.fallbackCount, err)
		return lc.fallbackCount
	}

	count := 0
	for _, lease := range leases.Items {
		if isLeaseValid(lease) {
			count++
		} else {
		}
	}

	if count == 0 {
		return lc.fallbackCount
	}

	if count != lc.fallbackCount {
		lc.fallbackCount = count
	}

	return count
}

func isLeaseValid(lease coordinationv1api.Lease) bool {
	var lastRenewTime time.Time
	if lease.Spec.RenewTime != nil {
		lastRenewTime = lease.Spec.RenewTime.Time
	} else if lease.Spec.AcquireTime != nil {
		lastRenewTime = lease.Spec.AcquireTime.Time
	} else {
		klog.Warningf("lease %v has neither a renew time or an acquire time, marking as expired: %v", lease.Name, lease)
		return false
	}

	duration := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second

	return lastRenewTime.Add(duration).After(timeNow()) // renewTime+duration > time.Now()
}
