package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// SingletonName is the only allowed name of the EgressGatewayHA object.
	SingletonName = "default"

	// DefaultActiveLabelKey=DefaultActiveLabelValue is put on the Node and
	// CiliumNode of the active node. Configurable at install time.
	DefaultActiveLabelKey   = "egressgateway-ha.spidernet.io/active-node"
	DefaultActiveLabelValue = "true"

	// DefaultEgressNodeLabelKey="true" is the default egressNodeSelector of the Helm chart.
	DefaultEgressNodeLabelKey = "egressgateway-ha.spidernet.io/egress-node"
)

// Condition types.
const (
	ConditionActiveNodeElected   = "ActiveNodeElected"
	ConditionActiveNodeReachable = "ActiveNodeReachable"
	ConditionActiveNodeReady     = "ActiveNodeReady"
	ConditionSwitching           = "Switching"
	ConditionGatewayReady        = "GatewayReady"
	ConditionReady               = "Ready"
)

// Condition reasons.
const (
	ReasonInitial           = "Initial"
	ReasonFailover          = "Failover"
	ReasonManual            = "Manual"
	ReasonEgressNodeRemoved = "EgressNodeRemoved"

	ReasonProbeSucceeded = "ProbeSucceeded"
	ReasonProbeFailed    = "ProbeFailed"

	ReasonWaitingForGateway     = "WaitingForGateway"
	ReasonEgressIPConflict      = "EgressIPConflict"
	ReasonNoAvailableEgressNode = "NoAvailableEgressNode"
	ReasonTargetUnavailable     = "TargetUnavailable"
	ReasonCompleted             = "Completed"

	ReasonEgressIPsConfigured = "EgressIPsConfigured"
	ReasonInterfaceNotFound   = "InterfaceNotFound"
	ReasonConfigureFailed     = "ConfigureFailed"
	// ReasonNoManagedPolicy means no managed policy uses an egress IP, so none is configured.
	ReasonNoManagedPolicy = "NoManagedPolicy"

	ReasonAsExpected = "AsExpected"
)

// EgressGatewayHASpec defines the desired state of EgressGatewayHA.
type EgressGatewayHASpec struct {
	// EgressNodeSelector selects the nodes that may become the active gateway.
	// +required
	EgressNodeSelector metav1.LabelSelector `json:"egressNodeSelector"`

	// Interface is the network interface the agent configures egress IPs on.
	// When empty, the agent on the active node selects the interface whose
	// IPv4 subnet contains the egress IPs, or else the default route interface.
	// +optional
	// +kubebuilder:validation:MaxLength=15
	Interface string `json:"interface,omitempty"`

	// EgressIPs lists the allowed egress IPs. Each item is an IPv4 address or
	// an inclusive range in the form "start-end".
	// +required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	EgressIPs []string `json:"egressIPs"`

	// DefaultEgressIP is used for managed policies that do not set an egressIP.
	// Defaults to the first IP of EgressIPs.
	// +optional
	DefaultEgressIP string `json:"defaultEgressIP,omitempty"`

	// ActiveNode is the current active gateway node. It is set by the controller,
	// and changing it triggers a manual switchover.
	// +optional
	ActiveNode string `json:"activeNode,omitempty"`
}

// ActiveNodeStatus reports the actual egress IP state on the active node.
// It is written by the agent on that node.
type ActiveNodeStatus struct {
	// Name of the node that wrote this status.
	Name string `json:"name"`

	// EgressIPs that are configured on the interface.
	// +optional
	// +listType=atomic
	EgressIPs []string `json:"egressIPs,omitempty"`

	// Ready is true when all egress IPs are configured.
	Ready bool `json:"ready"`
}

// EgressNodeStatus is the TCP probe result of an egress node.
type EgressNodeStatus struct {
	Name      string `json:"name"`
	Reachable bool   `json:"reachable"`
	// LastTransitionTime is when Reachable last changed.
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
	// Message is the last probe error, empty when the node is reachable.
	// +optional
	Message string `json:"message,omitempty"`
}

// EgressGatewayHAStatus defines the observed state of EgressGatewayHA.
type EgressGatewayHAStatus struct {
	// +optional
	ActiveNode *ActiveNodeStatus `json:"activeNode,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=name
	EgressNodes []EgressNodeStatus `json:"egressNodes,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EgressGatewayHA is the cluster-wide singleton that configures egress gateway high availability.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=egwha
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="the only allowed name is 'default'"
// +kubebuilder:printcolumn:name="Active",type=string,JSONPath=`.spec.activeNode`
// +kubebuilder:printcolumn:name="Reachable",type=string,JSONPath=`.status.conditions[?(@.type=="ActiveNodeReachable")].status`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type EgressGatewayHA struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EgressGatewayHASpec   `json:"spec"`
	Status EgressGatewayHAStatus `json:"status,omitempty"`
}

// EgressGatewayHAList contains a list of EgressGatewayHA.
// +kubebuilder:object:root=true
type EgressGatewayHAList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressGatewayHA `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EgressGatewayHA{}, &EgressGatewayHAList{})
}
