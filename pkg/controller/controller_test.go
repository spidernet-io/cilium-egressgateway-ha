package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/k8s"
)

const (
	ciliumNS = "kube-system"
	agentNS  = "egw"
)

type fakeStatus struct {
	c       client.Client
	applies int
}

func (s *fakeStatus) Apply(ctx context.Context, conds []metav1.Condition) error {
	s.applies++
	egw := &v1alpha1.EgressGatewayHA{}
	if err := s.c.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, egw); err != nil {
		return err
	}
	for _, t := range ownedConditions {
		meta.RemoveStatusCondition(&egw.Status.Conditions, t)
	}
	egw.Status.Conditions = append(egw.Status.Conditions, conds...)
	return s.c.Status().Update(ctx, egw)
}

type testNode struct {
	name        string
	candidate   bool
	ready       bool
	ciliumReady bool
	agentReady  bool
}

func healthy(name string) testNode { return testNode{name, true, true, true, true} }

func objects(nodes ...testNode) []client.Object {
	var objs []client.Object
	cond := func(ok bool) corev1.ConditionStatus {
		if ok {
			return corev1.ConditionTrue
		}
		return corev1.ConditionFalse
	}
	for _, n := range nodes {
		l := map[string]string{}
		if n.candidate {
			l["candidate"] = "true"
		}
		reason := "KubeletReady"
		if !n.ready {
			reason = "NodeStatusUnknown"
		}
		objs = append(objs,
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.name, Labels: l}, Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: cond(n.ready), Reason: reason}}}},
			&ciliumv2.CiliumNode{ObjectMeta: metav1.ObjectMeta{Name: n.name}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cilium-" + n.name, Namespace: ciliumNS, Labels: map[string]string{"k8s-app": "cilium"}},
				Spec: corev1.PodSpec{NodeName: n.name}, Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond(n.ciliumReady)}}}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + n.name, Namespace: agentNS, Labels: map[string]string{"app": "agent"}},
				Spec: corev1.PodSpec{NodeName: n.name}, Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond(n.agentReady)}}}},
		)
	}
	return objs
}

func newEGWHA(active string) *v1alpha1.EgressGatewayHA {
	return &v1alpha1.EgressGatewayHA{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName, Generation: 1},
		Spec: v1alpha1.EgressGatewayHASpec{
			CandidateNodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"candidate": "true"}},
			Interface:             "eth0",
			EgressIPs:             []string{"10.0.0.100"},
			ActiveNode:            active,
		},
	}
}

type env struct {
	t        *testing.T
	c        client.Client
	r        *Reconciler
	status   *fakeStatus
	recorder *events.FakeRecorder
}

func newEnv(t *testing.T, egw *v1alpha1.EgressGatewayHA, nodes ...testNode) *env {
	t.Helper()
	objs := objects(nodes...)
	if egw != nil {
		objs = append(objs, egw)
	}
	c := fake.NewClientBuilder().WithScheme(k8s.NewScheme()).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.EgressGatewayHA{}).Build()
	e := &env{t: t, c: c, status: &fakeStatus{c: c}, recorder: events.NewFakeRecorder(100)}
	e.r = &Reconciler{Client: c, Status: e.status, Recorder: e.recorder, Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
		Config: Config{
			CiliumNamespace: ciliumNS, CiliumSelector: labels.SelectorFromSet(labels.Set{"k8s-app": "cilium"}),
			AgentNamespace: agentNS, AgentSelector: labels.SelectorFromSet(labels.Set{"app": "agent"}),
			ResyncInterval: 10 * time.Second,
		}}
	return e
}

func (e *env) reconcile() {
	e.t.Helper()
	res, err := e.r.Reconcile(context.Background(), reconcile.Request{})
	if err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 10*time.Second {
		e.t.Fatalf("RequeueAfter = %v", res.RequeueAfter)
	}
}

func (e *env) egw() *v1alpha1.EgressGatewayHA {
	e.t.Helper()
	egw := &v1alpha1.EgressGatewayHA{}
	if err := e.c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.SingletonName}, egw); err != nil {
		e.t.Fatal(err)
	}
	return egw
}

