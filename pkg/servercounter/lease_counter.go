package servercounter

import (
	"fmt"
	"time"

	coordinationv1api "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/labels"
	coordinationv1listers "k8s.io/client-go/listers/coordination/v1"
	"k8s.io/klog/v2"
)

type ServerLeaseCounter struct {
	lister        coordinationv1listers.LeaseLister
	selector      labels.Selector
	fallbackCount int
}

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

func (lc *ServerLeaseCounter) CountServers() int {
	leases, err := lc.lister.List(lc.selector)
	if err != nil {
		klog.Errorf("could not list leases to update server count, using fallback count (%v): %v", lc.fallbackCount, err)
		return lc.fallbackCount
	}

	count := 0
	for _, lease := range leases {
		if IsLeaseValid(lease) {
			count++
		} else {
			klog.InfoS("excluding expired lease from server count", "selector", lc.selector, "lease", lease)
		}
	}

	lc.fallbackCount = count
	return count
}

func IsLeaseValid(lease *coordinationv1api.Lease) bool {
	var lastRenewTime time.Time
	if lease.Spec.RenewTime != nil {
		lastRenewTime = lease.Spec.RenewTime.Time
	} else if lease.Spec.AcquireTime != nil {
		lastRenewTime = lease.Spec.AcquireTime.Time
	} else {
		klog.Warningf("lease %v has neither a renew time or an acquire time, marking as expired: %v", lease.Name, lease)
	}

	duration := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second

	return lastRenewTime.Add(duration).After(time.Now()) // renewTime+duration > time.Now()
}
