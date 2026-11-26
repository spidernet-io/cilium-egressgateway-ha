// Package k8s holds Kubernetes helpers shared by the controller and the agent.
package k8s

import (
	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
)

// NewScheme returns a scheme with core Kubernetes, Cilium v2 and EgressGatewayHA types.
func NewScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(ciliumv2.AddToScheme(s))
	utilruntime.Must(v1alpha1.AddToScheme(s))
	return s
}
