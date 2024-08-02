package leases

import (
	"context"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/component-helpers/apimachinery/lease"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

type Controller struct {
	gcController      *GarbageCollectionController
	acquireController lease.Controller
}

func NewController(k8sClient kubernetes.Interface, holderIdentity string, leaseDurationSeconds int32, renewInterval time.Duration, gcCheckPeriod time.Duration, leaseName, leaseNamespace string, leaseLabels map[string]string) *Controller {
	acquireController := lease.NewController(
		clock.RealClock{},
		k8sClient,
		holderIdentity,
		leaseDurationSeconds,
		func() {
			klog.Errorf("repeat heartbeat error in lease acquisition controller for lease %v", leaseName)
		},
		renewInterval,
		leaseName,
		leaseNamespace,
		func(lease *coordinationv1.Lease) error {
			lease.SetLabels(leaseLabels)
			return nil
		},
	)
	gcController := NewGarbageCollectionController(k8sClient, leaseNamespace, gcCheckPeriod, labels.FormatLabels(leaseLabels))

	return &Controller{
		gcController:      gcController,
		acquireController: acquireController,
	}
}

// Run starts the garbage collection and lease acquisition controllers. Non-blocking.
func (c *Controller) Run(ctx context.Context) {
	go c.gcController.Run(ctx)
	go c.acquireController.Run(ctx)
}
