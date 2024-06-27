package servercounter

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	coordinationv1api "k8s.io/api/coordination/v1"
	coordinationv1listers "k8s.io/client-go/listers/coordination/v1"
)

// A ServerLeaseCounter counts leases in the k8s apiserver to determine the
// current proxy server count.
type ServerLeaseCounter struct {
	lister        coordinationv1listers.LeaseLister
	selector      labels.Selector
	fallbackCount int
}

// NewServerLeaseCounter creates a server counter that counts valid leases that match the label
// selector and provides the fallback count if this fails.
func NewServerLeaseCounter(lister coordinationv1listers.LeaseLister, labelSelector string, fallbackCount int) (*ServerLeaseCounter, error) {
	selector, err := labels.Parse(labelSelector)
	if err != nil {
		return nil, fmt.Errorf("could not parse label selector %v: %w", labelSelector, err)
	}
	return &ServerLeaseCounter{
		lister:        lister,
		selector:      selector,
		fallbackCount: fallbackCount,
	}, nil
}

// CountServers counts the number of leases in the apiserver matching the provided
// label selector.
//
// In the event that no valid leases are found or lease listing fails, the
// fallback count is returned. This fallback count is updated upon successful
// discovery of valid leases.
func (lc *ServerLeaseCounter) CountServers() int {
	// Since the number of proxy servers is generally small (1-10), we opted against
	// using a LIST and WATCH pattern and instead list all leases in the informer.
	// The informer still uses LIST and WATCH under the hood, so this doesn't result
	// in additional calls in the apiserver, and checking whether a lease is valid
	// is cheap.
	leases, err := lc.lister.List(lc.selector)
	if err != nil {
		klog.Errorf("could not list leases to update server count, using fallback count (%v): %v", lc.fallbackCount, err)
		return lc.fallbackCount
	}

	count := 0
	for _, lease := range leases {
		if isLeaseValid(lease) {
			count++
		} else {
			klog.InfoS("excluding expired lease from server count", "selector", lc.selector, "lease", lease)
		}
	}

	if count == 0 {
		klog.Infof("no valid leases found, using fallback count (%v)", lc.fallbackCount)
		return lc.fallbackCount
	}

	if count != lc.fallbackCount {
		klog.Infof("found %v valid leases, updating fallback count", count)
		lc.fallbackCount = count
	}

	return count
}

func isLeaseValid(lease *coordinationv1api.Lease) bool {
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

	return lastRenewTime.Add(duration).After(time.Now()) // renewTime+duration > time.Now()
}
