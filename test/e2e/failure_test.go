//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
)

const monitorLog = "/tmp/egw-monitor.log"

// trafficMonitor runs a request loop inside the client pod, so it keeps
// measuring even when the API server is down.
type trafficMonitor struct{ t *testing.T }

func startMonitor(t *testing.T) *trafficMonitor {
	t.Helper()
	loop := fmt.Sprintf(`echo $$ > /tmp/egw-monitor.pid; while true; do `+
		`t=$(cut -d" " -f1 /proc/uptime); r=$(curl -s -m 1 http://%s/clientip 2>/dev/null); `+
		`echo "$t ${r%%%%:*}"; sleep 0.2; done > %s`, targetAddr, monitorLog)
	if out, err := command("kubectl", "exec", "client", "--", "sh", "-c",
		fmt.Sprintf("rm -f %s; nohup sh -c '%s' >/dev/null 2>&1 &", monitorLog, loop)); err != nil {
		t.Fatalf("start monitor: %v: %s", err, out)
	}
	time.Sleep(2 * time.Second)
	return &trafficMonitor{t: t}
}

type trafficStats struct {
	requests, failed int
	// longestGap is the longest time between two successful requests with the egress IP.
	longestGap  time.Duration
	wrongSource map[string]int
}

func (s trafficStats) String() string {
	return fmt.Sprintf("%d requests, %d failed, longest gap between successes %v, wrong source %v",
		s.requests, s.failed, s.longestGap.Round(10*time.Millisecond), s.wrongSource)
}

// stop stops the loop and returns the statistics. The kubectl calls are
// retried because the API server may just have come back.
func (m *trafficMonitor) stop() trafficStats {
	m.t.Helper()
	var out string
	eventually(m.t, 2*time.Minute, "stop traffic monitor", func() error {
		var err error
		out, err = command("kubectl", "exec", "client", "--", "sh", "-c",
			fmt.Sprintf("kill $(cat /tmp/egw-monitor.pid) 2>/dev/null; cat %s", monitorLog))
		return err
	})
	s := trafficStats{wrongSource: map[string]int{}}
	last := -1.0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		ts, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		s.requests++
		src := ""
		if len(f) > 1 {
			src = f[1]
		}
		if src != egressIP {
			s.failed++
			if src != "" {
				s.wrongSource[src]++
			}
			continue
		}
		if last >= 0 {
			if gap := time.Duration((ts - last) * float64(time.Second)); gap > s.longestGap {
				s.longestGap = gap
			}
		}
		last = ts
	}
	if s.requests == 0 {
		m.t.Fatalf("traffic monitor produced no output: %q", out)
	}
	return s
}

// noInterruption fails when egress traffic was interrupted. A normal request
// cycle takes about 0.3s.
func noInterruption(t *testing.T, s trafficStats) {
	t.Helper()
	t.Logf("traffic: %s", s)
	if s.failed > 0 || s.longestGap > 2*time.Second {
		t.Errorf("egress traffic interrupted: %s", s)
	}
}

func switchEvents(t *testing.T, c client.Client) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	eventually(t, 2*time.Minute, "list events", func() error {
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
	})
	return out
}

// expectNoSwitch checks that active is still the active node and no switch was started since before.
func expectNoSwitch(t *testing.T, c client.Client, active string, before map[string]bool) {
	t.Helper()
	waitSteady(t, c, active)
	for e := range switchEvents(t, c) {
		if !before[e] {
			t.Errorf("unexpected switch: %s", e)
		}
	}
}

func apiServerUp() bool {
	_, err := command("docker", "exec", controlPlane, "curl", "-sfk", "-m", "2", "https://127.0.0.1:6443/readyz")
	return err == nil
}

