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
package util

import (
	"time"

	coordinationv1api "k8s.io/api/coordination/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

func IsLeaseValid(pc clock.PassiveClock, lease coordinationv1api.Lease) bool {
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

	return lastRenewTime.Add(duration).After(pc.Now()) // renewTime+duration > time.Now()
}
