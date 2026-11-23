package controller

import (
	"context"
	"fmt"
	"os"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sigs.k8s.io/yaml"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
)

// LoadDefaultSpec reads the EgressGatewayHA spec rendered from Helm values.
func LoadDefaultSpec(path string) (*v1alpha1.EgressGatewayHASpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	spec := &v1alpha1.EgressGatewayHASpec{}
	if err := yaml.UnmarshalStrict(b, spec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return spec, nil
}

func nodeReady(n *corev1.Node) corev1.ConditionStatus {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status
		}
	}
	return ""
}

// nodeChanged ignores heartbeat-only Node updates.
var nodeChanged = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	o, n := e.ObjectOld.(*corev1.Node), e.ObjectNew.(*corev1.Node)
	return !equality.Semantic.DeepEqual(o.Labels, n.Labels) || nodeReady(o) != nodeReady(n) ||
		!equality.Semantic.DeepEqual(o.Status.Addresses, n.Status.Addresses)
}}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: v1alpha1.SingletonName}}}
	})
	// Reconcile once at start so that the default object is created.
	start := make(chan event.GenericEvent, 1)
	start <- event.GenericEvent{Object: &v1alpha1.EgressGatewayHA{}}
	return ctrl.NewControllerManagedBy(mgr).
		Named("egressgatewayha").
		// Status changes matter: the agent reports readiness through status.
		Watches(&v1alpha1.EgressGatewayHA{}, enqueue).
		Watches(&corev1.Node{}, enqueue, builder.WithPredicates(nodeChanged)).
		Watches(&ciliumv2.CiliumNode{}, enqueue, builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Watches(&corev1.Pod{}, enqueue).
		WatchesRawSource(source.Channel(start, enqueue)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
