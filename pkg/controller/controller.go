// Package controller decides which node is the active egress gateway and moves
// the active label to it with a two-phase switch:
//
//  1. write spec.activeNode and wait until the agent on that node reports all
//     egress IPs configured in status.activeNode;
//  2. put the active label on its Node and CiliumNode, then remove it from all
//     other nodes.
//
// The switch is complete when the label is only on spec.activeNode. The origin
// of an in-progress switch is kept in SwitchAnnotation, written together with
// spec.activeNode, so a restarted controller continues where it stopped and a
// spec.activeNode change without a matching annotation is a manual switch.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"time"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/metrics"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/probe"
)

const (
	FieldManager = "egressgateway-ha-controller"

	// SwitchAnnotation records the switch in progress as JSON switchRecord.
	SwitchAnnotation = "egressgateway-ha.spidernet.io/switch"
)

type switchRecord struct {
	From    string `json:"from,omitempty"`
	To      string `json:"to"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

type Config struct {
	CiliumNamespace string
	CiliumSelector  labels.Selector
	AgentNamespace  string
	AgentSelector   labels.Selector
	ResyncInterval  time.Duration
	// Default is used to create EgressGatewayHA "default" when it does not exist.
	Default *v1alpha1.EgressGatewayHASpec
}

// StatusWriter applies the status fields owned by the controller.
type StatusWriter interface {
	Apply(ctx context.Context, conds []metav1.Condition, candidates []v1alpha1.CandidateStatus) error
}

// Prober probes nodes with TCP connections, see package probe.
type Prober interface {
	SetTargets(map[string]netip.Addr)
	Get(name string) (probe.Result, bool)
}

type Reconciler struct {
	Client client.Client
	// Reader reads EgressGatewayHA and the active labels without the cache: a
	// reconcile right after a write must not act on, or write back, stale state.
	// Defaults to Client.
	Reader   client.Reader
	Status   StatusWriter
	Recorder events.EventRecorder
	Config   Config
	Now      func() time.Time
	// Prober probes the candidate nodes; nil treats all nodes as reachable.
	Prober Prober

	trigger chan event.GenericEvent
	// notReady remembers components on the active node already reported not ready.
	notReady map[string]bool
}

// Notify triggers a reconcile, e.g. when a probe result changes.
func (r *Reconciler) Notify() {
	select {
	case r.trigger <- event.GenericEvent{Object: &v1alpha1.EgressGatewayHA{}}:
	default:
	}
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	egw := &v1alpha1.EgressGatewayHA{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, egw); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := r.createDefault(ctx); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.Config.ResyncInterval}, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(&egw.Spec.CandidateNodeSelector)
	if err != nil {
		log.Error(err, "invalid spec.candidateNodeSelector, not reconciling")
		return ctrl.Result{}, nil
	}
	snap, err := r.snapshot(ctx, sel, egw.Spec.ActiveNode)
	if err != nil {
		return ctrl.Result{}, err
	}
	st := &state{r: r, egw: egw, snap: snap, conds: newCondSet(egw.Status.Conditions, r.now())}
	if v, ok := egw.Annotations[SwitchAnnotation]; ok {
		sw := &switchRecord{}
		if err := json.Unmarshal([]byte(v), sw); err == nil {
			st.sw = sw
		} else {
			log.Error(err, "ignoring invalid switch annotation", "value", v)
		}
	}
	runErr := st.run(ctx)
	st.finishConditions()
	st.reportComponents()
	st.updateMetrics()
	st.notifyPolicies(ctx)
	if err := st.applyStatus(ctx); err != nil {
		return ctrl.Result{}, err
	}
	if runErr != nil {
		return ctrl.Result{}, runErr
	}
	return ctrl.Result{RequeueAfter: r.Config.ResyncInterval}, nil
}

func (r *Reconciler) createDefault(ctx context.Context) error {
	log := logf.FromContext(ctx)
	if r.Config.Default == nil {
		log.Info("EgressGatewayHA default not found and no default spec configured")
		return nil
	}
	egw := &v1alpha1.EgressGatewayHA{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName},
		Spec:       *r.Config.Default.DeepCopy(),
	}
	egw.Spec.ActiveNode = ""
	if err := r.Client.Create(ctx, egw); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create EgressGatewayHA default: %w", err)
	}
	log.Info("created EgressGatewayHA default from the default spec")
	return nil
}

func (r *Reconciler) snapshot(ctx context.Context, sel labels.Selector, active string) (*snapshot, error) {
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes); err != nil {
		return nil, err
	}
	cns := &ciliumv2.CiliumNodeList{}
	if err := r.Client.List(ctx, cns); err != nil {
		return nil, err
	}
	activeSel := client.MatchingLabels{v1alpha1.ActiveLabelKey: v1alpha1.ActiveLabelValue}
	labelledNodes := &corev1.NodeList{}
	if err := r.reader().List(ctx, labelledNodes, activeSel); err != nil {
		return nil, err
	}
	labelledCNs := &ciliumv2.CiliumNodeList{}
	if err := r.reader().List(ctx, labelledCNs, activeSel); err != nil {
		return nil, err
	}
	ciliumPods := &corev1.PodList{}
	if err := r.Client.List(ctx, ciliumPods, client.InNamespace(r.Config.CiliumNamespace),
		client.MatchingLabelsSelector{Selector: r.Config.CiliumSelector}); err != nil {
		return nil, err
	}
	agentPods := &corev1.PodList{}
	if err := r.Client.List(ctx, agentPods, client.InNamespace(r.Config.AgentNamespace),
		client.MatchingLabelsSelector{Selector: r.Config.AgentSelector}); err != nil {
		return nil, err
	}
	s := newSnapshot(sel, nodes.Items, cns.Items, ciliumPods.Items, agentPods.Items)
	s.setLabels(labelledNodes.Items, labelledCNs.Items)
	if r.Prober != nil {
		r.Prober.SetTargets(s.probeTargets(active))
		s.probe = r.Prober.Get
	}
	return s, nil
}

// state is the working set of one reconcile.
type state struct {
	r     *Reconciler
	egw   *v1alpha1.EgressGatewayHA
	snap  *snapshot
	sw    *switchRecord
	conds *condSet
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func (st *state) event(eventType, reason, note string) {
	if st.r.Recorder != nil {
		st.r.Recorder.Eventf(st.egw, nil, eventType, reason, "Switch", "%s", note)
	}
}

func (st *state) run(ctx context.Context) error {
	active := st.egw.Spec.ActiveNode
	if active != "" && st.snap.node(active).labelled() {
		return st.steady(ctx)
	}

	from := ""
	if l := st.snap.labelled(); len(l) > 0 {
		from = l[0]
	}

	if active == "" {
		// The label is still on a usable node, e.g. spec.activeNode was cleared: keep it.
		if from != "" && st.snap.unavailable(from) == "" {
			return st.setActive(ctx, from, nil)
		}
		to := st.snap.next("")
		if to == "" {
			st.noCandidate(fmt.Sprintf("no available candidate for the initial active node: %s", st.candidatesSummary()))
			return nil
		}
		return st.start(ctx, from, to, v1alpha1.ReasonInitial, fmt.Sprintf("%s selected as the initial active node", to))
	}

	if st.sw == nil || st.sw.To != active {
		// spec.activeNode was changed by someone else: a manual switch.
		if why := st.snap.unavailable(active); why != "" {
			return st.rejectTarget(ctx, from, active, why)
		}
		sw := &switchRecord{From: from, To: active, Reason: v1alpha1.ReasonManual}
		if err := st.setActive(ctx, active, sw); err != nil {
			return err
		}
		metrics.SwitchesTotal.WithLabelValues(v1alpha1.ReasonManual).Inc()
		st.event(corev1.EventTypeNormal, "SwitchStarted", fmt.Sprintf("switching %s -> %s (Manual)", orNone(from), active))
	}

	if reason, msg, failed := st.snap.failure(active); failed {
		to := st.snap.next(active)
		if to == "" {
			st.noFailoverTarget(active, fmt.Sprintf("switching %s -> %s: %s", orNone(st.sw.From), active, msg))
			return nil
		}
		return st.start(ctx, st.sw.From, to, reason, msg)
	}

	if a := st.egw.Status.ActiveNode; a != nil && a.Name == active && a.Ready {
		if err := st.moveLabel(ctx, active); err != nil {
			return err
		}
		return st.steady(ctx)
	}

	reason := v1alpha1.ReasonWaitingForGateway
	msg := fmt.Sprintf("switching %s -> %s: waiting for egress IPs on %s", orNone(st.sw.From), active, active)
	if gw := meta.FindStatusCondition(st.egw.Status.Conditions, v1alpha1.ConditionGatewayReady); gw != nil &&
		st.egw.Status.ActiveNode != nil && st.egw.Status.ActiveNode.Name == active && gw.Reason == v1alpha1.ReasonEgressIPConflict {
		reason = v1alpha1.ReasonEgressIPConflict
		msg = fmt.Sprintf("switching %s -> %s: %s", orNone(st.sw.From), active, gw.Message)
	}
	st.conds.set(v1alpha1.ConditionSwitching, metav1.ConditionTrue, reason, msg)
	if from == "" {
		st.conds.set(v1alpha1.ConditionActiveNodeElected, metav1.ConditionFalse, st.sw.Reason,
			fmt.Sprintf("waiting for %s to become the active node", active))
	}
	return nil
}

// steady handles an active node that already has the label: it finishes the
// label move, completes a recorded switch and detects failures.
func (st *state) steady(ctx context.Context) error {
	active := st.egw.Spec.ActiveNode
	if err := st.moveLabel(ctx, active); err != nil {
		return err
	}

	if st.sw != nil && st.sw.To == active {
		msg := fmt.Sprintf("%s became active node", active)
		if st.sw.From != "" {
			msg += fmt.Sprintf(", %s from %s", switchVerb(st.sw.Reason), st.sw.From)
		}
		if st.sw.Message != "" {
			msg += ": " + st.sw.Message
		}
		st.conds.set(v1alpha1.ConditionActiveNodeElected, metav1.ConditionTrue, st.sw.Reason, msg)
		st.conds.set(v1alpha1.ConditionSwitching, metav1.ConditionFalse, v1alpha1.ReasonCompleted,
			fmt.Sprintf("switched %s -> %s", orNone(st.sw.From), active))
		st.event(corev1.EventTypeNormal, "SwitchCompleted", msg)
		if err := st.setActive(ctx, active, nil); err != nil {
			return err
		}
	} else if c, ok := st.conds.get(v1alpha1.ConditionActiveNodeElected); !ok || c.Status != metav1.ConditionTrue {
		reason := v1alpha1.ReasonInitial
		if ok {
			reason = c.Reason
		}
		st.conds.set(v1alpha1.ConditionActiveNodeElected, metav1.ConditionTrue, reason, fmt.Sprintf("%s is the active node", active))
	}

	if reason, msg, failed := st.snap.failure(active); failed {
		to := st.snap.next(active)
		if to == "" {
			st.noFailoverTarget(active, msg)
			return nil
		}
		return st.start(ctx, active, to, reason, msg)
	}
	if c, ok := st.conds.get(v1alpha1.ConditionSwitching); !ok || c.Status == metav1.ConditionTrue {
		st.conds.set(v1alpha1.ConditionSwitching, metav1.ConditionFalse, v1alpha1.ReasonCompleted,
			fmt.Sprintf("%s is the active node", active))
	}
	return nil
}

func switchVerb(reason string) string {
	switch reason {
	case v1alpha1.ReasonManual:
		return "manual switch"
	case v1alpha1.ReasonCandidateRemoved:
		return "candidate removed"
	default:
		return "failover"
	}
}

// start begins phase 1 of a switch to node to.
func (st *state) start(ctx context.Context, from, to, reason, msg string) error {
	sw := &switchRecord{From: from, To: to, Reason: reason, Message: msg}
	if err := st.setActive(ctx, to, sw); err != nil {
		return err
	}
	metrics.SwitchesTotal.WithLabelValues(reason).Inc()
	st.conds.set(v1alpha1.ConditionSwitching, metav1.ConditionTrue, v1alpha1.ReasonWaitingForGateway,
		fmt.Sprintf("switching %s -> %s: waiting for egress IPs on %s", orNone(from), to, to))
	eventType := corev1.EventTypeWarning
	if reason == v1alpha1.ReasonInitial {
		eventType = corev1.EventTypeNormal
	}
	st.event(eventType, "SwitchStarted", fmt.Sprintf("switching %s -> %s (%s): %s", orNone(from), to, reason, msg))
	logf.FromContext(ctx).Info("switch started", "from", from, "to", to, "reason", reason, "message", msg)
	return nil
}

// rejectTarget restores spec.activeNode after a manual switch to an unavailable node.
func (st *state) rejectTarget(ctx context.Context, from, target, why string) error {
	if err := st.setActive(ctx, from, nil); err != nil {
		return err
	}
	msg := fmt.Sprintf("%s is not available (%s), spec.activeNode restored to %s", target, why, orNone(from))
	st.conds.set(v1alpha1.ConditionSwitching, metav1.ConditionFalse, v1alpha1.ReasonTargetUnavailable, msg)
	st.event(corev1.EventTypeWarning, v1alpha1.ReasonTargetUnavailable, msg)
	logf.FromContext(ctx).Info("manual switch rejected", "target", target, "reason", why)
	return nil
}

func (st *state) noCandidate(msg string) {
	if c, ok := st.conds.get(v1alpha1.ConditionSwitching); !ok || c.Reason != v1alpha1.ReasonNoAvailableCandidate || c.Message != msg {
		st.event(corev1.EventTypeWarning, v1alpha1.ReasonNoAvailableCandidate, msg)
	}
	st.conds.set(v1alpha1.ConditionSwitching, metav1.ConditionTrue, v1alpha1.ReasonNoAvailableCandidate, msg)
}

// noFailoverTarget keeps the failed active node. When no other candidate is
// reachable either, the controller's own network is the likely cause.
func (st *state) noFailoverTarget(active, msg string) {
	if st.snap.probe != nil && !st.snap.otherReachable(active) {
		st.noCandidate(msg + "; no other candidate is reachable from the controller either, " +
			"not switching (check the network of the controller node)")
		return
	}
	st.noCandidate(msg + ", and no other available candidate")
}

func (st *state) candidatesSummary() string {
	var out []string
	for name, n := range st.snap.nodes {
		if n.candidate {
			out = append(out, fmt.Sprintf("%s: %s", name, st.snap.unavailable(name)))
		}
	}
	if len(out) == 0 {
		return "no node matches candidateNodeSelector"
	}
	return fmt.Sprint(out)
}

// setActive writes spec.activeNode and the switch annotation (removed when sw
// is nil) in one patch, guarded by resourceVersion.
func (st *state) setActive(ctx context.Context, node string, sw *switchRecord) error {
	var ann any
	if sw != nil {
		b, err := json.Marshal(sw)
		if err != nil {
			return err
		}
		ann = string(b)
	}
	var activeNode any
	if node != "" {
		activeNode = node
	}
	if st.egw.Spec.ActiveNode == node && ((sw == nil && st.sw == nil) || (sw != nil && st.sw != nil && *sw == *st.sw)) {
		return nil
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"resourceVersion": st.egw.ResourceVersion,
			"annotations":     map[string]any{SwitchAnnotation: ann},
		},
		"spec": map[string]any{"activeNode": activeNode},
	})
	if err != nil {
		return err
	}
	if err := st.r.Client.Patch(ctx, st.egw, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("set spec.activeNode=%q: %w", node, err)
	}
	st.sw = sw
	return nil
}

func labelPatch(on bool) client.Patch {
	v := "null"
	if on {
		v = fmt.Sprintf("%q", v1alpha1.ActiveLabelValue)
	}
	return client.RawPatch(types.MergePatchType, fmt.Appendf(nil, `{"metadata":{"labels":{%q:%s}}}`, v1alpha1.ActiveLabelKey, v))
}

// moveLabel puts the active label on the Node and CiliumNode of active first,
// then removes it from all other nodes. CiliumNodes are patched directly
// because a dead node's cilium-agent does not sync Node labels.
func (st *state) moveLabel(ctx context.Context, active string) error {
	log := logf.FromContext(ctx)
	apply := func(n *nodeInfo, on bool) error {
		if n.exists && n.nodeLabel != on {
			if err := st.r.Client.Patch(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.name}}, labelPatch(on)); err != nil {
				return fmt.Errorf("label Node %s: %w", n.name, err)
			}
			n.nodeLabel = on
			log.Info("updated active label", "node", n.name, "kind", "Node", "active", on)
		}
		if n.hasCiliumNode && n.ciliumNodeLabel != on {
			if err := st.r.Client.Patch(ctx, &ciliumv2.CiliumNode{ObjectMeta: metav1.ObjectMeta{Name: n.name}}, labelPatch(on)); err != nil {
				return fmt.Errorf("label CiliumNode %s: %w", n.name, err)
			}
			n.ciliumNodeLabel = on
			log.Info("updated active label", "node", n.name, "kind", "CiliumNode", "active", on)
		}
		return nil
	}
	if err := apply(st.snap.node(active), true); err != nil {
		return err
	}
	for name, n := range st.snap.nodes {
		if name != active {
			if err := apply(n, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// finishConditions sets ActiveNodeReady and the summary Ready condition.
func (st *state) finishConditions() {
	active := st.egw.Spec.ActiveNode
	c := st.conds
	switch n := st.snap.node(active); {
	case active == "":
		c.set(v1alpha1.ConditionActiveNodeReady, metav1.ConditionUnknown, "NoActiveNode", "no active node selected")
	case !n.exists:
		c.set(v1alpha1.ConditionActiveNodeReady, metav1.ConditionFalse, "NodeNotFound", fmt.Sprintf("node %s not found", active))
	case n.ready:
		c.set(v1alpha1.ConditionActiveNodeReady, metav1.ConditionTrue, n.readyReason, fmt.Sprintf("%s is Ready", active))
	default:
		msg := fmt.Sprintf("%s NotReady since %s", active, n.readySince.UTC().Format(time.RFC3339))
		if n.readyMsg != "" {
			msg += ": " + n.readyMsg
		}
		c.set(v1alpha1.ConditionActiveNodeReady, metav1.ConditionFalse, n.readyReason, msg)
	}
	st.setReachable()
	if _, ok := c.get(v1alpha1.ConditionActiveNodeElected); !ok {
		c.set(v1alpha1.ConditionActiveNodeElected, metav1.ConditionFalse, v1alpha1.ReasonInitial, "no active node elected yet")
	}

	gw := metav1.Condition{Type: v1alpha1.ConditionGatewayReady, Status: metav1.ConditionUnknown,
		Message: fmt.Sprintf("not reported by the agent on %s yet", orNone(active))}
	if a := st.egw.Status.ActiveNode; a != nil && a.Name == active && active != "" {
		if g := meta.FindStatusCondition(st.egw.Status.Conditions, v1alpha1.ConditionGatewayReady); g != nil {
			gw = *g
		}
	}

	notReady := func(t string, cond metav1.Condition, want metav1.ConditionStatus) bool {
		if cond.Status == want {
			return false
		}
		c.set(v1alpha1.ConditionReady, metav1.ConditionFalse, t, fmt.Sprintf("%s: %s", t, cond.Message))
		return true
	}
	get := func(t string) metav1.Condition { cond, _ := c.get(t); return cond }
	switch {
	case notReady(v1alpha1.ConditionActiveNodeElected, get(v1alpha1.ConditionActiveNodeElected), metav1.ConditionTrue):
	case notReady(v1alpha1.ConditionSwitching, get(v1alpha1.ConditionSwitching), metav1.ConditionFalse):
	case c.has(v1alpha1.ConditionActiveNodeReachable) &&
		notReady(v1alpha1.ConditionActiveNodeReachable, get(v1alpha1.ConditionActiveNodeReachable), metav1.ConditionTrue):
	case notReady(v1alpha1.ConditionActiveNodeReady, get(v1alpha1.ConditionActiveNodeReady), metav1.ConditionTrue):
	case notReady(v1alpha1.ConditionGatewayReady, gw, metav1.ConditionTrue):
	default:
		c.set(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonAsExpected,
			fmt.Sprintf("%s is the active node and its egress IPs are configured", active))
	}
}

// setReachable sets ActiveNodeReachable from the probe of the active node. A
// result that is not available yet, e.g. right after a controller restart,
// keeps the previous condition so that Ready does not flap.
func (st *state) setReachable() {
	if st.r.Prober == nil {
		return
	}
	active := st.egw.Spec.ActiveNode
	if active == "" {
		st.conds.set(v1alpha1.ConditionActiveNodeReachable, metav1.ConditionUnknown, "NoActiveNode", "no active node selected")
		return
	}
	res, ok := st.r.Prober.Get(active)
	switch {
	case ok && res.Probed && res.Failed():
		st.conds.set(v1alpha1.ConditionActiveNodeReachable, metav1.ConditionFalse, v1alpha1.ReasonProbeFailed, res.Message())
	case ok && res.Probed:
		st.conds.set(v1alpha1.ConditionActiveNodeReachable, metav1.ConditionTrue, v1alpha1.ReasonProbeSucceeded, res.Message())
	case !st.conds.has(v1alpha1.ConditionActiveNodeReachable):
		st.conds.set(v1alpha1.ConditionActiveNodeReachable, metav1.ConditionUnknown, "Probing",
			fmt.Sprintf("TCP probe to %s not finished yet", active))
	}
}

// candidates returns status.candidates: the TCP probe result of each candidate.
func (st *state) candidates() []v1alpha1.CandidateStatus {
	if st.r.Prober == nil {
		return nil
	}
	var out []v1alpha1.CandidateStatus
	for _, name := range st.snap.candidateNames() {
		res, _ := st.r.Prober.Get(name)
		out = append(out, v1alpha1.CandidateStatus{Name: name, Reachable: res.Reachable()})
	}
	return out
}

func (st *state) applyStatus(ctx context.Context) error {
	conds, changed := st.conds.result(st.egw.Generation)
	cands := st.candidates()
	if !changed && slices.Equal(cands, st.egw.Status.Candidates) {
		return nil
	}
	return st.r.Status.Apply(ctx, conds, cands)
}
