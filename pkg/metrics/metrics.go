// Package metrics defines the Prometheus metrics of the controller and the
// agent. They are registered in the controller-runtime registry and served on
// the manager metrics endpoint.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "egressgateway_ha"

// Controller metrics. Only the elected replica updates them.
var (
	SwitchesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "switches_total",
		Help: "Started switches of the active node, by reason (Initial, Failover, Manual, CandidateRemoved).",
	}, []string{"reason"})

	ActiveNode = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "active_node",
		Help: "1 for the node in spec.activeNode.",
	}, []string{"node"})

	Condition = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "condition",
		Help: "1 for the current status and reason of each EgressGatewayHA condition.",
	}, []string{"type", "status", "reason"})

	CandidateReachable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "candidate_reachable",
		Help: "1 when the last TCP probe of the candidate node succeeded.",
	}, []string{"node"})

	CandidateAvailable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "candidate_available",
		Help: "1 when the candidate node can become the active node.",
	}, []string{"node"})

	ActiveNodeComponentReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "active_node_component_ready",
		Help: "1 when the component (agent, cilium-agent) on the active node is ready. Never triggers a switch.",
	}, []string{"component"})
)

// Agent metrics.
var (
	AgentEgressIP = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "agent_egress_ip",
		Help: "1 for each managed egress IP configured on the local node.",
	}, []string{"interface", "ip"})

	AgentConflictsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "agent_dad_conflicts_total",
		Help: "Duplicate address detections that found another device answering for the egress IP.",
	}, []string{"ip"})
)

func RegisterController() {
	ctrlmetrics.Registry.MustRegister(SwitchesTotal, ActiveNode, Condition, CandidateReachable,
		CandidateAvailable, ActiveNodeComponentReady)
}

func RegisterAgent() {
	ctrlmetrics.Registry.MustRegister(AgentEgressIP, AgentConflictsTotal)
}

func Bool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
