package agent

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	slimv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/k8s"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/metrics"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/netops"
)

const node = "node1"

var (
	ip200 = netip.MustParseAddr("172.18.0.200")
	ip201 = netip.MustParseAddr("172.18.0.201")
	ip202 = netip.MustParseAddr("172.18.0.202")
)

type fakeOps struct {
	ifaces    map[string][]netip.Addr
	conflicts map[netip.Addr]string
	detected  []netip.Addr
	announced []netip.Addr
}

func newFakeOps() *fakeOps {
	return &fakeOps{ifaces: map[string][]netip.Addr{"eth0": nil, "eth1": nil}, conflicts: map[netip.Addr]string{}}
}

func (f *fakeOps) List(iface string) ([]netip.Addr, error) {
	ips, ok := f.ifaces[iface]
	if !ok {
		return nil, netops.ErrInterfaceNotFound
	}
	return slices.Clone(ips), nil
}

func (f *fakeOps) ListAll() (map[string][]netip.Addr, error) {
	out := map[string][]netip.Addr{}
	for k, v := range f.ifaces {
		if len(v) > 0 {
			out[k] = slices.Clone(v)
		}
	}
	return out, nil
}

func (f *fakeOps) Add(iface string, ip netip.Addr) error {
	if _, ok := f.ifaces[iface]; !ok {
		return netops.ErrInterfaceNotFound
	}
	if !slices.Contains(f.ifaces[iface], ip) {
		f.ifaces[iface] = append(f.ifaces[iface], ip)
	}
	return nil
}

func (f *fakeOps) Del(iface string, ip netip.Addr) error {
	f.ifaces[iface] = slices.DeleteFunc(f.ifaces[iface], func(a netip.Addr) bool { return a == ip })
	return nil
}

func (f *fakeOps) Detect(_ context.Context, _ string, ip netip.Addr) error {
	f.detected = append(f.detected, ip)
	if mac, ok := f.conflicts[ip]; ok {
		hw, _ := net.ParseMAC(mac)
		return &netops.Conflict{IP: ip, MAC: hw}
	}
	return nil
}

func (f *fakeOps) Announce(_ string, ip netip.Addr) error {
	f.announced = append(f.announced, ip)
	return nil
}

// fakeStatus records applied status and writes it back so the next reconcile sees it.
type fakeStatus struct {
	c       client.Client
	applies int
	active  *v1alpha1.ActiveNodeStatus
	cond    metav1.Condition
}

func (s *fakeStatus) Apply(ctx context.Context, active *v1alpha1.ActiveNodeStatus, cond metav1.Condition) error {
	s.applies++
	s.active, s.cond = active, cond
	egw := &v1alpha1.EgressGatewayHA{}
	if err := s.c.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, egw); err != nil {
		return err
	}
	egw.Status.ActiveNode = active
	meta.SetStatusCondition(&egw.Status.Conditions, cond)
	return s.c.Status().Update(ctx, egw)
}

func egwha(active string) *v1alpha1.EgressGatewayHA {
	return &v1alpha1.EgressGatewayHA{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName, Generation: 1},
		Spec: v1alpha1.EgressGatewayHASpec{
			CandidateNodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"candidate": "true"}},
			Interface:             "eth0",
			EgressIPs:             []string{"172.18.0.200-172.18.0.210"},
			ActiveNode:            active,
		},
	}
}

func policy(name, egressIP string, managed bool) *ciliumv2.CiliumEgressGatewayPolicy {
	sel := map[string]slimv1.MatchLabelsValue{"other": "x"}
	if managed {
		sel = map[string]slimv1.MatchLabelsValue{v1alpha1.ActiveLabelKey: "true"}
	}
	return &ciliumv2.CiliumEgressGatewayPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ciliumv2.CiliumEgressGatewayPolicySpec{EgressGateway: &ciliumv2.EgressGateway{
			NodeSelector: &slimv1.LabelSelector{MatchLabels: sel},
			EgressIP:     egressIP,
		}},
	}
}

type env struct {
	r      *Reconciler
	c      client.Client
	ops    *fakeOps
	status *fakeStatus
	now    time.Time
}

