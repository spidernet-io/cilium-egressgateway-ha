//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	slimv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/controller"
)

const (
	controlPlane  = "egw-ha-control-plane"
	candidateKey  = "egress-gw-candidate"
	steadyTimeout = 2 * time.Minute
)

func e2ePolicy() *ciliumv2.CiliumEgressGatewayPolicy {
	return &ciliumv2.CiliumEgressGatewayPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "egw-e2e"},
		Spec: ciliumv2.CiliumEgressGatewayPolicySpec{
			Selectors: []ciliumv2.EgressRule{{PodSelector: &slimv1.LabelSelector{
				MatchLabels: map[string]slimv1.MatchLabelsValue{"app": "client"},
			}}},
			DestinationCIDRs: []ciliumv2.CIDR{ciliumv2.CIDR(strings.Split(targetAddr, ":")[0] + "/32")},
			EgressGateway: &ciliumv2.EgressGateway{
				NodeSelector: &slimv1.LabelSelector{MatchLabels: map[string]slimv1.MatchLabelsValue{
					v1alpha1.ActiveLabelKey: v1alpha1.ActiveLabelValue,
				}},
				EgressIP: egressIP,
			},
		},
	}
}

// activeLabels returns the nodes whose Node and CiliumNode carry the active label.
func activeLabels(ctx context.Context, c client.Client) (nodes, ciliumNodes []string, err error) {
	nl, cnl := &corev1.NodeList{}, &ciliumv2.CiliumNodeList{}
	if err := c.List(ctx, nl); err != nil {
		return nil, nil, err
	}
	if err := c.List(ctx, cnl); err != nil {
		return nil, nil, err
	}
	for _, n := range nl.Items {
		if n.Labels[v1alpha1.ActiveLabelKey] == v1alpha1.ActiveLabelValue {
			nodes = append(nodes, n.Name)
		}
	}
	for _, n := range cnl.Items {
		if n.Labels[v1alpha1.ActiveLabelKey] == v1alpha1.ActiveLabelValue {
			ciliumNodes = append(ciliumNodes, n.Name)
		}
	}
	return nodes, ciliumNodes, nil
}

// waitSteady waits until the switch to want (any node if empty) is complete:
// Ready=True, the label is only on the active node, and only it has the egress IP.
func waitSteady(t *testing.T, c client.Client, want string) string {
	t.Helper()
	var active string
	eventually(t, steadyTimeout, "steady state on "+orAny(want), func() error {
		ctx := context.Background()
		egw, err := getEGWHA(ctx, c)
		if err != nil {
			return err
		}
		active = egw.Spec.ActiveNode
		if active == "" || (want != "" && active != want) {
			return fmt.Errorf("spec.activeNode = %q", active)
		}
		if _, ok := egw.Annotations[controller.SwitchAnnotation]; ok {
			return fmt.Errorf("switch in progress: %s", egw.Annotations[controller.SwitchAnnotation])
		}
		if r := meta.FindStatusCondition(egw.Status.Conditions, v1alpha1.ConditionReady); r == nil ||
			r.Status != metav1.ConditionTrue || r.ObservedGeneration != egw.Generation {
			return fmt.Errorf("Ready = %+v", r)
		}
		nodes, cns, err := activeLabels(ctx, c)
		if err != nil {
			return err
		}
		if !slices.Equal(nodes, []string{active}) || !slices.Equal(cns, []string{active}) {
			return fmt.Errorf("active label on Node %v, CiliumNode %v", nodes, cns)
		}
		for _, n := range allNodes {
			if !running(n) {
				continue
			}
			ips := managedIPs(t, n)
			if wantIPs := []string(nil); n == active && !slices.Equal(ips, []string{egressIP}) ||
				n != active && !slices.Equal(ips, wantIPs) {
				return fmt.Errorf("%s has egress IPs %v", n, ips)
			}
		}
		return nil
	})
	return active
}

func orAny(s string) string {
	if s == "" {
		return "any node"
	}
	return s
}

