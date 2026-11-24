//go:build e2e

package webhook_test

import (
	"context"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	slimv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	. "github.com/spidernet-io/cilium-egressgateway-ha/test/e2e/common"
)

// policy selects no pod, so it never affects egress traffic.
func policy(name string, mutate func(*ciliumv2.EgressGateway)) *ciliumv2.CiliumEgressGatewayPolicy {
	gw := &ciliumv2.EgressGateway{NodeSelector: &slimv1.LabelSelector{
		MatchLabels: map[string]slimv1.MatchLabelsValue{v1alpha1.ActiveLabelKey: v1alpha1.ActiveLabelValue}}}
	if mutate != nil {
		mutate(gw)
	}
	return &ciliumv2.CiliumEgressGatewayPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ciliumv2.CiliumEgressGatewayPolicySpec{
			Selectors: []ciliumv2.EgressRule{{PodSelector: &slimv1.LabelSelector{
				MatchLabels: map[string]slimv1.MatchLabelsValue{"app": "egw-e2e-none"}}}},
			DestinationCIDRs: []ciliumv2.CIDR{"192.0.2.0/24"},
			EgressGateway:    gw,
		},
	}
}

func create(p *ciliumv2.CiliumEgressGatewayPolicy) {
	GinkgoHelper()
	Expect(c.Create(context.Background(), p)).To(Succeed())
	DeferCleanup(func() { _ = c.Delete(context.Background(), p) })
}

func get(p *ciliumv2.CiliumEgressGatewayPolicy) *ciliumv2.CiliumEgressGatewayPolicy {
	GinkgoHelper()
	got := &ciliumv2.CiliumEgressGatewayPolicy{}
	Expect(c.Get(context.Background(), client.ObjectKeyFromObject(p), got)).To(Succeed())
	return got
}

var _ = Describe("CiliumEgressGatewayPolicy webhook", func() {
	It("defaults the egressIP of a managed policy", func() {
		p := policy("egw-e2e-default", nil)
		create(p)
		Expect(get(p).Spec.EgressGateway.EgressIP).To(Equal(E.EgressIP()))
	})

	It("does not touch unmanaged policies", func() {
		p := policy("egw-e2e-unmanaged", func(gw *ciliumv2.EgressGateway) {
			gw.NodeSelector.MatchLabels = map[string]slimv1.MatchLabelsValue{"other": "x"}
			gw.Interface = "eth0"
		})
		create(p)
		Expect(get(p).Spec.EgressGateway.EgressIP).To(BeEmpty())
	})

	DescribeTable("rejects invalid managed policies",
		func(mutate func(*ciliumv2.EgressGateway), spec func(*ciliumv2.CiliumEgressGatewayPolicySpec), denied string) {
			p := policy("egw-e2e-invalid", mutate)
			if spec != nil {
				spec(&p.Spec)
			}
			err := c.Create(context.Background(), p)
			if err == nil {
				_ = c.Delete(context.Background(), p)
			}
			Expect(err).To(MatchError(ContainSubstring(denied)))
		},
		Entry("extra nodeSelector label", func(gw *ciliumv2.EgressGateway) { gw.NodeSelector.MatchLabels["zone"] = "a" }, nil,
			"nodeSelector must only be matchLabels"),
		Entry("matchExpressions", func(gw *ciliumv2.EgressGateway) {
			gw.NodeSelector = &slimv1.LabelSelector{MatchExpressions: []slimv1.LabelSelectorRequirement{
				{Key: v1alpha1.ActiveLabelKey, Operator: slimv1.LabelSelectorOpExists}}}
		}, nil, "nodeSelector must only be matchLabels"),
		Entry("interface", func(gw *ciliumv2.EgressGateway) { gw.Interface = "eth0" }, nil,
			"egressGateway.interface must not be set"),
		Entry("egressIP outside spec.egressIPs", func(gw *ciliumv2.EgressGateway) { gw.EgressIP = E.EgressIPAt(-1) }, nil,
			"is not in EgressGatewayHA spec.egressIPs"),
		Entry("egressGateways", nil, func(s *ciliumv2.CiliumEgressGatewayPolicySpec) {
			s.EgressGateways = []ciliumv2.EgressGateway{*s.EgressGateway}
		}, "egressGateways is not supported"),
	)

	It("warns when the egressIP changes", func() {
		p := policy("egw-e2e-change", func(gw *ciliumv2.EgressGateway) { gw.EgressIP = E.EgressIPAt(1) })
		create(p)
		out, err := Command("kubectl", "patch", "ciliumegressgatewaypolicy", p.Name, "--type=merge",
			"-p", `{"spec":{"egressGateway":{"egressIP":"`+E.EgressIPAt(2)+`"}}}`)
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("Warning: egressIP changed from " + E.EgressIPAt(1) + " to " + E.EgressIPAt(2)))
	})
})

var _ = Describe("EgressGatewayHA webhook", func() {
	It("rejects invalid changes", func() {
		inUse := policy("egw-e2e-in-use", func(gw *ciliumv2.EgressGateway) { gw.EgressIP = E.EgressIPAt(5) })
		create(inUse)
		for patch, denied := range map[string]string{
			`{"spec":{"activeNode":"no-such-node"}}`:                                              "node no-such-node not found",
			`{"spec":{"activeNode":"` + E.ClientNode + `"}}`:                                      "does not match spec.egressNodeSelector",
			`{"spec":{"defaultEgressIP":"` + E.TargetIP() + `"}}`:                                 "is not in spec.egressIPs",
			`{"spec":{"egressIPs":["` + E.EgressIPStart.String() + `-` + E.EgressIPAt(4) + `"]}}`: "egress IPs still used by policies: egw-e2e-in-use",
			`{"spec":{"egressIPs":["` + E.EgressIPRange() + `","bad-ip"]}}`:                       "spec.egressIPs",
		} {
			Expect(PatchEGWHA(c, patch)).To(MatchError(ContainSubstring(denied)), patch)
		}
	})
})
