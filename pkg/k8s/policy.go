package k8s

import (
	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
)

// IsManagedPolicy reports whether the policy references the active label in
// its nodeSelector, which makes it managed by this project.
func IsManagedPolicy(p *ciliumv2.CiliumEgressGatewayPolicy) bool {
	gw := p.Spec.EgressGateway
	if gw == nil || gw.NodeSelector == nil {
		return false
	}
	if _, ok := gw.NodeSelector.MatchLabels[v1alpha1.ActiveLabelKey]; ok {
		return true
	}
	for _, e := range gw.NodeSelector.MatchExpressions {
		if e.Key == v1alpha1.ActiveLabelKey {
			return true
		}
	}
	return false
}
