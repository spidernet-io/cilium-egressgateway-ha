package controller

import "github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"

// nodeLogState is the per-node state compared between reconciles to log changes.
type nodeLogState struct {
	egressNode  bool
	ready       bool
	probed      bool
	failures    int
	failed      bool
	unavailable string
}

// logNodeChanges logs at INFO when a node joins or leaves the egress nodes,
// its Ready status changes, or its TCP probe starts failing, reaches the
// failure threshold or recovers. Each probe failure and availability changes
// are logged at DEBUG.
func (st *state) logNodeChanges() {
	if st.r.seen == nil {
		st.r.seen = map[string]nodeLogState{}
	}
	names := map[string]bool{}
	for name := range st.r.seen {
		names[name] = true
	}
	for _, name := range st.snap.egressNodeNames() {
		names[name] = true
	}
	if a := st.egw.Spec.ActiveNode; a != "" {
		names[a] = true
	}
	for name := range names {
		n := st.snap.node(name)
		cur := nodeLogState{egressNode: n.exists && n.egressNode, ready: n.ready}
		var addr, lastErr string
		if st.snap.probe != nil {
			if r, ok := st.snap.probe(name); ok {
				cur.probed, cur.failures, cur.failed = r.Probed, r.ConsecutiveFailures, r.Probed && r.Failed()
				addr, lastErr = r.Addr, r.LastError
			}
		}
		if cur.egressNode {
			cur.unavailable = st.snap.unavailable(name)
		}
		prev, had := st.r.seen[name]
		log := st.log.WithValues("node", name)

		switch {
		case !n.exists && had && prev.egressNode:
			log.Info("egress node removed: node not found")
		case cur.egressNode && (!had || !prev.egressNode):
			log.Info("node is an egress node", "ready", cur.ready)
		case !cur.egressNode && had && prev.egressNode:
			log.Info("node is no longer an egress node")
		}
		if had && n.exists && prev.ready != cur.ready {
			log.Info("node Ready changed", "ready", cur.ready, "reason", n.readyReason, "message", n.readyMsg)
		}
		if cur.probed && (!had || prev.failures != cur.failures) {
			switch {
			case cur.failures == 0 && had && prev.failures > 0:
				log.Info("TCP probe recovered", "addr", addr, "previousFailures", prev.failures)
			case cur.failures > 0 && (!had || prev.failures == 0):
				log.Info("TCP probe failed", "addr", addr, "failures", cur.failures, "error", lastErr)
			case cur.failures > 0:
				log.V(1).Info("TCP probe failed", "addr", addr, "failures", cur.failures, "error", lastErr)
			}
			if cur.failed && (!had || !prev.failed) {
				log.Info("TCP probe failure threshold reached, node is unreachable", "addr", addr,
					"failures", cur.failures, "error", lastErr)
			}
		}
		if cur.egressNode && (!had || prev.unavailable != cur.unavailable) {
			if cur.unavailable == "" {
				log.V(1).Info("egress node available")
			} else {
				log.V(1).Info("egress node unavailable", "why", cur.unavailable)
			}
		}

		if !n.exists && !cur.egressNode {
			delete(st.r.seen, name)
			continue
		}
		st.r.seen[name] = cur
	}
}

// logSummary logs the decision of this reconcile at DEBUG.
func (st *state) logSummary() {
	if !st.log.V(1).Enabled() {
		return
	}
	nodes := map[string]string{}
	for _, name := range st.snap.egressNodeNames() {
		why := st.snap.unavailable(name)
		if why == "" {
			why = "available"
		}
		nodes[name] = why
	}
	kv := []any{"activeNode", st.egw.Spec.ActiveNode, "labelled", st.snap.labelled(), "egressNodes", nodes}
	if st.sw != nil {
		kv = append(kv, "switch", *st.sw)
	}
	if c, ok := st.conds.get(v1alpha1.ConditionSwitching); ok {
		kv = append(kv, "switching", c.Status, "switchingReason", c.Reason)
	}
	st.log.V(1).Info("reconciled", kv...)
}