func newEnv(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) *env {
	t.Helper()
	objs = append(objs,
		&ciliumv2.CiliumNode{ObjectMeta: metav1.ObjectMeta{Name: node}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node, Labels: map[string]string{"candidate": "true"}}},
	)
	b := fake.NewClientBuilder().WithScheme(k8s.NewScheme()).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.EgressGatewayHA{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	c := b.Build()
	e := &env{c: c, ops: newFakeOps(), status: &fakeStatus{c: c}, now: time.Unix(1_700_000_000, 0)}
	e.r = &Reconciler{Client: c, Ops: e.ops, Status: e.status, Now: func() time.Time { return e.now },
		Config: Config{NodeName: node, ReconcileInterval: 10 * time.Second}}
	return e
}

func (e *env) reconcile(t *testing.T) error {
	t.Helper()
	res, err := e.r.Reconcile(context.Background(), reconcile.Request{})
	// On errors controller-runtime retries with backoff instead.
	if err == nil && (res.RequeueAfter != e.r.requeue().RequeueAfter || res.RequeueAfter == 0) {
		t.Errorf("RequeueAfter = %v", res.RequeueAfter)
	}
	return err
}

func (e *env) syncedAnnotation(t *testing.T) string {
	t.Helper()
	cn := &ciliumv2.CiliumNode{}
	if err := e.c.Get(context.Background(), client.ObjectKey{Name: node}, cn); err != nil {
		t.Fatal(err)
	}
	return cn.Annotations[SyncedAnnotation]
}

func TestActiveNodeConfiguresEgressIPs(t *testing.T) {
	e := newEnv(t, nil, egwha(node),
		policy("a", "172.18.0.201", true),
		policy("b", "172.18.0.200", true),
		policy("c", "172.18.0.200", true),  // shared IP
		policy("d", "172.18.0.205", false), // not managed
		policy("e", "10.0.0.1", true),      // not allowed by spec.egressIPs
		policy("f", "", true),              // no egressIP
	)
	if e.r.ReadyCheck(nil) == nil {
		t.Error("ready before first sync")
	}
	if err := e.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if e.r.ReadyCheck(nil) != nil {
		t.Error("not ready after first sync")
	}
	if want := []netip.Addr{ip200, ip201}; !slices.Equal(e.ops.ifaces["eth0"], want) ||
		!slices.Equal(e.ops.detected, want) || !slices.Equal(e.ops.announced, want) {
		t.Fatalf("eth0=%v detected=%v announced=%v, want %v", e.ops.ifaces["eth0"], e.ops.detected, e.ops.announced, want)
	}
	if e.syncedAnnotation(t) == "" {
		t.Error("CiliumNode annotation not patched after adding egress IPs")
	}
	s := e.status
	if s.applies != 1 || s.active.Name != node || !s.active.Ready ||
		!slices.Equal(s.active.EgressIPs, []string{"172.18.0.200", "172.18.0.201"}) {
		t.Fatalf("status = %d applies, %+v", s.applies, s.active)
	}
	if s.cond.Type != v1alpha1.ConditionGatewayReady || s.cond.Status != metav1.ConditionTrue ||
		s.cond.Reason != v1alpha1.ReasonEgressIPsConfigured || s.cond.ObservedGeneration != 1 {
		t.Fatalf("condition = %+v", s.cond)
	}

	// A steady state reconcile does not probe, patch or write status again.
	e.ops.detected, e.ops.announced = nil, nil
	if err := e.c.Patch(context.Background(), &ciliumv2.CiliumNode{ObjectMeta: metav1.ObjectMeta{Name: node}},
		client.RawPatch("application/merge-patch+json", []byte(`{"metadata":{"annotations":null}}`))); err != nil {
		t.Fatal(err)
	}
	if err := e.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if len(e.ops.detected) != 0 || len(e.ops.announced) != 0 || e.status.applies != 1 || e.syncedAnnotation(t) != "" {
		t.Errorf("steady state: detected=%v announced=%v applies=%d annotation=%q",
			e.ops.detected, e.ops.announced, e.status.applies, e.syncedAnnotation(t))
	}

	// An egress IP removed by someone else is added back.
	_ = e.ops.Del("eth0", ip201)
	if err := e.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.ops.ifaces["eth0"], ip201) || e.syncedAnnotation(t) == "" {
		t.Errorf("removed IP not restored: eth0=%v", e.ops.ifaces["eth0"])
	}
}

