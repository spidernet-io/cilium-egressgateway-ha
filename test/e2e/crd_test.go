//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

func TestCRD(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := newClient(t)

	existing := &v1alpha1.EgressGatewayHA{}
	err := c.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, existing)
	if err == nil {
		t.Skip("EgressGatewayHA default already exists; refusing to modify it")
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get default: %v", err)
	}

	t.Run("reject non-default name", func(t *testing.T) {
		err := c.Create(ctx, sampleEGWHA("other"))
		if !apierrors.IsInvalid(err) {
			t.Fatalf("want Invalid error, got %v", err)
		}
	})

	t.Run("reject missing required fields", func(t *testing.T) {
		obj := sampleEGWHA(v1alpha1.SingletonName)
		obj.Spec.EgressIPs = nil
		obj.Spec.Interface = ""
		if err := c.Create(ctx, obj); !apierrors.IsInvalid(err) {
			t.Fatalf("want Invalid error, got %v", err)
		}
	})

	t.Run("create default and update status", func(t *testing.T) {
		obj := sampleEGWHA(v1alpha1.SingletonName)
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create default: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })

		obj.Status.ActiveNode = &v1alpha1.ActiveNodeStatus{Name: "n1", EgressIPs: []string{"172.18.0.200"}, Ready: true}
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonAsExpected,
		})
		if err := c.Status().Update(ctx, obj); err != nil {
			t.Fatalf("update status: %v", err)
		}

		got := &v1alpha1.EgressGatewayHA{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), got); err != nil {
			t.Fatal(err)
		}
		if got.Status.ActiveNode == nil || !got.Status.ActiveNode.Ready ||
			!meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionReady) {
			t.Fatalf("unexpected status: %+v", got.Status)
		}
	})
}
