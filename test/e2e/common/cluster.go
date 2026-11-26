//go:build e2e

package common

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/k8s"
)

const (
	// EgressNodeLabel marks the egress nodes; it is the default egressNodeSelector of the chart.
	EgressNodeLabel = v1alpha1.DefaultEgressNodeLabelKey
	SteadyTimeout   = 2 * time.Minute
)

// NewClient returns a client for the cluster in KUBECONFIG.
func NewClient() client.Client {
	GinkgoHelper()
	cfg, err := config.GetConfig()
	Expect(err).NotTo(HaveOccurred())
	c, err := client.New(cfg, client.Options{Scheme: k8s.NewScheme()})
	Expect(err).NotTo(HaveOccurred())
	return c
}

// Command runs a command and returns its trimmed combined output.
func Command(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// MustCommand runs a command and fails the spec on error.
func MustCommand(name string, args ...string) string {
	GinkgoHelper()
	out, err := Command(name, args...)
	Expect(err).NotTo(HaveOccurred(), "%s %v: %s", name, args, out)
	return out
}

func Kubectl(args ...string) string { GinkgoHelper(); return MustCommand("kubectl", args...) }

// DockerExec runs a command on a kind node.
func DockerExec(node string, args ...string) string {
	GinkgoHelper()
	return MustCommand("docker", append([]string{"exec", node}, args...)...)
}

// Running reports whether the kind node container is running.
func Running(node string) bool {
	out, err := Command("docker", "inspect", "-f", "{{.State.Running}}", node)
	return err == nil && out == "true"
}

// ManagedIPs returns the egress IPs with the managed label on eth0 of node.
func ManagedIPs(node string) []string {
	GinkgoHelper()
	out := DockerExec(node, "ip", "-4", "-o", "addr", "show", "dev", "eth0", "label", "eth0:egw")
	var ips []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 4 {
			ips = append(ips, strings.TrimSuffix(f[3], "/32"))
		}
	}
	return ips
}

func HasIP(node, ip string) bool {
	GinkgoHelper()
	return strings.Contains(DockerExec(node, "ip", "-4", "-o", "addr", "show", "dev", "eth0"), " "+ip+"/")
}

func MACOf(node string) string {
	GinkgoHelper()
	return DockerExec(node, "cat", "/sys/class/net/eth0/address")
}

// ClientSourceIP returns the source IP the target sees for a request from the client pod.
func ClientSourceIP() (string, error) {
	out, err := Command("kubectl", "exec", "client", "--", "curl", "-sS", "-m", "3", "http://"+E.TargetAddr+"/clientip")
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, out)
	}
	host, _, _ := strings.Cut(out, ":")
	return host, nil
}

func WaitSourceIP(want string) {
	GinkgoHelper()
	Eventually(ClientSourceIP).WithTimeout(time.Minute).WithPolling(500 * time.Millisecond).Should(Equal(want))
}

func PodReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// AgentPod returns the agent pod on node, or an empty pod.
func AgentPod(c client.Client, node string) *corev1.Pod {
	GinkgoHelper()
	pods := &corev1.PodList{}
	Expect(c.List(context.Background(), pods, client.InNamespace(E.Namespace),
		client.MatchingLabels{"app.kubernetes.io/component": "agent"})).To(Succeed())
	for i := range pods.Items {
		if p := &pods.Items[i]; p.Spec.NodeName == node && p.DeletionTimestamp == nil {
			return p
		}
	}
	return &corev1.Pod{}
}

// WaitUsable waits until node can become the active node again: Node Ready,
// and the cilium-agent and agent pods on it Ready.
func WaitUsable(c client.Client, node string) {
	GinkgoHelper()
	Eventually(func() error {
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
		if !PodReady(AgentPod(c, node)) {
			return fmt.Errorf("agent pod on %s not ready", node)
		}
		pods := &corev1.PodList{}
		if err := c.List(ctx, pods, client.InNamespace("kube-system"), client.MatchingLabels{"k8s-app": "cilium"}); err != nil {
			return err
		}
		for i := range pods.Items {
			if p := &pods.Items[i]; p.Spec.NodeName == node && PodReady(p) {
				return nil
			}
		}
		return fmt.Errorf("cilium-agent on %s not ready", node)
	}).WithTimeout(5 * time.Minute).WithPolling(time.Second).Should(Succeed())
}
