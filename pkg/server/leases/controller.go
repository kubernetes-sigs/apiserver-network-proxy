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
package leases

import (
	"context"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/component-helpers/apimachinery/lease"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

const shutdownDeleteGrace = 10

type Controller struct {
	leaseName      string
	leaseNamespace string
	k8sClient      kubernetes.Interface

	gcController      *GarbageCollectionController
	acquireController lease.Controller
}

func NewController(k8sClient kubernetes.Interface, holderIdentity string, leaseDurationSeconds int32, renewInterval time.Duration, gcCheckPeriod time.Duration, leaseName, leaseNamespace string, leaseLabels map[string]string) *Controller {
	acquireController := lease.NewController(
		clock.RealClock{},
		k8sClient,
		holderIdentity,
		leaseDurationSeconds,
		nil,
		renewInterval,
		leaseName,
		leaseNamespace,
		func(lease *coordinationv1.Lease) error {
			lease.SetLabels(leaseLabels)
			return nil
		},
	)
	gcController := NewGarbageCollectionController(clock.RealClock{}, k8sClient, leaseNamespace, gcCheckPeriod, labels.FormatLabels(leaseLabels))

	return &Controller{
		leaseNamespace:    leaseNamespace,
		leaseName:         leaseName,
		k8sClient:         k8sClient,
		gcController:      gcController,
		acquireController: acquireController,
	}
}

// Run starts the garbage collection and lease acquisition controllers. Non-blocking.
func (c *Controller) Run(ctx context.Context) {
	go c.gcController.Run(ctx)
	go c.acquireController.Run(ctx)

}

func (c *Controller) Stop() {
	klog.Infof("Cleaning up server lease %q", c.leaseName)
	err := c.k8sClient.CoordinationV1().Leases(c.leaseNamespace).Delete(context.Background(), c.leaseName, metav1.DeleteOptions{})
	if err != nil {
		klog.Errorf("Could not clean up lease %q in namespace %q", c.leaseName, c.leaseNamespace)
	}
}
