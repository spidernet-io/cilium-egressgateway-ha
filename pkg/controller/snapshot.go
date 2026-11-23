package controller

import (
	"fmt"
	"slices"
	"strings"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
)

// nodeInfo is everything the controller knows about one node.
type nodeInfo struct {
	name string
	// exists is false when only the CiliumNode is left.
	exists      bool
	candidate   bool
	ready       bool
	readyReason string
	readyMsg    string
	readySince  metav1.Time
	ciliumReady bool
	agentReady  bool
	// nodeLabel and ciliumNodeLabel report the active label on Node and CiliumNode.
	nodeLabel       bool
	ciliumNodeLabel bool
	hasCiliumNode   bool
}

func (n *nodeInfo) labelled() bool { return n.nodeLabel || n.ciliumNodeLabel }

// snapshot is the cluster state used by one reconcile.
type snapshot struct {
	nodes map[string]*nodeInfo
	// reachable reports TCP probe results; nil treats every node as reachable.
	reachable func(name string) (ok bool, msg string)
}

func isActive(l map[string]string) bool {
	return l[v1alpha1.ActiveLabelKey] == v1alpha1.ActiveLabelValue
}

func podReady(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func newSnapshot(sel labels.Selector, nodes []corev1.Node, cns []ciliumv2.CiliumNode, ciliumPods, agentPods []corev1.Pod) *snapshot {
	s := &snapshot{nodes: map[string]*nodeInfo{}}
	get := func(name string) *nodeInfo {
		n, ok := s.nodes[name]
		if !ok {
			n = &nodeInfo{name: name}
			s.nodes[name] = n
		}
		return n
	}
	for i := range nodes {
		node := &nodes[i]
		n := get(node.Name)
		n.exists = true
		n.candidate = sel.Matches(labels.Set(node.Labels))
		n.nodeLabel = isActive(node.Labels)
		n.readyReason, n.readyMsg = "NodeReadyUnknown", "Ready condition not reported"
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady {
				n.ready = c.Status == corev1.ConditionTrue
				n.readyReason, n.readyMsg, n.readySince = c.Reason, c.Message, c.LastTransitionTime
			}
		}
	}
	for i := range cns {
		n := get(cns[i].Name)
		n.hasCiliumNode = true
		n.ciliumNodeLabel = isActive(cns[i].Labels)
	}
	for i := range ciliumPods {
		if p := &ciliumPods[i]; podReady(p) {
			get(p.Spec.NodeName).ciliumReady = true
		}
	}
	for i := range agentPods {
		if p := &agentPods[i]; podReady(p) {
			get(p.Spec.NodeName).agentReady = true
		}
	}
	return s
}

// setLabels overrides the active label state with fresh reads.
func (s *snapshot) setLabels(nodes []corev1.Node, cns []ciliumv2.CiliumNode) {
	for _, n := range s.nodes {
		n.nodeLabel, n.ciliumNodeLabel = false, false
	}
	for i := range nodes {
		n := s.node(nodes[i].Name)
		if _, ok := s.nodes[n.name]; !ok {
			// Not in the cache yet: it will show up in a later reconcile.
			continue
		}
		n.nodeLabel = isActive(nodes[i].Labels)
	}
	for i := range cns {
		if n, ok := s.nodes[cns[i].Name]; ok {
			n.ciliumNodeLabel = isActive(cns[i].Labels)
			n.hasCiliumNode = true
		}
	}
}

func (s *snapshot) node(name string) *nodeInfo {
	if n, ok := s.nodes[name]; ok {
		return n
	}
	return &nodeInfo{name: name}
}

// labelled returns the sorted names of nodes with the active label on Node or CiliumNode.
func (s *snapshot) labelled() []string {
	var out []string
	for name, n := range s.nodes {
		if n.labelled() {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// failure reports why the active node must be replaced. Only node-level
// signals count: agent and cilium-agent readiness never trigger a switch.
func (s *snapshot) failure(name string) (reason, msg string, failed bool) {
	n := s.node(name)
	switch {
	case !n.exists:
		return v1alpha1.ReasonFailover, fmt.Sprintf("%s: node not found", name), true
	case !n.candidate:
		return v1alpha1.ReasonCandidateRemoved, fmt.Sprintf("%s no longer matches candidateNodeSelector", name), true
	case !n.ready:
		return v1alpha1.ReasonFailover, fmt.Sprintf("%s NotReady (%s)", name, n.readyReason), true
	}
	if s.reachable != nil {
		if ok, msg := s.reachable(name); !ok {
			return v1alpha1.ReasonFailover, fmt.Sprintf("%s: %s", name, msg), true
		}
	}
	return "", "", false
}

// unavailable returns why a node cannot become the active node, or "".
func (s *snapshot) unavailable(name string) string {
	n := s.node(name)
	var why []string
	switch {
	case !n.exists:
		return "node not found"
	case !n.candidate:
		return "does not match candidateNodeSelector"
	}
	if !n.ready {
		why = append(why, "node NotReady")
	}
	if !n.hasCiliumNode {
		why = append(why, "CiliumNode not found")
	}
	if !n.ciliumReady {
		why = append(why, "cilium-agent not ready")
	}
	if !n.agentReady {
		why = append(why, "egressgateway-ha agent not ready")
	}
	if s.reachable != nil {
		if ok, msg := s.reachable(name); !ok {
			why = append(why, msg)
		}
	}
	return strings.Join(why, ", ")
}

// available returns the sorted names of nodes that can become the active node.
func (s *snapshot) available() []string {
	var out []string
	for name := range s.nodes {
		if s.unavailable(name) == "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// next returns the first available node after current in name order, wrapping
// around, and never current itself. An empty current returns the first one.
func (s *snapshot) next(current string) string {
	avail := slices.DeleteFunc(s.available(), func(n string) bool { return n == current })
	if len(avail) == 0 {
		return ""
	}
	for _, n := range avail {
		if n > current {
			return n
		}
	}
	return avail[0]
}
