//go:build e2e

package common

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	slimv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/controller"
)

// Policy is the managed policy that sends the client pod traffic to the target
// through the active node.
func Policy() *ciliumv2.CiliumEgressGatewayPolicy {
	return &ciliumv2.CiliumEgressGatewayPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "egw-e2e"},
		Spec: ciliumv2.CiliumEgressGatewayPolicySpec{
			Selectors: []ciliumv2.EgressRule{{PodSelector: &slimv1.LabelSelector{
				MatchLabels: map[string]slimv1.MatchLabelsValue{"app": "client"},
			}}},
			DestinationCIDRs: []ciliumv2.CIDR{ciliumv2.CIDR(E.TargetIP() + "/32")},
			EgressGateway: &ciliumv2.EgressGateway{
				NodeSelector: &slimv1.LabelSelector{MatchLabels: map[string]slimv1.MatchLabelsValue{
					v1alpha1.DefaultActiveLabelKey: v1alpha1.DefaultActiveLabelValue,
				}},
				EgressIP: E.EgressIP(),
			},
		},
	}
}

// CreatePolicy creates Policy and deletes it when the suite ends.
func CreatePolicy(c client.Client) {
	GinkgoHelper()
	err := c.Create(context.Background(), Policy())
	if !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	DeferCleanup(func() { _ = c.Delete(context.Background(), Policy()) })
}

func GetEGWHA(c client.Client) (*v1alpha1.EgressGatewayHA, error) {
	egw := &v1alpha1.EgressGatewayHA{}
	return egw, c.Get(context.Background(), client.ObjectKey{Name: v1alpha1.SingletonName}, egw)
}

func PatchEGWHA(c client.Client, patch string) error {
	egw := &v1alpha1.EgressGatewayHA{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName}}
	return c.Patch(context.Background(), egw, client.RawPatch(types.MergePatchType, []byte(patch)))
}

func PatchActiveNode(c client.Client, node string) error {
	return PatchEGWHA(c, fmt.Sprintf(`{"spec":{"activeNode":%q}}`, node))
}

// SwitchTo switches manually to node and waits for the steady state. A node
// that was just restarted can still be rolled back as unavailable, so the
// switch is retried until it is accepted.
func SwitchTo(c client.Client, node string) {
	GinkgoHelper()
	Eventually(func() error {
		if err := PatchActiveNode(c, node); err != nil {
			return err
		}
		time.Sleep(5 * time.Second)
		egw, err := GetEGWHA(c)
		if err != nil {
			return err
		}
		if egw.Spec.ActiveNode != node {
			return fmt.Errorf("rolled back to %s", egw.Spec.ActiveNode)
		}
		return nil
	}).WithTimeout(3 * time.Minute).Should(Succeed())
	WaitSteady(c, node)
}

// ActiveLabels returns the nodes whose Node and CiliumNode carry the active label.
func ActiveLabels(c client.Client) (nodes, ciliumNodes []string, err error) {
	nl, cnl := &corev1.NodeList{}, &ciliumv2.CiliumNodeList{}
	if err := c.List(context.Background(), nl); err != nil {
		return nil, nil, err
	}
	if err := c.List(context.Background(), cnl); err != nil {
		return nil, nil, err
	}
	for _, n := range nl.Items {
		if n.Labels[v1alpha1.DefaultActiveLabelKey] == v1alpha1.DefaultActiveLabelValue {
			nodes = append(nodes, n.Name)
		}
	}
	for _, n := range cnl.Items {
		if n.Labels[v1alpha1.DefaultActiveLabelKey] == v1alpha1.DefaultActiveLabelValue {
			ciliumNodes = append(ciliumNodes, n.Name)
		}
	}
	return nodes, ciliumNodes, nil
}