func (e *env) switchRecord() *switchRecord {
	v, ok := e.egw().Annotations[SwitchAnnotation]
	if !ok {
		return nil
	}
	sw := &switchRecord{}
	if err := json.Unmarshal([]byte(v), sw); err != nil {
		e.t.Fatal(err)
	}
	return sw
}

// agentReports plays the agent on node: it writes status.activeNode and GatewayReady.
func (e *env) agentReports(node string, ready bool, reason, msg string) {
	e.t.Helper()
	egw := e.egw()
	egw.Status.ActiveNode = &v1alpha1.ActiveNodeStatus{Name: node, Ready: ready, EgressIPs: []string{"10.0.0.100"}}
	status := metav1.ConditionTrue
	if !ready {
		status = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&egw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionGatewayReady, Status: status, Reason: reason, Message: msg})
	if err := e.c.Status().Update(context.Background(), egw); err != nil {
		e.t.Fatal(err)
	}
}

// labelled returns the nodes with the active label as "name" when both Node
// and CiliumNode have it, or "name(Node)" / "name(CiliumNode)" when only one does.
func (e *env) labelled() string {
	e.t.Helper()
	ctx := context.Background()
	nodes, cns := &corev1.NodeList{}, &ciliumv2.CiliumNodeList{}
	if err := e.c.List(ctx, nodes); err != nil {
		e.t.Fatal(err)
	}
	if err := e.c.List(ctx, cns); err != nil {
		e.t.Fatal(err)
	}
	onCN := map[string]bool{}
	for _, cn := range cns.Items {
		onCN[cn.Name] = isActive(cn.Labels)
	}
	var out []string
	for _, n := range nodes.Items {
		switch on := isActive(n.Labels); {
		case on && onCN[n.Name]:
			out = append(out, n.Name)
		case on:
			out = append(out, n.Name+"(Node)")
		case onCN[n.Name]:
			out = append(out, n.Name+"(CiliumNode)")
		}
	}
	return strings.Join(out, ",")
}

