package util

import (
	"time"

	"github.com/google/uuid"
	coordinationv1api "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

var timeNow = time.Now

func IsLeaseValid(lease coordinationv1api.Lease) bool {
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
func NewLeaseFromTemplate(template LeaseTemplate) *coordinationv1api.Lease {
	lease := &coordinationv1api.Lease{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:   uuid.New().String(),
			Labels: template.Labels,
		},
		Spec: coordinationv1api.LeaseSpec{},
	}

	if template.DurationSecs != 0 {
		lease.Spec.LeaseDurationSeconds = &template.DurationSecs
	}
	if template.TimeSinceAcquire != time.Duration(0) {
		acquireTime := metav1.NewMicroTime(timeNow().Add(-template.TimeSinceAcquire))
		lease.Spec.AcquireTime = &acquireTime
	}
	if template.TimeSinceRenew != time.Duration(0) {
		renewTime := metav1.NewMicroTime(timeNow().Add(-template.TimeSinceRenew))
		lease.Spec.RenewTime = &renewTime
	}

	return lease
}

type LeaseTemplate struct {
	DurationSecs     int32
	TimeSinceAcquire time.Duration
	TimeSinceRenew   time.Duration
	Labels           map[string]string
}
