package k8s

import (
	"flag"
	"fmt"
	"strings"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
)

// ActiveLabel is the label the controller puts on the Node and CiliumNode of
// the active node. Managed policies select it in their nodeSelector. It is set
// at install time and must be the same for the controller and the agents.
type ActiveLabel struct {
	Key   string
	Value string
}

func DefaultActiveLabel() ActiveLabel {
	return ActiveLabel{Key: v1alpha1.DefaultActiveLabelKey, Value: v1alpha1.DefaultActiveLabelValue}
}

// BindFlags registers --active-node-label-key and --active-node-label-value.
func (l *ActiveLabel) BindFlags(fs *flag.FlagSet) {
	d := DefaultActiveLabel()
	fs.StringVar(&l.Key, "active-node-label-key", d.Key, "label key put on the active node and selected by managed policies")
	fs.StringVar(&l.Value, "active-node-label-value", d.Value, "label value put on the active node and selected by managed policies")
}

func (l ActiveLabel) Validate() error {
	if errs := validation.IsQualifiedName(l.Key); len(errs) > 0 {
		return fmt.Errorf("invalid active node label key %q: %s", l.Key, strings.Join(errs, "; "))
	}
	if errs := validation.IsValidLabelValue(l.Value); len(errs) > 0 || l.Value == "" {
		return fmt.Errorf("invalid active node label value %q: %s", l.Value, strings.Join(errs, "; "))
	}
	return nil
}

func (l ActiveLabel) String() string { return l.Key + "=" + l.Value }

// Matches reports whether labels carry the active label.
func (l ActiveLabel) Matches(labels map[string]string) bool { return labels[l.Key] == l.Value }

// IsManaged reports whether the policy references the active label key in its
// nodeSelector, which makes it managed by this project.
func (l ActiveLabel) IsManaged(p *ciliumv2.CiliumEgressGatewayPolicy) bool {
	gw := p.Spec.EgressGateway
	if gw == nil || gw.NodeSelector == nil {
		return false
	}
	if _, ok := gw.NodeSelector.MatchLabels[l.Key]; ok {
		return true
	}
	for _, e := range gw.NodeSelector.MatchExpressions {
		if e.Key == l.Key {
			return true
		}
	}
	return false
}
