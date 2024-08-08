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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

type GarbageCollectionController struct {
	leaseInterface coordinationv1.LeaseInterface

	labelSelector string
	gcCheckPeriod time.Duration

	pc clock.PassiveClock
}

func NewGarbageCollectionController(pc clock.PassiveClock, k8sclient kubernetes.Interface, namespace string, gcCheckPeriod time.Duration, leaseSelector string) *GarbageCollectionController {
	return &GarbageCollectionController{
		leaseInterface: k8sclient.CoordinationV1().Leases(namespace),
		gcCheckPeriod:  gcCheckPeriod,
		labelSelector:  leaseSelector,
		pc:             pc,
	}
}

func (c *GarbageCollectionController) Run(ctx context.Context) {
	wait.UntilWithContext(ctx, c.gc, c.gcCheckPeriod)
}

func (c *GarbageCollectionController) gc(ctx context.Context) {
	leases, err := c.leaseInterface.List(ctx, metav1.ListOptions{LabelSelector: c.labelSelector})
	if err != nil {
		klog.Errorf("Could not list leases to garbage collect: %v", err)
		return
	}

	for _, lease := range leases.Items {
		if util.IsLeaseValid(c.pc, lease) {
			continue
		}

		// Optimistic concurrency: if a lease has a different resourceVersion than
		// when we got it, it may have been renewed.
		err := c.leaseInterface.Delete(ctx, lease.Name, *metav1.NewRVDeletionPrecondition(lease.ResourceVersion))
		if errors.IsNotFound(err) {
			klog.V(4).Infof("Lease %v was already deleted", lease.Name)
		} else if err != nil {
			klog.Errorf("Could not delete lease %v: %v", lease.Name, err)
		}
	}
}
