package netops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const inNetNSEnv = "NETOPS_TEST_IN_NETNS"

// TestMain re-executes the tests in a new network namespace when running as
// root, so the tests never touch host interfaces.
func TestMain(m *testing.M) {
	if os.Getenv(inNetNSEnv) == "" && os.Geteuid() == 0 {
		if _, err := exec.LookPath("unshare"); err == nil {
			cmd := exec.Command("unshare", append([]string{"-n", "--", os.Args[0]}, os.Args[1:]...)...)
			cmd.Env = append(os.Environ(), inNetNSEnv+"=1")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					os.Exit(ee.ExitCode())
				}
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// testbed is a veth pair: local "egw0" in the test netns and "egw1" in a peer
// netns that owns 10.99.0.2/24.
type testbed struct {
	peerPID string
	peerMAC net.HardwareAddr
	local   *net.Interface
}

func (b *testbed) peer(t *testing.T, args ...string) string {
	t.Helper()
	return run(t, append([]string{"nsenter", "-t", b.peerPID, "-n"}, args...)...)
}

func newTestbed(t *testing.T) *testbed {
	t.Helper()
	if os.Getenv(inNetNSEnv) == "" {
		t.Skip("requires root and unshare to run in an isolated network namespace")
	}
	peer := exec.Command("unshare", "-n", "sleep", "600")
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Process.Kill(); _ = peer.Wait() })
	b := &testbed{peerPID: strconv.Itoa(peer.Process.Pid)}

	// unshare may not have switched namespaces yet right after Start.
	self, _ := os.Readlink("/proc/self/ns/net")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if ns, err := os.Readlink("/proc/" + b.peerPID + "/ns/net"); err == nil && ns != self {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("peer did not enter a new network namespace")
		}
	}

	run(t, "ip", "link", "add", "egw0", "type", "veth", "peer", "name", "egw1")
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "egw0").Run() })
	run(t, "ip", "link", "set", "egw1", "netns", b.peerPID)
	run(t, "ip", "link", "set", "egw0", "up")
	b.peer(t, "ip", "link", "set", "egw1", "up")
	b.peer(t, "ip", "addr", "add", "10.99.0.2/24", "dev", "egw1")

	// /sys follows the mount namespace, so read the MAC through netlink instead.
	fields := strings.Fields(b.peer(t, "ip", "-o", "link", "show", "egw1"))
	mac := fields[slices.Index(fields, "link/ether")+1]
	var err error
	if b.peerMAC, err = net.ParseMAC(mac); err != nil {
		t.Fatal(err)
	}
	if b.local, err = net.InterfaceByName("egw0"); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLabel(t *testing.T) {
	for iface, want := range map[string]string{
		"eth0":           "eth0:egw",
		"ens1f0np0":      "ens1f0np0:egw",
		"enp175s0f0np0":  "enp175s0f0np0:e",
		"enp175s0f0np01": "",
	} {
		got, err := Label(iface)
		if want == "" {
			if err == nil {
				t.Errorf("Label(%q) = %q, want error", iface, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("Label(%q) = %q, %v, want %q", iface, got, err, want)
		}
	}
}

func TestAddListDel(t *testing.T) {
	newTestbed(t)
	m := New()
	ip1, ip2 := netip.MustParseAddr("10.99.0.10"), netip.MustParseAddr("10.99.0.11")

	// Unmanaged addresses must be ignored by List.
	run(t, "ip", "addr", "add", "10.99.0.1/24", "dev", "egw0")
	run(t, "ip", "addr", "add", "10.99.0.12/32", "dev", "egw0")
	run(t, "ip", "addr", "add", "10.99.0.13/24", "dev", "egw0", "label", "egw0:egw")

	for _, ip := range []netip.Addr{ip1, ip2, ip1} {
		if err := m.Add("egw0", ip); err != nil {
			t.Fatalf("Add(%s): %v", ip, err)
		}
	}
	got, err := m.List("egw0")
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, netip.Addr.Compare)
	if !slices.Equal(got, []netip.Addr{ip1, ip2}) {
		t.Fatalf("List() = %v, want [%s %s]", got, ip1, ip2)
	}
	if out := run(t, "ip", "-4", "-o", "addr", "show", "dev", "egw0"); !strings.Contains(out, "10.99.0.10/32 scope global egw0:egw") {
		t.Errorf("address not labelled as expected:\n%s", out)
	}

	for _, ip := range []netip.Addr{ip1, ip1} {
		if err := m.Del("egw0", ip); err != nil {
			t.Fatalf("Del(%s): %v", ip, err)
		}
	}
	if got, _ := m.List("egw0"); !slices.Equal(got, []netip.Addr{ip2}) {
		t.Fatalf("List() after Del = %v, want [%s]", got, ip2)
	}
	all, err := m.ListAll()
	if err != nil || len(all) != 1 || !slices.Equal(all["egw0"], []netip.Addr{ip2}) {
		t.Fatalf("ListAll() = %v, %v, want map[egw0:[%s]]", all, err, ip2)
	}
	if out := run(t, "ip", "-4", "-o", "addr", "show", "dev", "egw0"); !strings.Contains(out, "10.99.0.12/32") {
		t.Errorf("unmanaged address was removed:\n%s", out)
	}

	if _, err := m.List("missing0"); !errors.Is(err, ErrInterfaceNotFound) {
		t.Errorf("List(missing0) error = %v, want ErrInterfaceNotFound", err)
	}
	if err := m.Add("missing0", ip1); !errors.Is(err, ErrInterfaceNotFound) {
		t.Errorf("Add(missing0) error = %v, want ErrInterfaceNotFound", err)
	}
	if err := m.Del("missing0", ip1); err != nil {
		t.Errorf("Del(missing0) = %v, want nil", err)
	}
}

func TestDetect(t *testing.T) {
	b := newTestbed(t)
	m := New()
	m.DAD = DADConfig{Probes: 2, Interval: 50 * time.Millisecond, Wait: 300 * time.Millisecond}
	ctx := context.Background()

	err := m.Detect(ctx, "egw0", netip.MustParseAddr("10.99.0.2"))
	var c *Conflict
	if !errors.As(err, &c) {
		t.Fatalf("Detect(held IP) = %v, want *Conflict", err)
	}
	if c.MAC.String() != b.peerMAC.String() {
		t.Errorf("conflict MAC = %s, want %s", c.MAC, b.peerMAC)
	}
	if !strings.Contains(c.Error(), "10.99.0.2 is answered by "+b.peerMAC.String()) {
		t.Errorf("unexpected message %q", c.Error())
	}

	start := time.Now()
	if err := m.Detect(ctx, "egw0", netip.MustParseAddr("10.99.0.3")); err != nil {
		t.Fatalf("Detect(free IP) = %v, want nil", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("Detect returned after %v, before the wait time", d)
	}

	if err := m.Detect(ctx, "missing0", netip.MustParseAddr("10.99.0.3")); !errors.Is(err, ErrInterfaceNotFound) {
		t.Errorf("Detect(missing0) = %v, want ErrInterfaceNotFound", err)
	}
}

func TestSelectInterface(t *testing.T) {
	pfx := netip.MustParsePrefix
	ip := netip.MustParseAddr
	subnets := []ifaceSubnet{
		{"cilium_host", pfx("10.244.1.5/32")},
		{"eth0", pfx("172.18.0.0/16")},
		{"eth1", pfx("192.168.10.0/24")},
	}
	defaults := []defaultRoute{{"eth1", 200}, {"eth0", 100}}
	for name, tc := range map[string]struct {
		ips  []netip.Addr
		want string
	}{
		"subnet match":                {[]netip.Addr{ip("192.168.10.100")}, "eth1"},
		"earlier IP wins":             {[]netip.Addr{ip("10.0.0.1"), ip("172.18.255.200"), ip("192.168.10.1")}, "eth0"},
		"host route ignored":          {[]netip.Addr{ip("10.244.1.5")}, "eth0"},
		"default route lowest metric": {[]netip.Addr{ip("10.0.0.1")}, "eth0"},
	} {
		if got, err := selectInterface(subnets, defaults, tc.ips); err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v, want %q", name, got, err, tc.want)
		}
	}
	if _, err := selectInterface(subnets, nil, []netip.Addr{ip("10.0.0.1")}); !errors.Is(err, ErrInterfaceNotFound) {
		t.Errorf("no match and no default route: err = %v, want ErrInterfaceNotFound", err)
	}
}

func TestFindInterface(t *testing.T) {
	newTestbed(t)
	run(t, "ip", "addr", "add", "10.99.0.1/24", "dev", "egw0")
	got, err := New().FindInterface([]netip.Addr{netip.MustParseAddr("10.99.0.10")})
	if err != nil || got != "egw0" {
		t.Fatalf("FindInterface() = %q, %v, want egw0", got, err)
	}
	if _, err := New().FindInterface([]netip.Addr{netip.MustParseAddr("10.98.0.10")}); !errors.Is(err, ErrInterfaceNotFound) {
		t.Errorf("FindInterface(no match, no default route) err = %v, want ErrInterfaceNotFound", err)
	}
}

func TestAnnounce(t *testing.T) {
	b := newTestbed(t)
	m := New()
	ip := netip.MustParseAddr("10.99.0.10")

	// Linux updates an existing neighbor entry on gratuitous ARP but does not
	// create one by default, so seed a stale entry with a wrong MAC.
	b.peer(t, "ip", "neigh", "replace", ip.String(), "lladdr", "02:00:00:00:00:99", "dev", "egw1", "nud", "stale")
	if err := m.Add("egw0", ip); err != nil {
		t.Fatal(err)
	}
	if err := m.Announce("egw0", ip); err != nil {
		t.Fatal(err)
	}
	want := b.local.HardwareAddr.String()
	deadline := time.Now().Add(2 * time.Second)
	for {
		out := b.peer(t, "ip", "neigh", "show", ip.String(), "dev", "egw1")
		if strings.Contains(out, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer neighbor entry not updated to %s: %s", want, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