func (e *env) setLabel(obj client.Object, on bool) {
	e.t.Helper()
	if err := e.c.Patch(context.Background(), obj, labelPatch(on)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) labelNode(name string) {
	e.setLabel(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, true)
	e.setLabel(&ciliumv2.CiliumNode{ObjectMeta: metav1.ObjectMeta{Name: name}}, true)
}

func (e *env) setNodeReady(name string, ready bool) {
	e.t.Helper()
	n := &corev1.Node{}
	if err := e.c.Get(context.Background(), client.ObjectKey{Name: name}, n); err != nil {
		e.t.Fatal(err)
	}
	n.Status.Conditions[0].Status = corev1.ConditionFalse
	n.Status.Conditions[0].Reason = "NodeStatusUnknown"
	n.Status.Conditions[0].Message = "Kubelet stopped posting node status."
	if ready {
		n.Status.Conditions[0].Status = corev1.ConditionTrue
		n.Status.Conditions[0].Reason = "KubeletReady"
	}
	if err := e.c.Status().Update(context.Background(), n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) setPodReady(ns, name string, ready bool) {
	e.t.Helper()
	p := &corev1.Pod{}
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, p); err != nil {
		e.t.Fatal(err)
	}
	p.Status.Conditions[0].Status = corev1.ConditionFalse
	if ready {
		p.Status.Conditions[0].Status = corev1.ConditionTrue
	}
	if err := e.c.Status().Update(context.Background(), p); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) cond(t string) metav1.Condition {
	e.t.Helper()
	c := meta.FindStatusCondition(e.egw().Status.Conditions, t)
	if c == nil {
		e.t.Fatalf("condition %s not found", t)
	}
	return *c
}

func (e *env) expectCond(t string, status metav1.ConditionStatus, reason, msgSubstr string) {
	e.t.Helper()
	c := e.cond(t)
	if c.Status != status || c.Reason != reason || !strings.Contains(c.Message, msgSubstr) {
		e.t.Fatalf("%s = %s/%s %q, want %s/%s containing %q", t, c.Status, c.Reason, c.Message, status, reason, msgSubstr)
	}
}

func (e *env) expect(active, labelled string) {
	e.t.Helper()
	if got := e.egw().Spec.ActiveNode; got != active {
		e.t.Fatalf("spec.activeNode = %q, want %q", got, active)
	}
	if got := e.labelled(); got != labelled {
		e.t.Fatalf("labelled = %q, want %q", got, labelled)
	}
}

func (e *env) events() string {
	var out []string
	for {
		select {
		case ev := <-e.recorder.Events:
			out = append(out, ev)
		default:
			return strings.Join(out, "\n")
		}
	}
}

// steadyEnv returns an environment where active already has the label and is ready.
func steadyEnv(t *testing.T, active string, nodes ...testNode) *env {
	e := newEnv(t, newEGWHA(active), nodes...)
	e.labelNode(active)
	e.agentReports(active, true, v1alpha1.ReasonEgressIPsConfigured, "configured")
	e.reconcile()
	e.expect(active, active)
	e.expectCond(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonAsExpected, "")
	e.events()
	return e
}

func TestNext(t *testing.T) {
	s := newSnapshot(labels.Everything(), nil, nil, nil, nil)
	for _, n := range []string{"n1", "n2", "n3", "n4"} {
		s.nodes[n] = &nodeInfo{name: n, exists: true, candidate: true, ready: true, hasCiliumNode: true, ciliumReady: true, agentReady: true}
	}
	s.nodes["n4"].agentReady = false
	for cur, want := range map[string]string{"": "n1", "n1": "n2", "n2": "n3", "n3": "n1", "n4": "n1", "n0": "n1", "n9": "n1"} {
		if got := s.next(cur); got != want {
			t.Errorf("next(%q) = %q, want %q", cur, got, want)
		}
	}
	s.nodes["n1"].ready, s.nodes["n3"].ready = false, false
	if got := s.next("n2"); got != "" {
		t.Errorf("next(n2) with no other available node = %q", got)
	}
}

func TestInitialElection(t *testing.T) {
	// n1 sorts first but its agent is not ready, n4 is not a candidate.
	e := newEnv(t, newEGWHA(""), testNode{"n1", true, true, true, false}, healthy("n3"), healthy("n2"),
		testNode{"n4", false, true, true, true})

	e.reconcile()
	e.expect("n2", "")
	if sw := e.switchRecord(); sw == nil || *sw != (switchRecord{To: "n2", Reason: v1alpha1.ReasonInitial,
		Message: "n2 selected as the initial active node"}) {
		t.Fatalf("switch record = %+v", sw)
	}
	e.expectCond(v1alpha1.ConditionSwitching, metav1.ConditionTrue, v1alpha1.ReasonWaitingForGateway, "switching <none> -> n2: waiting for egress IPs on n2")
	e.expectCond(v1alpha1.ConditionActiveNodeElected, metav1.ConditionFalse, v1alpha1.ReasonInitial, "")
	e.expectCond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ConditionActiveNodeElected, "")
	if ev := e.events(); !strings.Contains(ev, "Normal SwitchStarted") {
		t.Errorf("events = %q", ev)
	}

	// Still waiting while the agent has not reported.
	e.reconcile()
	e.expect("n2", "")

	// Phase 2 once the agent on n2 is ready.
	e.agentReports("n2", true, v1alpha1.ReasonEgressIPsConfigured, "1 egress IP(s) configured")
	e.reconcile()
	e.expect("n2", "n2")
	if e.switchRecord() != nil {
		t.Error("switch annotation not removed after completion")
	}
	e.expectCond(v1alpha1.ConditionActiveNodeElected, metav1.ConditionTrue, v1alpha1.ReasonInitial, "n2 became active node: n2 selected")
	e.expectCond(v1alpha1.ConditionSwitching, metav1.ConditionFalse, v1alpha1.ReasonCompleted, "switched <none> -> n2")
	e.expectCond(v1alpha1.ConditionActiveNodeReady, metav1.ConditionTrue, "KubeletReady", "n2 is Ready")
	e.expectCond(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonAsExpected, "")
	if ev := e.events(); !strings.Contains(ev, "Normal SwitchCompleted") {
		t.Errorf("events = %q", ev)
	}

	// A steady reconcile writes nothing.
	applies := e.status.applies
	e.reconcile()
	if e.status.applies != applies {
		t.Error("status re-applied without changes")
	}
}

