//go:build e2e

package reliability_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	. "github.com/spidernet-io/cilium-egressgateway-ha/test/e2e/common"
)

// Control plane problems must not switch the active node nor interrupt egress traffic.
var _ = Describe("Reliability", Ordered, Label("disruptive"), func() {
	var (
		active string
		before map[string]bool
		mon    *TrafficMonitor
	)
	partition := func(op string) {
		DockerExec(active, "iptables", op, "OUTPUT", "-d", E.ControlPlaneIP, "-p", "tcp", "--dport", "6443", "-j", "DROP")
		DockerExec(active, "iptables", op, "INPUT", "-p", "tcp", "--dport", "10250", "-j", "DROP")
	}

	BeforeAll(func() {
		CreatePolicy(c)
		DeferCleanup(func() {
			_, _ = Command("docker", "exec", E.ControlPlane, "sh", "-c",
				"[ -f /root/kube-apiserver.yaml ] && mv /root/kube-apiserver.yaml /etc/kubernetes/manifests/")
			for _, n := range E.AllNodes() {
				_, _ = Command("docker", "exec", n, "sh", "-c",
					"while iptables -D OUTPUT -d "+E.ControlPlaneIP+" -p tcp --dport 6443 -j DROP 2>/dev/null; do :; done; "+
						"while iptables -D INPUT -p tcp --dport 10250 -j DROP 2>/dev/null; do :; done")
			}
			_, _ = Command("kubectl", "exec", "client", "--", "sh", "-c", "kill $(cat /tmp/egw-monitor.pid) 2>/dev/null")
		})
		SwitchTo(c, E.Worker1)
		active = E.Worker1
		WaitSourceIP(E.EgressIP())
	})

	// Each spec below disturbs something and expects no switch and no egress interruption.
	BeforeEach(func(ctx SpecContext) {
		if strings.Contains(CurrentSpecReport().LeafNodeText, "partition") {
			return
		}
		before = SwitchEvents(c)
		mon = StartMonitor()
		DeferCleanup(func() {
			ExpectNoInterruption(mon.Stop())
			ExpectNoSwitch(c, active, before)
		})
	})

	It("survives a kubelet restart on the active node", func() {
		DockerExec(active, "systemctl", "restart", "kubelet")
		time.Sleep(30 * time.Second)
	})

	It("survives an agent rolling upgrade", func() {
		Kubectl("-n", E.Namespace, "rollout", "restart", "ds/egwha-agent")
		Kubectl("-n", E.Namespace, "rollout", "status", "ds/egwha-agent", "--timeout=5m")
		time.Sleep(5 * time.Second)
	})

	It("survives a cilium-agent restart on the active node", func() {
		pods := &corev1.PodList{}
		Expect(c.List(context.Background(), pods, client.InNamespace("kube-system"),
			client.MatchingLabels{"k8s-app": "cilium"})).To(Succeed())
		for i := range pods.Items {
			if p := &pods.Items[i]; p.Spec.NodeName == active {
				Expect(c.Delete(context.Background(), p)).To(Succeed())
			}
		}
		time.Sleep(5 * time.Second)
		WaitUsable(c, active)
		time.Sleep(5 * time.Second)
	})

	It("survives a controller leader restart", func() {
		leader := func() string {
			out, _ := Command("kubectl", "-n", E.Namespace, "get", "lease", "egressgateway-ha-controller",
				"-o", "jsonpath={.spec.holderIdentity}")
			return strings.Split(out, "_")[0]
		}
		old := leader()
		Kubectl("-n", E.Namespace, "delete", "pod", old)
		Eventually(leader).WithTimeout(SteadyTimeout).ShouldNot(Or(BeEmpty(), Equal(old)))
		time.Sleep(10 * time.Second)
	})

	It("survives the API server being unavailable", func() {
		DockerExec(E.ControlPlane, "mv", "/etc/kubernetes/manifests/kube-apiserver.yaml", "/root/")
		Eventually(APIServerUp).WithTimeout(SteadyTimeout).Should(BeFalse())
		down := time.Now()
		time.Sleep(60 * time.Second)
		DockerExec(E.ControlPlane, "mv", "/root/kube-apiserver.yaml", "/etc/kubernetes/manifests/")
		Eventually(APIServerUp).WithTimeout(3 * time.Minute).Should(BeTrue())
		AddReportEntry("API server down", time.Since(down).Round(time.Second).String())
		for _, n := range E.EgressNodes() {
			WaitUsable(c, n)
		}
	})

	It("stops at the DAD conflict during a partition from the controller and API server, and completes after it heals", func() {
		// L2 still works: ARP is not filtered, so the new node's DAD sees the old one.
		mon = StartMonitor()
		start := time.Now()
		partition("-I")
		WaitSwitching(c, metav1.ConditionTrue, v1alpha1.ReasonEgressIPConflict, E.EgressIP()+" is answered by "+MACOf(active))
		AddReportEntry("partition -> blocked by DAD", time.Since(start).Round(time.Second).String())
		egw, err := GetEGWHA(c)
		Expect(err).NotTo(HaveOccurred())
		target := egw.Spec.ActiveNode
		nodes, cns, err := ActiveLabels(c)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes).To(Equal([]string{active}))
		Expect(cns).To(Equal([]string{active}))
		Expect(HasIP(target, E.EgressIP())).To(BeFalse())
		time.Sleep(20 * time.Second)
		ExpectNoInterruption(mon.Stop())

		mon = StartMonitor()
		healed := time.Now()
		partition("-D")
		old := active
		active = WaitSteady(c, target)
		AddReportEntry("partition healed -> switched", time.Since(healed).Round(time.Second).String())
		time.Sleep(5 * time.Second)
		s := mon.Stop()
		GinkgoWriter.Printf("traffic across the switch: %s\n", s)
		Expect(s.LongestGap).To(BeNumerically("<=", 10*time.Second), fmt.Sprint(s))
		Expect(s.WrongSource).To(BeEmpty())
		WaitUsable(c, old)
	})
})