// SteadyState returns nil when the switch to want (any node if empty) is
// complete: Ready=True, the label only on the active node, and only it holds
// the egress IP. It returns the active node.
func SteadyState(c client.Client, want string) (string, error) {
	egw, err := GetEGWHA(c)
	if err != nil {
		return "", err
	}
	active := egw.Spec.ActiveNode
	if active == "" || (want != "" && active != want) {
		return active, fmt.Errorf("spec.activeNode = %q", active)
	}
	if v, ok := egw.Annotations[controller.SwitchAnnotation]; ok {
		return active, fmt.Errorf("switch in progress: %s", v)
	}
	if r := meta.FindStatusCondition(egw.Status.Conditions, v1alpha1.ConditionReady); r == nil ||
		r.Status != metav1.ConditionTrue || r.ObservedGeneration != egw.Generation {
		return active, fmt.Errorf("Ready = %+v", r)
	}
	nodes, cns, err := ActiveLabels(c)
	if err != nil {
		return active, err
	}
	if !slices.Equal(nodes, []string{active}) || !slices.Equal(cns, []string{active}) {
		return active, fmt.Errorf("active label on Node %v, CiliumNode %v", nodes, cns)
	}
	for _, n := range E.AllNodes() {
		if !Running(n) {
			continue
		}
		ips := ManagedIPs(n)
		if n == active && !slices.Equal(ips, []string{E.EgressIP()}) || n != active && len(ips) > 0 {
			return active, fmt.Errorf("%s has egress IPs %v", n, ips)
		}
	}
	return active, nil
}

// WaitSteady waits for SteadyState and returns the active node.
func WaitSteady(c client.Client, want string) string {
	GinkgoHelper()
	var active string
	Eventually(func() error {
		var err error
		active, err = SteadyState(c, want)
		return err
	}).WithTimeout(SteadyTimeout).WithPolling(500 * time.Millisecond).Should(Succeed())
	return active
}

func ExpectElected(c client.Client, reason, msg string) {
	GinkgoHelper()
	egw, err := GetEGWHA(c)
	Expect(err).NotTo(HaveOccurred())
	cond := meta.FindStatusCondition(egw.Status.Conditions, v1alpha1.ConditionActiveNodeElected)
	Expect(cond).NotTo(BeNil())
	Expect(cond.Reason).To(Equal(reason))
	Expect(cond.Message).To(ContainSubstring(msg))
}

func WaitSwitching(c client.Client, status metav1.ConditionStatus, reason, msg string) {
	GinkgoHelper()
	Eventually(func() error {
		egw, err := GetEGWHA(c)
		if err != nil {
			return err
		}
		cond := meta.FindStatusCondition(egw.Status.Conditions, v1alpha1.ConditionSwitching)
		if cond == nil || cond.Status != status || cond.Reason != reason || !strings.Contains(cond.Message, msg) {
			return fmt.Errorf("Switching = %+v", cond)
		}
		return nil
	}).WithTimeout(SteadyTimeout).Should(Succeed())
}

// SwitchEvents returns the SwitchStarted events recorded so far.
func SwitchEvents(c client.Client) map[string]bool {
	GinkgoHelper()
	out := map[string]bool{}
	Eventually(func() error {
		l := &eventsv1.EventList{}
		if err := c.List(context.Background(), l); err != nil {
			return err
		}
		for _, e := range l.Items {
			if e.Regarding.Kind == "EgressGatewayHA" && e.Reason == "SwitchStarted" {
				out[string(e.UID)+"/"+e.Note] = true
			}
		}
		return nil
	}).WithTimeout(SteadyTimeout).Should(Succeed())
	return out
}

// ExpectNoSwitch checks that active is still the active node and no switch started since before.
func ExpectNoSwitch(c client.Client, active string, before map[string]bool) {
	GinkgoHelper()
	WaitSteady(c, active)
	for e := range SwitchEvents(c) {
		Expect(before).To(HaveKey(e), "unexpected switch")
	}
}

func APIServerUp() bool {
	_, err := Command("docker", "exec", E.ControlPlane, "curl", "-sfk", "-m", "2", "https://127.0.0.1:6443/readyz")
	return err == nil
}
