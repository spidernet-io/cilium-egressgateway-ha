//go:build e2e

package failover_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	. "github.com/spidernet-io/cilium-egressgateway-ha/test/e2e/common"
)

func activeNode() string {
	egw, err := GetEGWHA(c)
	Expect(err).NotTo(HaveOccurred())
	return egw.Spec.ActiveNode
}

var _ = Describe("Failover", Ordered, Label("disruptive"), func() {
	BeforeAll(func() {
		CreatePolicy(c)
		DeferCleanup(func() {
			_, _ = Command("docker", "start", E.Worker1)
			_, _ = Command("docker", "exec", E.Worker1, "systemctl", "start", "kubelet")
		})
	})

	BeforeEach(func() {
		SwitchTo(c, E.Worker1)
		WaitSourceIP(E.EgressIP())
	})

	It("fails over when the active node becomes NotReady and does not switch back", func() {
		start := time.Now()
		DockerExec(E.Worker1, "systemctl", "stop", "kubelet")
		Eventually(activeNode).WithTimeout(3 * time.Minute).ShouldNot(Equal(E.Worker1))
		active := WaitSteady(c, E.Worker2)
		AddReportEntry("kubelet stopped -> switched", time.Since(start).Round(time.Second).String())
		ExpectElected(c, v1alpha1.ReasonFailover, E.Worker1+" NotReady")
		WaitSourceIP(E.EgressIP())

		DockerExec(E.Worker1, "systemctl", "start", "kubelet")
		WaitUsable(c, E.Worker1)
		time.Sleep(15 * time.Second)
		WaitSteady(c, active)
	})

	It("fails over by TCP probe when the active node crashes and does not switch back", func() {
		start := time.Now()
		MustCommand("docker", "kill", E.Worker1)
		Eventually(activeNode).WithTimeout(time.Minute).ShouldNot(Equal(E.Worker1))
		switched := time.Since(start)
		Eventually(func() string {
			out, _ := Command("kubectl", "exec", "client", "--", "curl", "-sS", "-m", "1", "http://"+E.TargetAddr+"/clientip")
			return out
		}).WithTimeout(time.Minute).WithPolling(100 * time.Millisecond).Should(HavePrefix(E.EgressIP() + ":"))
		recovered := time.Since(start)
		active := WaitSteady(c, E.Worker2)
		AddReportEntry("crash -> switched", switched.Round(100*time.Millisecond).String())
		AddReportEntry("crash -> egress traffic recovered", recovered.Round(100*time.Millisecond).String())
		ExpectElected(c, v1alpha1.ReasonFailover, "probes to ")
		Expect(switched).To(BeNumerically("<", 30*time.Second), "want the TCP probe to detect it in about 16-21s")

		MustCommand("docker", "start", E.Worker1)
		WaitUsable(c, E.Worker1)
		time.Sleep(15 * time.Second)
		WaitSteady(c, active)
		egw, err := GetEGWHA(c)
		Expect(err).NotTo(HaveOccurred())
		for _, cand := range egw.Status.EgressNodes {
			Expect(cand.Reachable).To(BeTrue(), "status.egressNodes: %+v", egw.Status.EgressNodes)
		}
	})
})
