package controller

import (
	"context"
	"fmt"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/metrics"
)

// reportComponents emits a Warning Event when the agent or cilium-agent on
// the active node becomes not ready. These signals never trigger a switch:
// the egress IP and the BPF maps stay in place while they restart.
func (st *state) reportComponents() {
	active := st.egw.Spec.ActiveNode
	if st.r.notReady == nil {
		st.r.notReady = map[string]bool{}
	}
	n := st.snap.node(active)
	type component struct {
		name, reason string
		ready        bool
	}
	components := []component{{"agent", "ActiveNodeAgentNotReady", n.agentReady}}
	if st.snap.ciliumAgentCheck {
		components = append(components, component{"cilium-agent", "ActiveNodeCiliumAgentNotReady", n.ciliumReady})
	}
	for _, c := range components {
		key := active + "/" + c.name
		if active == "" || !n.exists || c.ready {
			delete(st.r.notReady, key)
			continue
		}
		if !st.r.notReady[key] {
			st.r.notReady[key] = true
			st.event(corev1.EventTypeWarning, c.reason,
				fmt.Sprintf("%s on active node %s is not ready; egress traffic is not affected, no switch", c.name, active))
		}
	}
}

func (st *state) updateMetrics() {
	active := st.egw.Spec.ActiveNode
	metrics.ActiveNode.Reset()
	if active != "" {
		metrics.ActiveNode.WithLabelValues(active).Set(1)
	}

	metrics.Condition.Reset()
	conds, _ := st.conds.result(st.egw.Generation)
	if a := st.egw.Status.ActiveNode; a != nil && a.Name == active && active != "" {
		if g := meta.FindStatusCondition(st.egw.Status.Conditions, v1alpha1.ConditionGatewayReady); g != nil {
			conds = append(conds, *g)
		}
	}
	for _, c := range conds {
		metrics.Condition.WithLabelValues(c.Type, string(c.Status), c.Reason).Set(1)
	}

	metrics.CandidateReachable.Reset()
	metrics.CandidateAvailable.Reset()
	for _, name := range st.snap.candidateNames() {
		if st.snap.probe != nil {
			r, _ := st.snap.probe(name)
			metrics.CandidateReachable.WithLabelValues(name).Set(metrics.Bool(r.Reachable()))
		}
		metrics.CandidateAvailable.WithLabelValues(name).Set(metrics.Bool(st.snap.unavailable(name) == ""))
	}

	metrics.ActiveNodeComponentReady.Reset()
	if active != "" {
		n := st.snap.node(active)
		metrics.ActiveNodeComponentReady.WithLabelValues("agent").Set(metrics.Bool(n.agentReady))
		if st.snap.ciliumAgentCheck {
			metrics.ActiveNodeComponentReady.WithLabelValues("cilium-agent").Set(metrics.Bool(n.ciliumReady))
		}
	}
}

// notifyPolicies emits a Warning Event on every managed policy when Ready
// becomes False or its reason changes while False, e.g. an egress IP conflict
// or no available active node, so users looking at their policy see it.
func (st *state) notifyPolicies(ctx context.Context) {
	if st.r.Recorder == nil {
		return
	}
	cur, ok := st.conds.get(v1alpha1.ConditionReady)
	if !ok || cur.Status != metav1.ConditionFalse {
		return
	}
	if old, had := st.conds.old[v1alpha1.ConditionReady]; had && old.Status == cur.Status && old.Reason == cur.Reason {
		return
	}
	policies := &ciliumv2.CiliumEgressGatewayPolicyList{}
	if err := st.r.Client.List(ctx, policies); err != nil {
		logf.FromContext(ctx).Error(err, "unable to list policies for events")
		return
	}
	for i := range policies.Items {
		if p := &policies.Items[i]; st.r.Config.ActiveLabel.IsManaged(p) {
			st.r.Recorder.Eventf(p, st.egw, corev1.EventTypeWarning, "EgressGatewayNotReady", "Check",
				"EgressGatewayHA default is not ready (%s): %s", cur.Reason, cur.Message)
		}
	}
}
