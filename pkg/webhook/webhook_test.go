package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	slimv1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/apis/meta/v1"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/k8s"
)

func egwha(mutate func(*v1alpha1.EgressGatewayHASpec)) *v1alpha1.EgressGatewayHA {
	obj := &v1alpha1.EgressGatewayHA{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SingletonName},
		Spec: v1alpha1.EgressGatewayHASpec{
			EgressNodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"candidate": "true"}},
			Interface:          "eth0",
			EgressIPs:          []string{"10.0.0.100-10.0.0.110", "10.0.0.5"},
		},
	}
	if mutate != nil {
		mutate(&obj.Spec)
	}
	return obj
}

func node(name, ip string, candidate bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}}}
	if candidate {
		n.Labels["candidate"] = "true"
	}
	return n
}

func policy(name string, mutate func(*ciliumv2.EgressGateway)) *ciliumv2.CiliumEgressGatewayPolicy {
	gw := &ciliumv2.EgressGateway{NodeSelector: &slimv1.LabelSelector{
		MatchLabels: map[string]slimv1.MatchLabelsValue{v1alpha1.ActiveLabelKey: "true"}}}
	if mutate != nil {
		mutate(gw)
	}
	return &ciliumv2.CiliumEgressGatewayPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "cilium.io/v2", Kind: "CiliumEgressGatewayPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ciliumv2.CiliumEgressGatewayPolicySpec{EgressGateway: gw},
	}
}

func withIP(ip string) func(*ciliumv2.EgressGateway) {
	return func(gw *ciliumv2.EgressGateway) { gw.EgressIP = ip }
}

func newHandlers(objs ...client.Object) *Handlers {
	s := k8s.NewScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &Handlers{Client: c, Decoder: admission.NewDecoder(s)}
}

func request(t *testing.T, op admissionv1.Operation, obj, old runtime.Object) admission.Request {
	t.Helper()
	raw := func(o runtime.Object) runtime.RawExtension {
		if o == nil {
			return runtime.RawExtension{}
		}
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		return runtime.RawExtension{Raw: b}
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: op, Object: raw(obj), OldObject: raw(old)}}
}

func TestMutatePolicy(t *testing.T) {
	ctx := context.Background()
	h := newHandlers(egwha(nil))
	resp := h.MutatePolicy(ctx, request(t, admissionv1.Create, policy("p", nil), nil))
	if !resp.Allowed || len(resp.Patches) != 1 || resp.Patches[0].Path != "/spec/egressGateway/egressIP" ||
		resp.Patches[0].Value != "10.0.0.100" {
		t.Fatalf("default from egressIPs: %+v", resp)
	}

	h = newHandlers(egwha(func(s *v1alpha1.EgressGatewayHASpec) { s.DefaultEgressIP = "10.0.0.105" }))
	if resp := h.MutatePolicy(ctx, request(t, admissionv1.Create, policy("p", nil), nil)); resp.Patches[0].Value != "10.0.0.105" {
		t.Fatalf("default from defaultEgressIP: %+v", resp)
	}
	for name, p := range map[string]*ciliumv2.CiliumEgressGatewayPolicy{
		"egressIP set": policy("p", withIP("10.0.0.101")),
		"unmanaged": policy("p", func(gw *ciliumv2.EgressGateway) {
			gw.NodeSelector.MatchLabels = map[string]slimv1.MatchLabelsValue{"other": "x"}
		}),
	} {
		if resp := h.MutatePolicy(ctx, request(t, admissionv1.Create, p, nil)); !resp.Allowed || len(resp.Patches) != 0 {
			t.Errorf("%s: %+v", name, resp)
		}
	}

	h = newHandlers()
	if resp := h.MutatePolicy(ctx, request(t, admissionv1.Create, policy("p", nil), nil)); resp.Allowed ||
		!strings.Contains(resp.Result.Message, "EgressGatewayHA default not found") {
		t.Fatalf("missing EgressGatewayHA: %+v", resp)
	}
}

