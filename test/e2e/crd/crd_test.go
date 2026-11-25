//go:build e2e

package crd_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/test/e2e/common"
)

func sample(name string) *v1alpha1.EgressGatewayHA {
	return &v1alpha1.EgressGatewayHA{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.EgressGatewayHASpec{
			EgressNodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.DefaultEgressNodeLabelKey: "true"}},
			Interface:          "eth0",
			EgressIPs:          []string{"192.0.2.1"},
		},
	}
}

// The controller creates and owns EgressGatewayHA "default"; only the schema is checked here.
var _ = Describe("EgressGatewayHA CRD", func() {
	It("rejects a name other than default", func() {
		err := c.Create(context.Background(), sample("other"), client.DryRunAll)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("the only allowed name is 'default'"))
	})

	It("rejects missing required fields", func() {
		obj := sample(v1alpha1.SingletonName)
		obj.Spec.EgressIPs = nil
		err := c.Create(context.Background(), obj, client.DryRunAll)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
	})

	It("accepts an omitted interface", func() {
		obj, err := common.GetEGWHA(c)
		Expect(err).NotTo(HaveOccurred())
		obj.Spec.Interface = ""
		Expect(c.Update(context.Background(), obj, client.DryRunAll)).To(Succeed())
	})
})