func expectElected(t *testing.T, c client.Client, reason, msg string) {
	t.Helper()
	egw, err := getEGWHA(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(egw.Status.Conditions, v1alpha1.ConditionActiveNodeElected)
	if cond == nil || cond.Reason != reason || !strings.Contains(cond.Message, msg) {
		t.Fatalf("ActiveNodeElected = %+v, want reason %s containing %q", cond, reason, msg)
	}
}

func waitSwitching(t *testing.T, c client.Client, status metav1.ConditionStatus, reason, msg string) {
	t.Helper()
	eventually(t, steadyTimeout, "Switching "+reason, func() error {
		egw, err := getEGWHA(context.Background(), c)
		if err != nil {
			return err
		}
		cond := meta.FindStatusCondition(egw.Status.Conditions, v1alpha1.ConditionSwitching)
		if cond == nil || cond.Status != status || cond.Reason != reason || !strings.Contains(cond.Message, msg) {
			return fmt.Errorf("Switching = %+v", cond)
		}
		return nil
	})
}

func kubectl(t *testing.T, args ...string) {
	t.Helper()
	if out, err := command("kubectl", args...); err != nil {
		t.Fatalf("kubectl %v: %v: %s", args, err, out)
	}
}

// waitUsable waits until node can become the active node again: Node Ready,
// and the cilium-agent and agent pods on it Ready.
func waitUsable(t *testing.T, c client.Client, node string) {
	t.Helper()
	eventually(t, 3*time.Minute, node+" usable", func() error {
		ctx := context.Background()
		n := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: node}, n); err != nil {
			return err
		}
		ready := false
		for _, cond := range n.Status.Conditions {
			ready = ready || cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue
		}
		if !ready {
			return fmt.Errorf("%s not Ready", node)
		}
		if !podReady(agentPod(t, c, node)) {
			return fmt.Errorf("agent pod on %s not ready", node)
		}
		pods := &corev1.PodList{}
		if err := c.List(ctx, pods, client.InNamespace("kube-system"), client.MatchingLabels{"k8s-app": "cilium"}); err != nil {
			return err
		}
		for i := range pods.Items {
			if p := &pods.Items[i]; p.Spec.NodeName == node && podReady(p) {
				return nil
			}
		}
		return fmt.Errorf("cilium-agent on %s not ready", node)
	})
}