func TestValidatePolicy(t *testing.T) {
	ctx := context.Background()
	h := newHandlers(egwha(nil), node("n1", "10.0.0.5", true))
	for name, tc := range map[string]struct {
		mutate func(*ciliumv2.EgressGateway)
		spec   func(*ciliumv2.CiliumEgressGatewayPolicySpec)
		denied string
	}{
		"valid": {mutate: withIP("10.0.0.110")},
		"extra label": {mutate: func(gw *ciliumv2.EgressGateway) {
			gw.EgressIP = "10.0.0.100"
			gw.NodeSelector.MatchLabels["zone"] = "a"
		}, denied: "nodeSelector must only be matchLabels"},
		"wrong value": {mutate: func(gw *ciliumv2.EgressGateway) {
			gw.EgressIP = "10.0.0.100"
			gw.NodeSelector.MatchLabels[v1alpha1.ActiveLabelKey] = "false"
		}, denied: "nodeSelector must only be matchLabels"},
		"matchExpressions": {mutate: func(gw *ciliumv2.EgressGateway) {
			gw.EgressIP = "10.0.0.100"
			gw.NodeSelector = &slimv1.LabelSelector{MatchExpressions: []slimv1.LabelSelectorRequirement{
				{Key: v1alpha1.ActiveLabelKey, Operator: slimv1.LabelSelectorOpExists}}}
		}, denied: "nodeSelector must only be matchLabels"},
		"interface": {mutate: func(gw *ciliumv2.EgressGateway) {
			gw.EgressIP = "10.0.0.100"
			gw.Interface = "eth0"
		}, denied: "egressGateway.interface must not be set"},
		"egressGateways": {mutate: withIP("10.0.0.100"), spec: func(s *ciliumv2.CiliumEgressGatewayPolicySpec) {
			s.EgressGateways = []ciliumv2.EgressGateway{{}}
		}, denied: "egressGateways is not supported"},
		"no egressIP":     {denied: "egressGateway.egressIP must be set"},
		"IPv6":            {mutate: withIP("fd00::1"), denied: "not an IPv4 address"},
		"not allowed":     {mutate: withIP("10.0.0.111"), denied: "10.0.0.111 is not in EgressGatewayHA spec.egressIPs"},
		"node IP":         {mutate: withIP("10.0.0.5"), denied: "10.0.0.5 is the InternalIP of node n1"},
		"multiple errors": {mutate: func(gw *ciliumv2.EgressGateway) { gw.Interface = "eth0" }, denied: "spec.interface; egressGateway.egressIP must be set"},
	} {
		p := policy("p", tc.mutate)
		if tc.spec != nil {
			tc.spec(&p.Spec)
		}
		resp := h.ValidatePolicy(ctx, request(t, admissionv1.Create, p, nil))
		if tc.denied == "" {
			if !resp.Allowed {
				t.Errorf("%s: denied: %s", name, resp.Result.Message)
			}
		} else if resp.Allowed || !strings.Contains(resp.Result.Message, tc.denied) {
			t.Errorf("%s: allowed=%v message=%q, want denied with %q", name, resp.Allowed, resp.Result.Message, tc.denied)
		}
	}

	unmanaged := policy("p", func(gw *ciliumv2.EgressGateway) {
		gw.NodeSelector.MatchLabels = map[string]slimv1.MatchLabelsValue{"other": "x"}
		gw.Interface = "eth0"
	})
	if resp := h.ValidatePolicy(ctx, request(t, admissionv1.Create, unmanaged, nil)); !resp.Allowed {
		t.Errorf("unmanaged policy denied: %s", resp.Result.Message)
	}

	resp := h.ValidatePolicy(ctx, request(t, admissionv1.Update, policy("p", withIP("10.0.0.101")), policy("p", withIP("10.0.0.100"))))
	if !resp.Allowed || len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "existing connections will be broken") {
		t.Errorf("egressIP change: %+v", resp)
	}
	resp = h.ValidatePolicy(ctx, request(t, admissionv1.Update, policy("p", withIP("10.0.0.100")), policy("p", withIP("10.0.0.100"))))
	if !resp.Allowed || len(resp.Warnings) != 0 {
		t.Errorf("unchanged egressIP: %+v", resp)
	}
}

