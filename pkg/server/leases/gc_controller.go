package leases

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

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

func (c *GarbageCollectionController) Run(ctx context.Context) {
	wait.Until(c.gc, c.gcCheckPeriod, ctx.Done())
}

func (c *GarbageCollectionController) gc() {
	ctx := context.Background()
	leases, err := c.leaseInterface.List(ctx, metav1.ListOptions{LabelSelector: c.labelSelector})
	if err != nil {
		klog.Errorf("could not list leases to garbage collect: %v", err)
		return
	}

	for _, lease := range leases.Items {
		if util.IsLeaseValid(lease) {
			continue
		}

		// Optimistic concurrency: if a lease has a different resourceVersion than
		// when we got it, it may have been renewed.
		err := c.leaseInterface.Delete(ctx, lease.Name, *metav1.NewRVDeletionPrecondition(lease.ResourceVersion))
		if errors.IsNotFound(err) {
			klog.Infof("lease %v was already deleted", lease.Name)
		} else if err != nil {
			klog.Errorf("could not delete lease %v: %v", lease.Name, err)
		}
	}
}
