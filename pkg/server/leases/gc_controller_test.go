package leases

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

func TestGarbageCollectionController(t *testing.T) {
	testCases := []struct {
		name           string
		template       util.LeaseTemplate
		selector       string
		expectDeletion bool
	}{
		{
			name: "does not delete valid acquired lease matching selector",
			template: util.LeaseTemplate{
				Labels:           map[string]string{"some": "label"},
				TimeSinceAcquire: 2 * time.Minute,
				DurationSecs:     1000,
			},
			selector:       "some=label",
			expectDeletion: false,
		}, {
			name: "does not delete valid renewed lease matching selector",
			template: util.LeaseTemplate{
				Labels:           map[string]string{"some": "label"},
				TimeSinceAcquire: 10 * time.Minute,
				TimeSinceRenew:   time.Minute,
				DurationSecs:     120,
			},
			selector:       "some=label",
			expectDeletion: false,
		}, {
			name: "does not delete expired lease not matching selector",
			template: util.LeaseTemplate{
				Labels:           map[string]string{"another": "label"},
				TimeSinceAcquire: 2 * time.Minute,
				TimeSinceRenew:   time.Minute,
				DurationSecs:     1000,
			},
			selector:       "some=label",
			expectDeletion: false,
		}, {
			name: "deletes expired lease matching selector",
			template: util.LeaseTemplate{
				Labels:           map[string]string{"some": "label"},
				TimeSinceAcquire: 4 * time.Minute,
				TimeSinceRenew:   3 * time.Minute,
				DurationSecs:     1,
			},
			selector:       "some=label",
			expectDeletion: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			lease := util.NewLeaseFromTemplate(tc.template)
			k8sClient := fake.NewSimpleClientset(lease)

			controller := NewGarbageCollectionController(k8sClient, "", 10*time.Millisecond, tc.selector)

			go controller.Run(context.Background())

			time.Sleep(100 * time.Millisecond)

			gotLease, err := k8sClient.CoordinationV1().Leases("").Get(context.Background(), lease.Name, metav1.GetOptions{})
			if errors.IsNotFound(err) && tc.expectDeletion {
				return
			} else if err != nil {
				t.Fatalf("error while getting lease: %v", err)
			} else if tc.expectDeletion {
				t.Errorf("lease should have been deleted, instead got: %v", gotLease)
			}
		})
	}

}
