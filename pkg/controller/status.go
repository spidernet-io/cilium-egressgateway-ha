package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
)

// ownedConditions are written by the controller; GatewayReady belongs to the agent.
var ownedConditions = []string{
	v1alpha1.ConditionActiveNodeElected,
	v1alpha1.ConditionActiveNodeReachable,
	v1alpha1.ConditionActiveNodeReady,
	v1alpha1.ConditionSwitching,
	v1alpha1.ConditionReady,
}

// condSet tracks the controller-owned conditions of one reconcile.
type condSet struct {
	old map[string]metav1.Condition
	cur map[string]metav1.Condition
	now time.Time
}

func newCondSet(existing []metav1.Condition, now time.Time) *condSet {
	s := &condSet{old: map[string]metav1.Condition{}, cur: map[string]metav1.Condition{}, now: now}
	for _, t := range ownedConditions {
		if c := meta.FindStatusCondition(existing, t); c != nil {
			s.old[t] = *c
			s.cur[t] = *c
		}
	}
	return s
}

func (s *condSet) set(t string, status metav1.ConditionStatus, reason, msg string) {
	c := metav1.Condition{Type: t, Status: status, Reason: reason, Message: msg, LastTransitionTime: metav1.NewTime(s.now)}
	if old, ok := s.old[t]; ok && old.Status == status {
		c.LastTransitionTime = old.LastTransitionTime
	}
	s.cur[t] = c
}

func (s *condSet) get(t string) (metav1.Condition, bool) {
	c, ok := s.cur[t]
	return c, ok
}

func (s *condSet) has(t string) bool {
	_, ok := s.cur[t]
	return ok
}

// result returns all owned conditions and whether any of them changed.
func (s *condSet) result(generation int64) ([]metav1.Condition, bool) {
	var out []metav1.Condition
	changed := len(s.cur) != len(s.old)
	for _, t := range ownedConditions {
		c, ok := s.cur[t]
		if !ok {
			continue
		}
		c.ObservedGeneration = generation
		old, had := s.old[t]
		if !had || old.Status != c.Status || old.Reason != c.Reason || old.Message != c.Message ||
			old.ObservedGeneration != c.ObservedGeneration {
			changed = true
		}
		out = append(out, c)
	}
	return out, changed
}

// ClientStatusWriter applies the conditions with Server-Side Apply using FieldManager.
type ClientStatusWriter struct {
	Client client.Client
}

func (w *ClientStatusWriter) Apply(ctx context.Context, conds []metav1.Condition, candidates []v1alpha1.CandidateStatus) error {
	list := make([]any, 0, len(conds))
	for _, c := range conds {
		list = append(list, map[string]any{
			"type":               c.Type,
			"status":             string(c.Status),
			"reason":             c.Reason,
			"message":            c.Message,
			"observedGeneration": c.ObservedGeneration,
			"lastTransitionTime": c.LastTransitionTime.UTC().Format(time.RFC3339),
		})
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "EgressGatewayHA",
		"metadata":   map[string]any{"name": v1alpha1.SingletonName},
		"status":     map[string]any{"conditions": list},
	}}
	if len(candidates) > 0 {
		cl := make([]any, 0, len(candidates))
		for _, c := range candidates {
			cl = append(cl, map[string]any{"name": c.Name, "reachable": c.Reachable})
		}
		u.Object["status"].(map[string]any)["candidates"] = cl
	}
	return w.Client.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(FieldManager), client.ForceOwnership)
}
