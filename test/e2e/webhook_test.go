//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	slimv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
)

// webhookPolicy selects no pod, so it does not affect the traffic tests.
func webhookPolicy(name string, mutate func(*ciliumv2.EgressGateway)) *ciliumv2.CiliumEgressGatewayPolicy {
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

func TestWebhook(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	cleanup := func(names ...string) {
		t.Cleanup(func() {
			for _, n := range names {
				_ = c.Delete(context.Background(), webhookPolicy(n, nil))
			}
		})
	}

	t.Run("managed policy without egressIP gets the default", func(t *testing.T) {
		p := webhookPolicy("egw-e2e-default", nil)
		cleanup(p.Name)
		if err := c.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		got := &ciliumv2.CiliumEgressGatewayPolicy{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.EgressGateway.EgressIP != egressIP {
			t.Fatalf("egressIP = %q, want %s", got.Spec.EgressGateway.EgressIP, egressIP)
		}
	})

	t.Run("unmanaged policy is not touched", func(t *testing.T) {
		p := webhookPolicy("egw-e2e-unmanaged", func(gw *ciliumv2.EgressGateway) {
			gw.NodeSelector.MatchLabels = map[string]slimv1.MatchLabelsValue{"other": "x"}
			gw.Interface = "eth0"
		})
		cleanup(p.Name)
		if err := c.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		got := &ciliumv2.CiliumEgressGatewayPolicy{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.EgressGateway.EgressIP != "" {
			t.Fatalf("egressIP = %q, want empty", got.Spec.EgressGateway.EgressIP)
		}
	})

	t.Run("invalid managed policies are rejected", func(t *testing.T) {
		for name, tc := range map[string]struct {
			mutate func(*ciliumv2.EgressGateway)
			spec   func(*ciliumv2.CiliumEgressGatewayPolicySpec)
			denied string
		}{
			"extra label": {mutate: func(gw *ciliumv2.EgressGateway) { gw.NodeSelector.MatchLabels["zone"] = "a" },
				denied: "nodeSelector must only be matchLabels"},
			"matchExpressions": {mutate: func(gw *ciliumv2.EgressGateway) {
				gw.NodeSelector = &slimv1.LabelSelector{MatchExpressions: []slimv1.LabelSelectorRequirement{
					{Key: v1alpha1.ActiveLabelKey, Operator: slimv1.LabelSelectorOpExists}}}
			}, denied: "nodeSelector must only be matchLabels"},
			"interface": {mutate: func(gw *ciliumv2.EgressGateway) { gw.Interface = "eth0" },
				denied: "egressGateway.interface must not be set"},
			"egressIP not allowed": {mutate: func(gw *ciliumv2.EgressGateway) { gw.EgressIP = "172.18.0.199" },
				denied: "172.18.0.199 is not in EgressGatewayHA spec.egressIPs"},
			"egressGateways": {spec: func(s *ciliumv2.CiliumEgressGatewayPolicySpec) {
				s.EgressGateways = []ciliumv2.EgressGateway{*s.EgressGateway}
			}, denied: "egressGateways is not supported"},
		} {
			p := webhookPolicy("egw-e2e-invalid", tc.mutate)
			if tc.spec != nil {
				tc.spec(&p.Spec)
			}
			err := c.Create(ctx, p)
			if err == nil {
				_ = c.Delete(ctx, p)
			}
			if err == nil || !strings.Contains(err.Error(), tc.denied) {
				t.Errorf("%s: got %v, want denial with %q", name, err, tc.denied)
			}
		}
	})

	t.Run("changing egressIP warns", func(t *testing.T) {
		p := webhookPolicy("egw-e2e-change", func(gw *ciliumv2.EgressGateway) { gw.EgressIP = "172.18.0.201" })
		cleanup(p.Name)
		if err := c.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		out, err := command("kubectl", "patch", "ciliumegressgatewaypolicy", p.Name, "--type=merge",
			"-p", `{"spec":{"egressGateway":{"egressIP":"172.18.0.202"}}}`)
		if err != nil || !strings.Contains(out, "Warning: egressIP changed from 172.18.0.201 to 172.18.0.202") {
			t.Fatalf("kubectl patch: %v: %s", err, out)
		}
	})

	t.Run("invalid EgressGatewayHA changes are rejected", func(t *testing.T) {
		p := webhookPolicy("egw-e2e-in-use", func(gw *ciliumv2.EgressGateway) { gw.EgressIP = "172.18.0.205" })
		cleanup(p.Name)
		if err := c.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		for patch, denied := range map[string]string{
			`{"spec":{"activeNode":"no-such-node"}}`:                        "node no-such-node not found",
			`{"spec":{"activeNode":"` + worker3 + `"}}`:                     "does not match spec.egressNodeSelector",
			`{"spec":{"defaultEgressIP":"172.18.0.250"}}`:                   "spec.defaultEgressIP 172.18.0.250 is not in spec.egressIPs",
			`{"spec":{"egressIPs":["172.18.0.200-172.18.0.204"]}}`:          "egress IPs still used by policies: egw-e2e-in-use (172.18.0.205)",
			`{"spec":{"egressIPs":["172.18.0.200-172.18.0.210","bad-ip"]}}`: "spec.egressIPs",
		} {
			if err := patchEGWHA(c, patch); err == nil || !strings.Contains(err.Error(), denied) {
				t.Errorf("%s: got %v, want denial with %q", patch, err, denied)
			}
		}
	})
}
