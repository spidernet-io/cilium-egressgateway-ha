//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/k8s"
)

// The kind cluster "egw-ha": node names equal docker container names.
const (
	worker1    = "egw-ha-worker"
	worker2    = "egw-ha-worker2"
	worker3    = "egw-ha-worker3" // not a candidate; runs the client pod
	targetAddr = "172.18.0.6:8080"
	egressIP   = "172.18.0.200"
	namespace  = "kube-system"
)

var allNodes = []string{"egw-ha-control-plane", worker1, worker2, worker3}

func newClient(t *testing.T) client.Client {
	t.Helper()
	cfg, err := config.GetConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: k8s.NewScheme()})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func command(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func dockerExec(t *testing.T, node string, args ...string) string {
	t.Helper()
	out, err := command("docker", append([]string{"exec", node}, args...)...)
	if err != nil {
		t.Fatalf("docker exec %s %v: %v: %s", node, args, err, out)
	}
	return out
}

// managedIPs returns the egress IPs with the managed label on eth0 of node.
func managedIPs(t *testing.T, node string) []string {
	t.Helper()
	out := dockerExec(t, node, "ip", "-4", "-o", "addr", "show", "dev", "eth0", "label", "eth0:egw")
	var ips []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 {
			ips = append(ips, strings.TrimSuffix(f[3], "/32"))
		}
	}
	return ips
}

func hasIP(t *testing.T, node, ip string) bool {
	t.Helper()
	return strings.Contains(dockerExec(t, node, "ip", "-4", "-o", "addr", "show", "dev", "eth0"), " "+ip+"/")
}

func macOf(t *testing.T, node string) string {
	t.Helper()
	return dockerExec(t, node, "cat", "/sys/class/net/eth0/address")
}

// clientSourceIP returns the source IP the target sees for a request from the client pod.
func clientSourceIP() (string, error) {
	out, err := command("kubectl", "exec", "client", "--", "curl", "-sS", "-m", "3", "http://"+targetAddr+"/clientip")
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, out)
	}
	host, _, _ := strings.Cut(out, ":")
	return host, nil
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := cond()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s: %v", timeout, what, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func getEGWHA(ctx context.Context, c client.Client) (*v1alpha1.EgressGatewayHA, error) {
	egw := &v1alpha1.EgressGatewayHA{}
	return egw, c.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, egw)
}

func setActiveNode(t *testing.T, c client.Client, node string) {
	t.Helper()
	patch := fmt.Sprintf(`{"spec":{"activeNode":%q}}`, node)
	egw := &v1alpha1.EgressGatewayHA{}
	egw.Name = v1alpha1.SingletonName
	if err := c.Patch(context.Background(), egw, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatalf("set activeNode=%q: %v", node, err)
	}
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

// running reports whether the kind node container is running.
func running(node string) bool {
	out, err := command("docker", "inspect", "-f", "{{.State.Running}}", node)
	return err == nil && out == "true"
}