func TestActiveNodeKeepsExistingIPsWithoutDAD(t *testing.T) {
	e := newEnv(t, nil, egwha(node), policy("a", "172.18.0.200", true))
	e.ops.ifaces["eth0"] = []netip.Addr{ip200}
	// The IP left by the previous agent answers ARP itself; it must not be treated as a conflict.
	e.ops.conflicts[ip200] = "aa:bb:cc:dd:ee:ff"
	if err := e.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if len(e.ops.detected) != 0 || e.syncedAnnotation(t) != "" || !e.status.active.Ready {
		t.Errorf("detected=%v annotation=%q status=%+v", e.ops.detected, e.syncedAnnotation(t), e.status.active)
	}
}

func TestActiveNodeConflict(t *testing.T) {
	e := newEnv(t, nil, egwha(node), policy("a", "172.18.0.200", true), policy("b", "172.18.0.201", true))
	e.ops.conflicts[ip200] = "aa:bb:cc:dd:ee:ff"
	conflictsBefore := testutil.ToFloat64(metrics.AgentConflictsTotal.WithLabelValues("172.18.0.200"))
	if err := e.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.ops.ifaces["eth0"], []netip.Addr{ip201}) {
		t.Fatalf("eth0 = %v, want only %s", e.ops.ifaces["eth0"], ip201)
	}
	if got := testutil.ToFloat64(metrics.AgentConflictsTotal.WithLabelValues("172.18.0.200")) - conflictsBefore; got != 1 {
		t.Errorf("agent_dad_conflicts_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.AgentEgressIP.WithLabelValues("eth0", "172.18.0.201")); got != 1 {
		t.Errorf("agent_egress_ip{172.18.0.201} = %v, want 1", got)
	}
	s := e.status
	if s.active.Ready || !slices.Equal(s.active.EgressIPs, []string{"172.18.0.201"}) ||
		s.cond.Status != metav1.ConditionFalse || s.cond.Reason != v1alpha1.ReasonEgressIPConflict ||
		s.cond.Message != "172.18.0.200 is answered by aa:bb:cc:dd:ee:ff, not added" {
		t.Fatalf("status = %+v, condition = %+v", s.active, s.cond)
	}
	firstTransition := s.cond.LastTransitionTime

	// The conflict goes away: the IP is added and the condition transitions.
	delete(e.ops.conflicts, ip200)
	e.now = e.now.Add(time.Minute)
	if err := e.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if !s.active.Ready || s.cond.Status != metav1.ConditionTrue || s.cond.LastTransitionTime.Equal(&firstTransition) {
		t.Fatalf("after conflict resolved: status = %+v, condition = %+v", s.active, s.cond)
	}
}

func TestActiveNodeInterfaceNotFound(t *testing.T) {
	obj := egwha(node)
	obj.Spec.Interface = "missing0"
	e := newEnv(t, nil, obj, policy("a", "172.18.0.200", true))
	if err := e.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if e.status.active.Ready || e.status.cond.Reason != v1alpha1.ReasonInterfaceNotFound ||
		!strings.Contains(e.status.cond.Message, "interface missing0 not found on node1") {
		t.Fatalf("status = %+v, condition = %+v", e.status.active, e.status.cond)
	}
}

func TestActiveNodeRemovesStaleIPs(t *testing.T) {
	e := newEnv(t, nil, egwha(node), policy("a", "172.18.0.200", true))
	e.ops.ifaces["eth0"] = []netip.Addr{ip200, ip202} // 202 is no longer used by any policy
	e.ops.ifaces["eth1"] = []netip.Addr{ip201}        // left on the previous spec.interface
	if err := e.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.ops.ifaces["eth0"], []netip.Addr{ip200}) || len(e.ops.ifaces["eth1"]) != 0 {
		t.Fatalf("ifaces = %v", e.ops.ifaces)
	}
}

