package k8s

import (
	"testing"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	slimv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
)

func TestActiveLabelValidate(t *testing.T) {
	for l, ok := range map[ActiveLabel]bool{
		DefaultActiveLabel():                      true,
		{Key: "example.com/active", Value: "yes"}: true,
		{Key: "", Value: "true"}:                  false,
		{Key: "bad key", Value: "true"}:           false,
		{Key: "example.com/active", Value: ""}:    false,
		{Key: "example.com/active", Value: "a b"}: false,
	} {
		if err := l.Validate(); (err == nil) != ok {
			t.Errorf("Validate(%v) = %v, want ok=%v", l, err, ok)
		}
	}
}

func TestActiveLabelIsManaged(t *testing.T) {
	l := ActiveLabel{Key: "example.com/active", Value: "yes"}
	policy := func(sel *slimv1.LabelSelector) *ciliumv2.CiliumEgressGatewayPolicy {
		return &ciliumv2.CiliumEgressGatewayPolicy{Spec: ciliumv2.CiliumEgressGatewayPolicySpec{
			EgressGateway: &ciliumv2.EgressGateway{NodeSelector: sel}}}
	}
	for name, tc := range map[string]struct {
		p    *ciliumv2.CiliumEgressGatewayPolicy
		want bool
	}{
		"matchLabels":      {policy(&slimv1.LabelSelector{MatchLabels: map[string]slimv1.MatchLabelsValue{l.Key: "x"}}), true},
		"matchExpressions": {policy(&slimv1.LabelSelector{MatchExpressions: []slimv1.LabelSelectorRequirement{{Key: l.Key}}}), true},
		"default key":      {policy(&slimv1.LabelSelector{MatchLabels: map[string]slimv1.MatchLabelsValue{v1alpha1.DefaultActiveLabelKey: "true"}}), false},
		"no nodeSelector":  {policy(nil), false},
		"no egressGateway": {&ciliumv2.CiliumEgressGatewayPolicy{}, false},
	} {
		if got := l.IsManaged(tc.p); got != tc.want {
			t.Errorf("%s: IsManaged = %v, want %v", name, got, tc.want)
		}
	}
	if !l.Matches(map[string]string{l.Key: "yes"}) || l.Matches(map[string]string{l.Key: "true"}) {
		t.Error("Matches must compare the value")
	}
}