func TestValidateEGWHA(t *testing.T) {
	ctx := context.Background()
	h := newHandlers(node("n1", "10.0.0.1", true), node("n2", "10.0.0.2", false),
		policy("uses-101", withIP("10.0.0.101")),
		policy("unmanaged", func(gw *ciliumv2.EgressGateway) {
			gw.NodeSelector.MatchLabels = map[string]slimv1.MatchLabelsValue{"other": "x"}
			gw.EgressIP = "10.0.0.109"
		}))
	type tc struct {
		old, obj *v1alpha1.EgressGatewayHA
		denied   string
	}
	set := func(f func(*v1alpha1.EgressGatewayHASpec)) *v1alpha1.EgressGatewayHA { return egwha(f) }
	for name, tc := range map[string]tc{
		"valid":              {obj: set(func(s *v1alpha1.EgressGatewayHASpec) { s.DefaultEgressIP = "10.0.0.5" })},
		"invalid egressIPs":  {obj: set(func(s *v1alpha1.EgressGatewayHASpec) { s.EgressIPs = []string{"10.0.0.9-10.0.0.1"} }), denied: "spec.egressIPs"},
		"default not in IPs": {obj: set(func(s *v1alpha1.EgressGatewayHASpec) { s.DefaultEgressIP = "10.0.0.99" }), denied: "spec.defaultEgressIP 10.0.0.99 is not in spec.egressIPs"},
		"active candidate":   {obj: set(func(s *v1alpha1.EgressGatewayHASpec) { s.ActiveNode = "n1" })},
		"active not found":   {obj: set(func(s *v1alpha1.EgressGatewayHASpec) { s.ActiveNode = "n9" }), denied: "node n9 not found"},
		"active not candidate": {old: set(func(s *v1alpha1.EgressGatewayHASpec) { s.ActiveNode = "n1" }),
			obj: set(func(s *v1alpha1.EgressGatewayHASpec) { s.ActiveNode = "n2" }), denied: "node n2 does not match spec.egressNodeSelector"},
		"unchanged deleted active node": {old: set(func(s *v1alpha1.EgressGatewayHASpec) { s.ActiveNode = "n9" }),
			obj: set(func(s *v1alpha1.EgressGatewayHASpec) { s.ActiveNode = "n9"; s.Interface = "eth1" })},
		"remove used IP": {old: set(nil), obj: set(func(s *v1alpha1.EgressGatewayHASpec) { s.EgressIPs = []string{"10.0.0.102-10.0.0.110"} }),
			denied: "egress IPs still used by policies: uses-101 (10.0.0.101)"},
		"remove unused IP": {old: set(nil), obj: set(func(s *v1alpha1.EgressGatewayHASpec) { s.EgressIPs = []string{"10.0.0.100-10.0.0.101"} })},
	} {
		op := admissionv1.Create
		var old runtime.Object
		if tc.old != nil {
			op, old = admissionv1.Update, tc.old
		}
		resp := h.ValidateEGWHA(ctx, request(t, op, tc.obj, old))
		if tc.denied == "" {
			if !resp.Allowed {
				t.Errorf("%s: denied: %s", name, resp.Result.Message)
			}
		} else if resp.Allowed || !strings.Contains(resp.Result.Message, tc.denied) {
			t.Errorf("%s: allowed=%v message=%q, want denied with %q", name, resp.Allowed, resp.Result.Message, tc.denied)
		}
	}
}

func TestCert(t *testing.T) {
	now := time.Now()
	names := CertConfig{Namespace: "ns", ServiceName: "svc"}.dnsNames()
	data, err := generateCert(names, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := validCert(data, names, now); err != nil {
		t.Fatalf("fresh cert invalid: %v", err)
	}
	if err := validCert(data, []string{"other.ns.svc"}, now); err == nil {
		t.Error("cert valid for a name it does not cover")
	}
	if err := validCert(data, names, now.Add(certValidity-renewBefore/2)); err == nil {
		t.Error("cert close to expiry is still valid")
	}
	other, _ := generateCert(names, now)
	data["ca.crt"] = other["ca.crt"]
	if err := validCert(data, names, now); err == nil {
		t.Error("cert valid with a different CA")
	}
}

func TestEnsureCert(t *testing.T) {
	ctx := context.Background()
	mwc := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "m"},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{Name: "a.example.com"}}}
	vwc := &admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "v"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{Name: "a.example.com"}, {Name: "b.example.com"}}}
	c := fake.NewClientBuilder().WithScheme(k8s.NewScheme()).WithObjects(mwc, vwc).Build()
	cfg := CertConfig{Namespace: "ns", SecretName: "cert", ServiceName: "svc",
		MutatingWebhookConfiguration: "m", ValidatingWebhookConfiguration: "v"}

	pair, err := EnsureCert(ctx, c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "cert"}, sec); err != nil {
		t.Fatal(err)
	}
	ca := sec.Data["ca.crt"]
	if err := c.Get(ctx, client.ObjectKeyFromObject(mwc), mwc); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(vwc), vwc); err != nil {
		t.Fatal(err)
	}
	if string(mwc.Webhooks[0].ClientConfig.CABundle) != string(ca) ||
		string(vwc.Webhooks[0].ClientConfig.CABundle) != string(ca) || string(vwc.Webhooks[1].ClientConfig.CABundle) != string(ca) {
		t.Fatal("caBundle not set on all webhooks")
	}
	if err := validCert(sec.Data, cfg.dnsNames(), time.Now()); err != nil || len(pair.Certificate) == 0 {
		t.Fatalf("returned certificate: %v", err)
	}

	// A second replica reuses the stored certificate.
	if _, err := EnsureCert(ctx, c, cfg); err != nil {
		t.Fatal(err)
	}
	again := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "cert"}, again); err != nil {
		t.Fatal(err)
	}
	if string(again.Data["ca.crt"]) != string(ca) {
		t.Fatal("certificate regenerated although still valid")
	}
}
