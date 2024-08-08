package testing

import (
	"time"

	"github.com/google/uuid"
	coordinationv1api "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
)

type LeaseTemplate struct {
	DurationSecs     int32
	TimeSinceAcquire time.Duration
	TimeSinceRenew   time.Duration
	Labels           map[string]string
}

func NewLeaseFromTemplate(pc clock.PassiveClock, template LeaseTemplate) *coordinationv1api.Lease {
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
		acquireTime := metav1.NewMicroTime(pc.Now().Add(-template.TimeSinceAcquire))
		lease.Spec.AcquireTime = &acquireTime
	}
	if template.TimeSinceRenew != time.Duration(0) {
		renewTime := metav1.NewMicroTime(pc.Now().Add(-template.TimeSinceRenew))
		lease.Spec.RenewTime = &renewTime
	}

	return lease
}