func TestFailureScenarios(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	t.Cleanup(func() {
		_, _ = command("docker", "exec", controlPlane, "sh", "-c",
			"[ -f /root/kube-apiserver.yaml ] && mv /root/kube-apiserver.yaml /etc/kubernetes/manifests/")
		for _, n := range allNodes {
			_, _ = command("docker", "exec", n, "sh", "-c", "while iptables -D OUTPUT -d 172.18.0.2 -p tcp --dport 6443 -j DROP 2>/dev/null; do :; done; "+
				"while iptables -D INPUT -p tcp --dport 10250 -j DROP 2>/dev/null; do :; done")
		}
		_, _ = command("kubectl", "exec", "client", "--", "sh", "-c", "kill $(cat /tmp/egw-monitor.pid) 2>/dev/null")
		_ = c.Delete(context.Background(), e2ePolicy())
	})
	if err := c.Create(ctx, e2ePolicy()); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	// A previous test may have just restarted worker1, whose pods can still
	// report a stale Ready: retry until the manual switch is accepted.
	eventually(t, 3*time.Minute, "switch to "+worker1, func() error {
		if err := patchActiveNode(c, worker1); err != nil {
			return err
		}
		time.Sleep(5 * time.Second)
		egw, err := getEGWHA(ctx, c)
		if err != nil {
			return err
		}
		if egw.Spec.ActiveNode != worker1 {
			return fmt.Errorf("rolled back to %s", egw.Spec.ActiveNode)
		}
		return nil
	})
	active := waitSteady(t, c, worker1)
	waitSourceIP(t, egressIP)

	t.Run("kubelet restart on the active node", func(t *testing.T) {
		before := switchEvents(t, c)
		m := startMonitor(t)
		dockerExec(t, active, "systemctl", "restart", "kubelet")
		time.Sleep(30 * time.Second)
		noInterruption(t, m.stop())
		expectNoSwitch(t, c, active, before)
	})

	t.Run("agent rolling upgrade", func(t *testing.T) {
		before := switchEvents(t, c)
		m := startMonitor(t)
		kubectl(t, "-n", namespace, "rollout", "restart", "ds/egwha-agent")
		kubectl(t, "-n", namespace, "rollout", "status", "ds/egwha-agent", "--timeout=180s")
		time.Sleep(5 * time.Second)
		noInterruption(t, m.stop())
		expectNoSwitch(t, c, active, before)
	})

	t.Run("cilium-agent restart on the active node", func(t *testing.T) {
		before := switchEvents(t, c)
		m := startMonitor(t)
		pods := &corev1.PodList{}
		if err := c.List(ctx, pods, client.InNamespace("kube-system"), client.MatchingLabels{"k8s-app": "cilium"}); err != nil {
			t.Fatal(err)
		}
		for i := range pods.Items {
			if p := &pods.Items[i]; p.Spec.NodeName == active {
				if err := c.Delete(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
		}
		time.Sleep(5 * time.Second)
		waitUsable(t, c, active)
		time.Sleep(5 * time.Second)
		noInterruption(t, m.stop())
		expectNoSwitch(t, c, active, before)
	})

	t.Run("controller leader restart", func(t *testing.T) {
		before := switchEvents(t, c)
		m := startMonitor(t)
		leader := func() string {
			out, _ := command("kubectl", "-n", namespace, "get", "lease", "egressgateway-ha-controller",
				"-o", "jsonpath={.spec.holderIdentity}")
			return strings.Split(out, "_")[0]
		}
		old := leader()
		kubectl(t, "-n", namespace, "delete", "pod", old)
		eventually(t, 2*time.Minute, "new controller leader", func() error {
			if l := leader(); l == "" || l == old {
				return fmt.Errorf("leader %q", l)
			}
			return nil
		})
		time.Sleep(10 * time.Second)
		noInterruption(t, m.stop())
		expectNoSwitch(t, c, active, before)
	})

	t.Run("API server unavailable", func(t *testing.T) {
		before := switchEvents(t, c)
		m := startMonitor(t)
		dockerExec(t, controlPlane, "mv", "/etc/kubernetes/manifests/kube-apiserver.yaml", "/root/")
		eventually(t, 2*time.Minute, "API server down", func() error {
			if apiServerUp() {
				return fmt.Errorf("still up")
			}
			return nil
		})
		down := time.Now()
		time.Sleep(60 * time.Second)
		dockerExec(t, controlPlane, "mv", "/root/kube-apiserver.yaml", "/etc/kubernetes/manifests/")
		eventually(t, 3*time.Minute, "API server up", func() error {
			if !apiServerUp() {
				return fmt.Errorf("still down")
			}
			return nil
		})
		t.Logf("API server was down for %v", time.Since(down).Round(time.Second))
		for _, n := range []string{controlPlane, worker1, worker2} {
			waitUsable(t, c, n)
		}
		noInterruption(t, m.stop())
		expectNoSwitch(t, c, active, before)
	})

	t.Run("partition of the active node from controller and API server", func(t *testing.T) {
		// L2 still works: ARP is not filtered, so the new node's DAD sees the old node.
		m := startMonitor(t)
		dockerExec(t, active, "iptables", "-I", "OUTPUT", "-d", "172.18.0.2", "-p", "tcp", "--dport", "6443", "-j", "DROP")
		dockerExec(t, active, "iptables", "-I", "INPUT", "-p", "tcp", "--dport", "10250", "-j", "DROP")
		start := time.Now()
		waitSwitching(t, c, metav1.ConditionTrue, v1alpha1.ReasonEgressIPConflict, egressIP+" is answered by "+macOf(t, active))
		// Right after the API server scenario this includes the controller
		// re-acquiring the Lease and probing from scratch.
		t.Logf("switch blocked by DAD %v after the partition", time.Since(start).Round(time.Second))
		egw, err := getEGWHA(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		target := egw.Spec.ActiveNode
		nodes, cns, err := activeLabels(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(nodes, ",") != active || strings.Join(cns, ",") != active || hasIP(t, target, egressIP) {
			t.Fatalf("label moved or IP added during the partition: Node %v, CiliumNode %v", nodes, cns)
		}
		time.Sleep(20 * time.Second)
		noInterruption(t, m.stop())

		m = startMonitor(t)
		healed := time.Now()
		dockerExec(t, active, "iptables", "-D", "OUTPUT", "-d", "172.18.0.2", "-p", "tcp", "--dport", "6443", "-j", "DROP")
		dockerExec(t, active, "iptables", "-D", "INPUT", "-p", "tcp", "--dport", "10250", "-j", "DROP")
		newActive := waitSteady(t, c, target)
		t.Logf("switch to %s completed %v after the partition healed", newActive, time.Since(healed).Round(time.Second))
		time.Sleep(5 * time.Second)
		s := m.stop()
		t.Logf("traffic across the switch: %s", s)
		if s.longestGap > 10*time.Second || len(s.wrongSource) > 0 {
			t.Errorf("egress traffic interrupted too long: %s", s)
		}
		cond := meta.FindStatusCondition(func() []metav1.Condition {
			egw, _ := getEGWHA(ctx, c)
			return egw.Status.Conditions
		}(), v1alpha1.ConditionActiveNodeElected)
		t.Logf("ActiveNodeElected: %s", cond.Message)
		waitUsable(t, c, active)
	})
}