func TestNoCandidateAtStart(t *testing.T) {
	e := newEnv(t, newEGWHA(""), testNode{"n1", true, true, false, true})
	e.reconcile()
	e.expect("", "")
	e.expectCond(v1alpha1.ConditionSwitching, metav1.ConditionTrue, v1alpha1.ReasonNoAvailableCandidate, "n1: cilium-agent not ready")
	e.expectCond(v1alpha1.ConditionActiveNodeReady, metav1.ConditionUnknown, "NoActiveNode", "")
}

func TestFailover(t *testing.T) {
	e := steadyEnv(t, "n2", healthy("n1"), healthy("n2"), healthy("n3"))

	e.setNodeReady("n2", false)
	e.reconcile()
	// Phase 1: the label stays on n2 until n3 reports ready.
	e.expect("n3", "n2")
	if sw := e.switchRecord(); sw == nil || sw.From != "n2" || sw.Reason != v1alpha1.ReasonFailover || sw.Message != "n2 NotReady (NodeStatusUnknown)" {
		t.Fatalf("switch record = %+v", sw)
	}
	e.expectCond(v1alpha1.ConditionSwitching, metav1.ConditionTrue, v1alpha1.ReasonWaitingForGateway, "switching n2 -> n3")
	e.expectCond(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ConditionSwitching, "Switching: switching n2 -> n3")
	if ev := e.events(); !strings.Contains(ev, "Warning SwitchStarted switching n2 -> n3 (Failover)") {
		t.Errorf("events = %q", ev)
	}

	// The old status written by n2 must not count as n3 being ready.
	e.reconcile()
	e.expect("n3", "n2")

	e.agentReports("n3", true, v1alpha1.ReasonEgressIPsConfigured, "configured")
	e.reconcile()
	e.expect("n3", "n3")
	e.expectCond(v1alpha1.ConditionActiveNodeElected, metav1.ConditionTrue, v1alpha1.ReasonFailover,
		"n3 became active node, failover from n2: n2 NotReady (NodeStatusUnknown)")
	e.expectCond(v1alpha1.ConditionReady, metav1.ConditionTrue, v1alpha1.ReasonAsExpected, "")

	// The recovered node does not get the role back.
	e.setNodeReady("n2", true)
	e.reconcile()
	e.expect("n3", "n3")

	// Wrap around from the last node to the first.
	e.setNodeReady("n3", false)
	e.reconcile()
	e.expect("n1", "n3")
}

