package leases

import (
	"context"
	"time"

	coordinationv1api "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/klog/v2"
)

var timeNow = time.Now

type GarbageCollectionController struct {
	leaseInterface coordinationv1.LeaseInterface

	labelSelector string
	gcCheckPeriod time.Duration
}

func NewGarbageCollectionController(k8sclient kubernetes.Interface, namespace string, gcCheckPeriod time.Duration, leaseSelector string) *GarbageCollectionController {
	return &GarbageCollectionController{
		leaseInterface: k8sclient.CoordinationV1().Leases(namespace),
		gcCheckPeriod:  gcCheckPeriod,
		labelSelector:  leaseSelector,
	}
}

func (c *GarbageCollectionController) Run(stopCh <-chan struct{}) {
	go wait.Until(c.gc, c.gcCheckPeriod, stopCh)

	<-stopCh
}

func (c *GarbageCollectionController) gc() {
	ctx := context.Background()
	leases, err := c.leaseInterface.List(ctx, metav1.ListOptions{LabelSelector: c.labelSelector})
	if err != nil {
		klog.Errorf("could not list leases to garbage collect: %v", err)
		return
	}

	for _, lease := range leases.Items {
		if isLeaseValid(lease) {
			continue
		}

		// Optimistic concurrency: if a lease has a different resourceVersion than
		// when we got it, it may have been renewed.

	}
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