func TestController(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)

	t.Cleanup(func() {
		_, _ = command("docker", "start", worker1)
		_, _ = command("docker", "exec", worker1, "systemctl", "start", "kubelet")
		_, _ = command("docker", "exec", worker3, "ip", "addr", "del", egressIP+"/32", "dev", "eth0")
		for _, n := range []string{controlPlane, worker1, worker2} {
			_, _ = command("kubectl", "label", "node", n, candidateKey+"=true", "--overwrite")
		}
		_ = c.Delete(ctx, e2ePolicy())
	})
	if err := c.Create(ctx, e2ePolicy()); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}

	var active string
	t.Run("an active node is elected and egress traffic uses the egress IP", func(t *testing.T) {
		active = waitSteady(t, c, "")
		waitSourceIP(t, egressIP)
	})

	t.Run("agent restart keeps the egress IP and traffic", func(t *testing.T) {
		pod := agentPod(t, c, active)
		if err := c.Delete(ctx, pod); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(steadyTimeout)
		for {
			if !hasIP(t, active, egressIP) {
				t.Fatal("egress IP disappeared during agent restart")
			}
			if p := agentPod(t, c, active); p.UID != pod.UID && podReady(p) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("new agent pod not ready")
			}
			time.Sleep(200 * time.Millisecond)
		}
		if ip, err := clientSourceIP(); err != nil || ip != egressIP {
			t.Fatalf("source IP after restart = %q, %v", ip, err)
		}
		waitSteady(t, c, active)
	})

	t.Run("manual switch", func(t *testing.T) {
		target := worker1
		if active == worker1 {
			target = worker2
		}
		setActiveNode(t, c, target)
		active = waitSteady(t, c, target)
		expectElected(t, c, v1alpha1.ReasonManual, target+" became active node, manual switch from")
		waitSourceIP(t, egressIP)
	})

	t.Run("manual switch to a non-candidate is rejected by the webhook", func(t *testing.T) {
		err := patchActiveNode(c, worker3)
		if err == nil || !strings.Contains(err.Error(), "node "+worker3+" does not match spec.egressNodeSelector") {
			t.Fatalf("want webhook denial, got %v", err)
		}
		waitSteady(t, c, active)
	})

	t.Run("DAD conflict blocks the switch until the IP is released", func(t *testing.T) {
		if active != worker1 {
			setActiveNode(t, c, worker1)
			active = waitSteady(t, c, worker1)
		}
		// Simulate an old active node that still holds the IP.
		dockerExec(t, worker3, "ip", "addr", "add", egressIP+"/32", "dev", "eth0")
		setActiveNode(t, c, worker2)
		waitSwitching(t, c, metav1.ConditionTrue, v1alpha1.ReasonEgressIPConflict,
			egressIP+" is answered by "+macOf(t, worker3))
		nodes, cns, err := activeLabels(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(nodes, []string{worker1}) || !slices.Equal(cns, []string{worker1}) || hasIP(t, worker2, egressIP) {
			t.Fatalf("label moved or IP added despite the conflict: Node %v, CiliumNode %v", nodes, cns)
		}
		dockerExec(t, worker3, "ip", "addr", "del", egressIP+"/32", "dev", "eth0")
		active = waitSteady(t, c, worker2)
		waitSourceIP(t, egressIP)
	})

	t.Run("removing the active node from the candidates switches away", func(t *testing.T) {
		kubectl(t, "label", "node", active, candidateKey+"-")
		// Candidates are sorted: control-plane, worker, worker2; worker2 wraps around.
		next := controlPlane
		if active != worker2 {
			t.Fatalf("unexpected active node %s", active)
		}
		removed := active
		active = waitSteady(t, c, next)
		expectElected(t, c, v1alpha1.ReasonCandidateRemoved, removed+" no longer matches egressNodeSelector")
		waitSourceIP(t, egressIP)
		kubectl(t, "label", "node", removed, candidateKey+"=true")
		waitUsable(t, c, removed)
		waitSteady(t, c, active)
	})

	t.Run("NotReady active node fails over and is not switched back", func(t *testing.T) {
		setActiveNode(t, c, worker1)
		active = waitSteady(t, c, worker1)
		waitSourceIP(t, egressIP)

		start := time.Now()
		dockerExec(t, worker1, "systemctl", "stop", "kubelet")
		eventually(t, 3*time.Minute, "failover away from "+worker1, func() error {
			egw, err := getEGWHA(ctx, c)
			if err != nil {
				return err
			}
			if egw.Spec.ActiveNode == worker1 {
				return fmt.Errorf("still on %s", worker1)
			}
			return nil
		})
		active = waitSteady(t, c, worker2)
		t.Logf("switched to %s %v after stopping kubelet", active, time.Since(start).Round(time.Second))
		expectElected(t, c, v1alpha1.ReasonFailover, worker1+" NotReady")
		waitSourceIP(t, egressIP)

		dockerExec(t, worker1, "systemctl", "start", "kubelet")
		waitUsable(t, c, worker1)
		time.Sleep(15 * time.Second)
		waitSteady(t, c, worker2)
	})

	t.Run("crashed active node fails over by TCP probe and is not switched back", func(t *testing.T) {
		setActiveNode(t, c, worker1)
		active = waitSteady(t, c, worker1)
		waitSourceIP(t, egressIP)

		start := time.Now()
		if out, err := command("docker", "kill", worker1); err != nil {
			t.Fatalf("docker kill: %v: %s", err, out)
		}
		var switched time.Duration
		eventually(t, time.Minute, "failover away from "+worker1, func() error {
			egw, err := getEGWHA(ctx, c)
			if err != nil {
				return err
			}
			if egw.Spec.ActiveNode == worker1 {
				return fmt.Errorf("still on %s", worker1)
			}
			switched = time.Since(start)
			return nil
		})
		// Poll the egress path quickly to measure when traffic recovers.
		eventually(t, time.Minute, "egress traffic recovered", func() error {
			out, err := command("kubectl", "exec", "client", "--", "curl", "-sS", "-m", "1", "http://"+targetAddr+"/clientip")
			if err != nil || !strings.HasPrefix(out, egressIP+":") {
				return fmt.Errorf("%v: %s", err, out)
			}
			return nil
		})
		recovered := time.Since(start)
		active = waitSteady(t, c, worker2)
		t.Logf("crash -> spec.activeNode=%s: %v, crash -> egress traffic recovered: %v",
			active, switched.Round(100*time.Millisecond), recovered.Round(100*time.Millisecond))
		expectElected(t, c, v1alpha1.ReasonFailover, "probes to ")
		if switched > 30*time.Second {
			t.Errorf("failover took %v, want it detected by the TCP probe (about 16-21s)", switched)
		}

		if out, err := command("docker", "start", worker1); err != nil {
			t.Fatalf("docker start: %v: %s", err, out)
		}
		waitUsable(t, c, worker1)
		time.Sleep(15 * time.Second)
		waitSteady(t, c, worker2)
		egw, err := getEGWHA(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		for _, cand := range egw.Status.Candidates {
			if !cand.Reachable {
				t.Errorf("status.candidates = %+v, want all reachable after recovery", egw.Status.Candidates)
			}
		}
	})
}