func TestCandidateRemoved(t *testing.T) {
	e := steadyEnv(t, "n1", healthy("n1"), healthy("n2"))
	n := &corev1.Node{}
	if err := e.c.Get(context.Background(), client.ObjectKey{Name: "n1"}, n); err != nil {
		t.Fatal(err)
	}
	delete(n.Labels, "candidate")
	if err := e.c.Update(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.expect("n2", "n1")
	if sw := e.switchRecord(); sw.Reason != v1alpha1.ReasonCandidateRemoved {
		t.Fatalf("switch record = %+v", sw)
	}
}

func TestComponentsNotReadyDoNotSwitch(t *testing.T) {
	e := steadyEnv(t, "n1", healthy("n1"), healthy("n2"))
	e.setPodReady(agentNS, "agent-n1", false)
	e.setPodReady(ciliumNS, "cilium-n1", false)
	e.reconcile()
	e.expect("n1", "n1")
	e.expectCond(v1alpha1.ConditionSwitching, metav1.ConditionFalse, v1alpha1.ReasonCompleted, "")
}

func TestNoAvailableCandidateKeepsActive(t *testing.T) {
	e := steadyEnv(t, "n1", healthy("n1"), testNode{"n2", true, true, true, false})
	e.setNodeReady("n1", false)
	e.reconcile()
	e.expect("n1", "n1")
	e.expectCond(v1alpha1.ConditionSwitching, metav1.ConditionTrue, v1alpha1.ReasonNoAvailableCandidate,
		"n1 NotReady (NodeStatusUnknown), and no other available candidate")
	e.expectCond(v1alpha1.ConditionActiveNodeReady, metav1.ConditionFalse, "NodeStatusUnknown", "Kubelet stopped posting node status.")
	if ev := e.events(); strings.Count(ev, v1alpha1.ReasonNoAvailableCandidate) != 1 {
		t.Errorf("events = %q", ev)
	}
	e.reconcile()
	if ev := e.events(); ev != "" {
		t.Errorf("repeated event: %q", ev)
	}

	// n2 becomes available: switch now.
	e.setPodReady(agentNS, "agent-n2", true)
	e.reconcile()
	e.expect("n2", "n1")

	// n1 recovers before n2 got ready: no switch back is forced.
	e.setNodeReady("n1", true)
	e.reconcile()
	e.expect("n2", "n1")
}

func TestManualSwitch(t *testing.T) {
	e := steadyEnv(t, "n1", healthy("n1"), healthy("n2"), healthy("n3"))
	egw := e.egw()
	egw.Spec.ActiveNode = "n3"
	if err := e.c.Update(context.Background(), egw); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.expect("n3", "n1")
	if sw := e.switchRecord(); sw == nil || sw.From != "n1" || sw.To != "n3" || sw.Reason != v1alpha1.ReasonManual {
		t.Fatalf("switch record = %+v", sw)
	}
	e.agentReports("n3", true, v1alpha1.ReasonEgressIPsConfigured, "configured")
	e.reconcile()
	e.expect("n3", "n3")
	e.expectCond(v1alpha1.ConditionActiveNodeElected, metav1.ConditionTrue, v1alpha1.ReasonManual,
		"n3 became active node, manual switch from n1")
}

func TestManualSwitchToUnavailableNode(t *testing.T) {
	e := steadyEnv(t, "n1", healthy("n1"), testNode{"n2", true, true, true, false})
	egw := e.egw()
	egw.Spec.ActiveNode = "n2"
	if err := e.c.Update(context.Background(), egw); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.expect("n1", "n1")
	e.expectCond(v1alpha1.ConditionSwitching, metav1.ConditionFalse, v1alpha1.ReasonTargetUnavailable,
		"n2 is not available (egressgateway-ha agent not ready), spec.activeNode restored to n1")
	if ev := e.events(); !strings.Contains(ev, "Warning TargetUnavailable") {
		t.Errorf("events = %q", ev)
	}
	// The reason stays visible in the next reconciles.
	e.reconcile()
	e.expectCond(v1alpha1.ConditionSwitching, metav1.ConditionFalse, v1alpha1.ReasonTargetUnavailable, "")
}

func TestTargetFailsDuringSwitch(t *testing.T) {
	e := steadyEnv(t, "n1", healthy("n1"), healthy("n2"), healthy("n3"))
	e.setNodeReady("n1", false)
	e.reconcile()
	e.expect("n2", "n1")
	e.setNodeReady("n2", false)
	e.reconcile()
	e.expect("n3", "n1")
	if sw := e.switchRecord(); sw.From != "n1" || sw.To != "n3" {
		t.Fatalf("switch record = %+v", sw)
	}
}

func TestConflictWhileWaiting(t *testing.T) {
	e := steadyEnv(t, "n1", healthy("n1"), healthy("n2"))
	e.setNodeReady("n1", false)
	e.reconcile()
	e.agentReports("n2", false, v1alpha1.ReasonEgressIPConflict, "10.0.0.100 is answered by aa:bb:cc:dd:ee:ff, not added")
	e.reconcile()
	e.expect("n2", "n1")
	e.expectCond(v1alpha1.ConditionSwitching, metav1.ConditionTrue, v1alpha1.ReasonEgressIPConflict,
		"switching n1 -> n2: 10.0.0.100 is answered by aa:bb:cc:dd:ee:ff, not added")
}

func TestResumeLabelMove(t *testing.T) {
	// The controller stopped after labelling n2 but before removing n1's labels.
	egw := newEGWHA("n2")
	egw.Annotations = map[string]string{SwitchAnnotation: `{"from":"n1","to":"n2","reason":"Failover","message":"n1 NotReady"}`}
	e := newEnv(t, egw, healthy("n1"), healthy("n2"))
	e.labelNode("n1")
	e.setLabel(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}}, true)
	e.expect("n2", "n1,n2(Node)")
	e.reconcile()
	e.expect("n2", "n2")
	e.expectCond(v1alpha1.ConditionActiveNodeElected, metav1.ConditionTrue, v1alpha1.ReasonFailover, "failover from n1")
}

