//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
)

func sampleEGWHA(name string) *v1alpha1.EgressGatewayHA {
	return &v1alpha1.EgressGatewayHA{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.EgressGatewayHASpec{
			CandidateNodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"egress-gw-candidate": "true"}},
			Interface:             "eth0",
			EgressIPs:             []string{"172.18.0.200-172.18.0.210"},
		},
	}
}

// The controller creates and owns EgressGatewayHA "default"; only the schema is checked here.
func TestCRD(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := newClient(t)

	if err := c.Create(ctx, sampleEGWHA("other")); !apierrors.IsInvalid(err) {
		t.Errorf("non-default name: want Invalid error, got %v", err)
	}
	obj := sampleEGWHA(v1alpha1.SingletonName)
	obj.Spec.EgressIPs = nil
	obj.Spec.Interface = ""
	if err := c.Create(ctx, obj, client.DryRunAll); !apierrors.IsInvalid(err) {
		t.Errorf("missing required fields: want Invalid error, got %v", err)
	}
}
