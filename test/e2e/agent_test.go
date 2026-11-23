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
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/agent"
)

// The controller is not implemented yet: the test plays its role by setting
// spec.activeNode and moving the active label.
func TestAgent(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	if _, err := getEGWHA(ctx, c); !apierrors.IsNotFound(err) {
		t.Fatalf("EgressGatewayHA default must not exist before the test: %v", err)
	}

	policy := &ciliumv2.CiliumEgressGatewayPolicy{
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
	egw := sampleEGWHA(v1alpha1.SingletonName)
	egw.Spec.ActiveNode = worker1

	t.Cleanup(func() {
		_ = tryExec(worker3, "ip", "addr", "del", egressIP+"/32", "dev", "eth0")
		setActiveLabel(t, c, "")
		setActiveNode(t, c, "")
		eventually(t, time.Minute, "all agents release egress IPs", func() error {
			for _, n := range allNodes {
				if ips := managedIPs(t, n); len(ips) > 0 {
					return fmt.Errorf("%s still has %v", n, ips)
				}
			}
			return nil
		})
		_ = c.Delete(ctx, egw)
		_ = c.Delete(ctx, policy)
	})
	if err := c.Create(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, egw); err != nil {
		t.Fatal(err)
	}

	t.Run("active node configures egress IP and reports status", func(t *testing.T) {
		waitGatewayReady(t, c, worker1)
		if got := managedIPs(t, worker1); !slices.Equal(got, []string{egressIP}) {
			t.Fatalf("%s managed IPs = %v", worker1, got)
		}
		for _, n := range []string{worker2, "egw-ha-control-plane"} {
			if got := managedIPs(t, n); len(got) != 0 {
				t.Fatalf("standby %s has %v", n, got)
			}
		}
		cn := &ciliumv2.CiliumNode{}
		if err := c.Get(ctx, client.ObjectKey{Name: worker1}, cn); err != nil {
			t.Fatal(err)
		}
		if cn.Annotations[agent.SyncedAnnotation] == "" {
			t.Errorf("CiliumNode %s missing %s", worker1, agent.SyncedAnnotation)
		}
	})

	t.Run("egress traffic uses the egress IP", func(t *testing.T) {
		setActiveLabel(t, c, worker1)
		waitSourceIP(t, egressIP)
	})

	t.Run("agent restart keeps the egress IP and traffic", func(t *testing.T) {
		pod := agentPod(t, c, worker1)
		if err := c.Delete(ctx, pod); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Minute)
		for {
			if !hasIP(t, worker1, egressIP) {
				t.Fatal("egress IP disappeared during agent restart")
			}
			if p := agentPod(t, c, worker1); p.UID != pod.UID && podReady(p) {
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
	})

	t.Run("switch to another node", func(t *testing.T) {
		setActiveNode(t, c, worker2)
		waitGatewayReady(t, c, worker2)
		if hasIP(t, worker1, egressIP) {
			t.Fatalf("old active node %s still has the egress IP", worker1)
		}
		setActiveLabel(t, c, worker2)
		waitSourceIP(t, egressIP)
	})

	t.Run("DAD conflict blocks the new node", func(t *testing.T) {
		// Simulate an old active node that still holds the IP.
		dockerExec(t, worker3, "ip", "addr", "add", egressIP+"/32", "dev", "eth0")
		setActiveNode(t, c, worker1)
		wantMsg := egressIP + " is answered by " + macOf(t, worker3)
		eventually(t, time.Minute, "GatewayReady=False EgressIPConflict", func() error {
			egw, err := getEGWHA(ctx, c)
			if err != nil {
				return err
			}
			cond := meta.FindStatusCondition(egw.Status.Conditions, v1alpha1.ConditionGatewayReady)
			if a := egw.Status.ActiveNode; a == nil || a.Name != worker1 || a.Ready || cond == nil ||
				cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonEgressIPConflict ||
				!strings.Contains(cond.Message, wantMsg) {
				return fmt.Errorf("status %+v, condition %+v", egw.Status.ActiveNode, cond)
			}
			return nil
		})
		if hasIP(t, worker1, egressIP) || hasIP(t, worker2, egressIP) {
			t.Fatal("egress IP added despite the conflict, or not released by the old node")
		}

		dockerExec(t, worker3, "ip", "addr", "del", egressIP+"/32", "dev", "eth0")
		waitGatewayReady(t, c, worker1)
		setActiveLabel(t, c, worker1)
		waitSourceIP(t, egressIP)
	})
}

func tryExec(node string, args ...string) error {
	_, err := command("docker", append([]string{"exec", node}, args...)...)
	return err
}

func waitGatewayReady(t *testing.T, c client.Client, node string) {
	t.Helper()
	eventually(t, time.Minute, node+" GatewayReady", func() error {
		egw, err := getEGWHA(context.Background(), c)
		if err != nil {
			return err
		}
		a := egw.Status.ActiveNode
		cond := meta.FindStatusCondition(egw.Status.Conditions, v1alpha1.ConditionGatewayReady)
		if a == nil || a.Name != node || !a.Ready || !slices.Equal(a.EgressIPs, []string{egressIP}) ||
			cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonEgressIPsConfigured ||
			cond.ObservedGeneration != egw.Generation {
			return fmt.Errorf("status %+v, condition %+v", a, cond)
		}
		return nil
	})
}

func waitSourceIP(t *testing.T, want string) {
	t.Helper()
	eventually(t, time.Minute, "client source IP "+want, func() error {
		ip, err := clientSourceIP()
		if err != nil {
			return err
		}
		if ip != want {
			return fmt.Errorf("source IP %s", ip)
		}
		return nil
	})
}

func agentPod(t *testing.T, c client.Client, node string) *corev1.Pod {
	t.Helper()
	pods := &corev1.PodList{}
	if err := c.List(context.Background(), pods, client.InNamespace(namespace),
		client.MatchingLabels{"app.kubernetes.io/component": "agent"}); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		if p := &pods.Items[i]; p.Spec.NodeName == node && p.DeletionTimestamp == nil {
			return p
		}
	}
	return &corev1.Pod{}
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