func TestStrayLabelsRemoved(t *testing.T) {
	e := steadyEnv(t, "n1", healthy("n1"), healthy("n2"))
	e.setLabel(&ciliumv2.CiliumNode{ObjectMeta: metav1.ObjectMeta{Name: "n2"}}, true)
	e.reconcile()
	e.expect("n1", "n1")
}

func TestClearedActiveNodeKeepsLabelledNode(t *testing.T) {
	e := steadyEnv(t, "n2", healthy("n1"), healthy("n2"))
	egw := e.egw()
	egw.Spec.ActiveNode = ""
	if err := e.c.Update(context.Background(), egw); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.expect("n2", "n2")
}

func TestCreateDefault(t *testing.T) {
	e := newEnv(t, nil, healthy("n1"))
	e.reconcile() // no default spec: nothing happens
	if err := e.c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.SingletonName}, &v1alpha1.EgressGatewayHA{}); err == nil {
		t.Fatal("created without a default spec")
	}
	spec := newEGWHA("n9").Spec
	e.r.Config.Default = &spec
	e.reconcile()
	if got := e.egw(); got.Spec.ActiveNode != "" || got.Spec.Interface != "eth0" {
		t.Fatalf("created spec = %+v", got.Spec)
	}
}

func TestReachability(t *testing.T) {
	e := steadyEnv(t, "n1", healthy("n1"), healthy("n2"))
	e.r.Reachable = func(name string) (bool, string) {
		if name == "n1" {
			return false, "3/3 probes to 10.0.0.1:10250 failed: i/o timeout"
		}
		return true, ""
	}
	e.reconcile()
	e.expect("n2", "n1")
	if sw := e.switchRecord(); sw.Message != "n1: 3/3 probes to 10.0.0.1:10250 failed: i/o timeout" {
		t.Fatalf("switch record = %+v", sw)
	}
}

func TestReadsEGWHAAndLabelsWithoutCache(t *testing.T) {
	e := steadyEnv(t, "n1", healthy("n1"), healthy("n2"))
	// The cached client must not be used to read EgressGatewayHA or the active labels.
	e.r.Reader = e.c
	e.r.Client = interceptor.NewClient(e.c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha1.EgressGatewayHA); ok {
				return fmt.Errorf("EgressGatewayHA read from the cache")
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lo := &client.ListOptions{}
			lo.ApplyOptions(opts)
			if lo.LabelSelector != nil && !lo.LabelSelector.Empty() {
				if _, ok := list.(*corev1.PodList); !ok {
					return fmt.Errorf("active labels read from the cache")
				}
			}
			return c.List(ctx, list, opts...)
		},
	})
	e.setNodeReady("n1", false)
	e.reconcile()
	e.expect("n2", "n1")
}
