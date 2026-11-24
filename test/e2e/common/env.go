//go:build e2e

// Package common holds the environment and helpers shared by the e2e suites.
// The environment is written by test/scripts/setup-testenv.sh.
package common

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// Env describes the kind test environment.
type Env struct {
	KindCluster    string
	ControlPlane   string
	ControlPlaneIP string
	// Worker1 and Worker2 are egress nodes together with ControlPlane.
	Worker1, Worker2 string
	// ClientNode runs the client pod and is never an egress node.
	ClientNode string
	// TargetAddr is the external HTTP target, host:port.
	TargetAddr string
	// EgressIPs is spec.egressIPs, an inclusive range.
	EgressIPStart, EgressIPEnd netip.Addr
	Namespace                  string
}

// E is loaded once from the environment.
var E = mustLoad()

func mustLoad() Env {
	get := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			panic(fmt.Sprintf("%s is not set; run the tests with make e2e-test", k))
		}
		return v
	}
	start, end, ok := strings.Cut(get("E2E_EGRESS_IP_RANGE"), "-")
	if !ok {
		panic("E2E_EGRESS_IP_RANGE must be start-end")
	}
	return Env{
		KindCluster:    get("E2E_KIND_CLUSTER"),
		ControlPlane:   get("E2E_CONTROL_PLANE"),
		ControlPlaneIP: get("E2E_CONTROL_PLANE_IP"),
		Worker1:        get("E2E_WORKER1"),
		Worker2:        get("E2E_WORKER2"),
		ClientNode:     get("E2E_CLIENT_NODE"),
		TargetAddr:     get("E2E_TARGET_ADDR"),
		EgressIPStart:  netip.MustParseAddr(start),
		EgressIPEnd:    netip.MustParseAddr(end),
		Namespace:      get("E2E_NAMESPACE"),
	}
}

// EgressIP is the default egress IP: the first IP of spec.egressIPs.
func (e Env) EgressIP() string { return e.EgressIPStart.String() }

// EgressIPAt returns the n-th IP of spec.egressIPs; a negative n is outside the range.
func (e Env) EgressIPAt(n int) string {
	ip := e.EgressIPStart
	for ; n > 0; n-- {
		ip = ip.Next()
	}
	for ; n < 0; n++ {
		ip = ip.Prev()
	}
	return ip.String()
}

// EgressIPRange is spec.egressIPs as written in the chart values.
func (e Env) EgressIPRange() string { return e.EgressIPStart.String() + "-" + e.EgressIPEnd.String() }

// TargetIP is the IP of the external target.
func (e Env) TargetIP() string { return strings.Split(e.TargetAddr, ":")[0] }

// EgressNodes are the candidate nodes, sorted by name.
func (e Env) EgressNodes() []string { return []string{e.ControlPlane, e.Worker1, e.Worker2} }

// AllNodes are all kind nodes.
func (e Env) AllNodes() []string { return append(e.EgressNodes(), e.ClientNode) }
