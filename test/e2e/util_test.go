//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
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

func patchLabel(ctx context.Context, c client.Client, obj client.Object, value *string) error {
	v := "null"
	if value != nil {
		v = fmt.Sprintf("%q", *value)
	}
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%s}}}`, v1alpha1.ActiveLabelKey, v)
	return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(patch)))
}

// setActiveLabel moves the active label to node on Node and CiliumNode, the
// same way the controller does in the second phase. An empty node removes it.
func setActiveLabel(t *testing.T, c client.Client, node string) {
	t.Helper()
	ctx := context.Background()
	val := v1alpha1.ActiveLabelValue
	for _, n := range allNodes {
		var v *string
		if n == node {
			v = &val
		}
		for _, obj := range []client.Object{&corev1.Node{}, &ciliumv2.CiliumNode{}} {
			obj.SetName(n)
			if err := patchLabel(ctx, c, obj, v); err != nil {
				t.Fatalf("label %T %s: %v", obj, n, err)
			}
		}
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