func TestStandbyNodeReleasesIPs(t *testing.T) {
	for _, active := range []string{"node2", ""} {
		e := newEnv(t, nil, egwha(active), policy("a", "172.18.0.200", true))
		e.ops.ifaces["eth0"] = []netip.Addr{ip200}
		e.ops.ifaces["eth1"] = []netip.Addr{ip201}
		if err := e.reconcile(t); err != nil {
			t.Fatal(err)
		}
		if all, _ := e.ops.ListAll(); len(all) != 0 || e.status.applies != 0 {
			t.Errorf("activeNode=%q: ifaces=%v applies=%d", active, all, e.status.applies)
		}
	}
}

func TestKeepsStateWhenAPIUnavailable(t *testing.T) {
	failGet := &interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errors.New("connection refused")
	}}
	e := newEnv(t, failGet, egwha("node2"))
	e.ops.ifaces["eth0"] = []netip.Addr{ip200}
	if err := e.reconcile(t); err == nil {
		t.Fatal("want error")
	}
	if !slices.Equal(e.ops.ifaces["eth0"], []netip.Addr{ip200}) || e.r.ReadyCheck(nil) == nil {
		t.Errorf("eth0=%v ready=%v", e.ops.ifaces["eth0"], e.r.ReadyCheck(nil) == nil)
	}

	// A missing EgressGatewayHA also keeps the current state.
	e = newEnv(t, nil)
	e.ops.ifaces["eth0"] = []netip.Addr{ip200}
	if err := e.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.ops.ifaces["eth0"], []netip.Addr{ip200}) || e.r.ReadyCheck(nil) != nil {
		t.Errorf("not found: eth0=%v ready=%v", e.ops.ifaces["eth0"], e.r.ReadyCheck(nil) == nil)
	}
}

func TestPeriodicGARP(t *testing.T) {
	e := newEnv(t, nil, egwha(node), policy("a", "172.18.0.200", true))
	e.r.Config.GARPInterval = 3 * time.Second
	if err := e.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if got := e.r.requeue().RequeueAfter; got != 3*time.Second {
		t.Errorf("requeue = %v, want the shorter GARP interval", got)
	}
	e.now = e.now.Add(time.Second)
	_ = e.reconcile(t)
	if len(e.ops.announced) != 1 {
		t.Fatalf("announced %v before the GARP interval elapsed", e.ops.announced)
	}
	e.now = e.now.Add(3 * time.Second)
	_ = e.reconcile(t)
	if !slices.Equal(e.ops.announced, []netip.Addr{ip200, ip200}) {
		t.Fatalf("announced = %v, want a periodic GARP", e.ops.announced)
	}
}

func TestCleanupOnExit(t *testing.T) {
	notCandidate := egwha("node2")
	notCandidate.Spec.CandidateNodeSelector.MatchLabels = map[string]string{"candidate": "no"}
	activeNotCandidate := egwha(node)
	activeNotCandidate.Spec.CandidateNodeSelector.MatchLabels = map[string]string{"candidate": "no"}
	for name, tc := range map[string]struct {
		egw     *v1alpha1.EgressGatewayHA
		funcs   *interceptor.Funcs
		release bool
	}{
		"active node keeps":             {egw: egwha(node)},
		"standby candidate keeps":       {egw: egwha("node2")},
		"no longer candidate releases":  {egw: notCandidate, release: true},
		"missing EgressGatewayHA keeps": {},
		"API error keeps": {egw: notCandidate, funcs: &interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("connection refused")
			}}},
	} {
		t.Run(name, func(t *testing.T) {
			var objs []client.Object
			if tc.egw != nil {
				objs = append(objs, tc.egw)
			}
			e := newEnv(t, tc.funcs, objs...)
			e.ops.ifaces["eth0"] = []netip.Addr{ip200}
			_ = e.r.CleanupOnExit(context.Background(), e.c)
			if released := len(e.ops.ifaces["eth0"]) == 0; released != tc.release {
				t.Errorf("released = %v, want %v", released, tc.release)
			}
		})
	}
}
