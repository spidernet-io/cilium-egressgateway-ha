//go:build e2e

package switchover_test

import (
	"context"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	. "github.com/spidernet-io/cilium-egressgateway-ha/test/e2e/common"
)

// next returns the egress node after active in name order, wrapping around.
func next(active string) string {
	nodes := E.EgressNodes()
	slices.Sort(nodes)
	i := slices.Index(nodes, active)
	return nodes[(i+1)%len(nodes)]
}

var _ = Describe("Switchover", Ordered, func() {
	var active string

	BeforeAll(func() {
		CreatePolicy(c)
		DeferCleanup(func() {
			_, _ = Command("docker", "exec", E.ClientNode, "ip", "addr", "del", E.EgressIP()+"/32", "dev", "eth0")
			for _, n := range E.EgressNodes() {
				_, _ = Command("kubectl", "label", "node", n, CandidateLabel+"=true", "--overwrite")
			}
		})
	})

	It("elects an active node and egress traffic uses the egress IP", func() {
		active = WaitSteady(c, "")
		WaitSourceIP(E.EgressIP())
	})

	It("keeps the egress IP and traffic while the agent restarts", func() {
		pod := AgentPod(c, active)
		Expect(c.Delete(context.Background(), pod)).To(Succeed())
		Eventually(func() bool {
			Expect(HasIP(active, E.EgressIP())).To(BeTrue(), "egress IP disappeared during agent restart")
			p := AgentPod(c, active)
			return p.UID != pod.UID && PodReady(p)
		}).WithTimeout(SteadyTimeout).WithPolling(200 * time.Millisecond).Should(BeTrue())
		Expect(ClientSourceIP()).To(Equal(E.EgressIP()))
		WaitSteady(c, active)
	})

	It("switches manually", func() {
		target := E.Worker1
		if active == E.Worker1 {
			target = E.Worker2
		}
		Expect(PatchActiveNode(c, target)).To(Succeed())
		active = WaitSteady(c, target)
		ExpectElected(c, v1alpha1.ReasonManual, target+" became active node, manual switch from")
		WaitSourceIP(E.EgressIP())
	})

	It("rejects a manual switch to a node that is not an egress node", func() {
		Expect(PatchActiveNode(c, E.ClientNode)).To(MatchError(ContainSubstring(
			"node " + E.ClientNode + " does not match spec.egressNodeSelector")))
		WaitSteady(c, active)
	})

	It("blocks the switch on a DAD conflict until the egress IP is released", func() {
		if active != E.Worker1 {
			SwitchTo(c, E.Worker1)
			active = E.Worker1
		}
		// Simulate an old active node that still holds the egress IP.
		DockerExec(E.ClientNode, "ip", "addr", "add", E.EgressIP()+"/32", "dev", "eth0")
		Expect(PatchActiveNode(c, E.Worker2)).To(Succeed())
		WaitSwitching(c, metav1.ConditionTrue, v1alpha1.ReasonEgressIPConflict,
			E.EgressIP()+" is answered by "+MACOf(E.ClientNode))
		nodes, cns, err := ActiveLabels(c)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes).To(Equal([]string{E.Worker1}))
		Expect(cns).To(Equal([]string{E.Worker1}))
		Expect(HasIP(E.Worker2, E.EgressIP())).To(BeFalse())

		DockerExec(E.ClientNode, "ip", "addr", "del", E.EgressIP()+"/32", "dev", "eth0")
		active = WaitSteady(c, E.Worker2)
		WaitSourceIP(E.EgressIP())
	})

	It("switches away when the active node is no longer an egress node", func() {
		removed, want := active, next(active)
		Kubectl("label", "node", removed, CandidateLabel+"-")
		active = WaitSteady(c, want)
		ExpectElected(c, v1alpha1.ReasonCandidateRemoved, removed+" no longer matches egressNodeSelector")
		WaitSourceIP(E.EgressIP())

		Kubectl("label", "node", removed, CandidateLabel+"=true")
		WaitUsable(c, removed)
		WaitSteady(c, active)
	})
})
