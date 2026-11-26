// Package webhook validates and defaults managed CiliumEgressGatewayPolicies
// and validates EgressGatewayHA. Only static checks are done here; whether an
// egress IP really works is reported at runtime through the conditions.
package webhook

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/iprange"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/k8s"
)

const (
	MutatePolicyPath   = "/mutate-ciliumegressgatewaypolicy"
	ValidatePolicyPath = "/validate-ciliumegressgatewaypolicy"
	ValidateEGWHAPath  = "/validate-egressgatewayha"
)

type Handlers struct {
	ActiveLabel k8s.ActiveLabel
	Client      client.Reader
	Decoder     admission.Decoder
}

func (h *Handlers) getEGWHA(ctx context.Context) (*v1alpha1.EgressGatewayHA, error) {
	egw := &v1alpha1.EgressGatewayHA{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, egw); err != nil {
		return nil, err
	}
	return egw, nil
}

// DefaultEgressIP returns spec.defaultEgressIP, or the first IP of spec.egressIPs.
func DefaultEgressIP(spec *v1alpha1.EgressGatewayHASpec) (string, error) {
	if spec.DefaultEgressIP != "" {
		return spec.DefaultEgressIP, nil
	}
	l, err := iprange.Parse(spec.EgressIPs)
	if err != nil {
		return "", err
	}
	ip, ok := l.First()
	if !ok {
		return "", fmt.Errorf("spec.egressIPs is empty")
	}
	return ip.String(), nil
}

func egwhaError(err error) admission.Response {
	if apierrors.IsNotFound(err) {
		return admission.Denied("EgressGatewayHA default not found")
	}
	return admission.Errored(http.StatusInternalServerError, err)
}

// MutatePolicy fills egressGateway.egressIP of a managed policy with the default egress IP.
func (h *Handlers) MutatePolicy(ctx context.Context, req admission.Request) admission.Response {
	p := &ciliumv2.CiliumEgressGatewayPolicy{}
	if err := h.Decoder.Decode(req, p); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if !h.ActiveLabel.IsManaged(p) || p.Spec.EgressGateway.EgressIP != "" {
		return admission.Allowed("")
	}
	egw, err := h.getEGWHA(ctx)
	if err != nil {
		return egwhaError(err)
	}
	ip, err := DefaultEgressIP(&egw.Spec)
	if err != nil {
		return admission.Denied(fmt.Sprintf("cannot get the default egress IP of EgressGatewayHA default: %v", err))
	}
	return admission.Patched("egressIP defaulted",
		jsonpatch.JsonPatchOperation{Operation: "add", Path: "/spec/egressGateway/egressIP", Value: ip})
}

// ValidatePolicy validates a managed policy.
func (h *Handlers) ValidatePolicy(ctx context.Context, req admission.Request) admission.Response {
	p := &ciliumv2.CiliumEgressGatewayPolicy{}
	if err := h.Decoder.Decode(req, p); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if !h.ActiveLabel.IsManaged(p) {
		return admission.Allowed("")
	}
	gw := p.Spec.EgressGateway
	var errs []string

	sel := gw.NodeSelector
	if len(sel.MatchExpressions) > 0 || len(sel.MatchLabels) != 1 ||
		string(sel.MatchLabels[h.ActiveLabel.Key]) != h.ActiveLabel.Value {
		errs = append(errs, fmt.Sprintf("egressGateway.nodeSelector must only be matchLabels {%s: %q}",
			h.ActiveLabel.Key, h.ActiveLabel.Value))
	}
	if gw.Interface != "" {
		errs = append(errs, "egressGateway.interface must not be set: Cilium ignores policies with both interface and egressIP; "+
			"the interface is set by EgressGatewayHA spec.interface")
	}
	if len(p.Spec.EgressGateways) > 0 {
		errs = append(errs, "egressGateways is not supported, use egressGateway")
	}
	if msg, err := h.checkEgressIP(ctx, gw.EgressIP); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	} else if msg != "" {
		errs = append(errs, msg)
	}
	if len(errs) > 0 {
		return admission.Denied(strings.Join(errs, "; "))
	}

	resp := admission.Allowed("")
	if req.Operation == admissionv1.Update {
		old := &ciliumv2.CiliumEgressGatewayPolicy{}
		if err := h.Decoder.DecodeRaw(req.OldObject, old); err == nil && h.ActiveLabel.IsManaged(old) &&
			old.Spec.EgressGateway.EgressIP != "" && old.Spec.EgressGateway.EgressIP != gw.EgressIP {
			resp = resp.WithWarnings(fmt.Sprintf("egressIP changed from %s to %s: existing connections will be broken",
				old.Spec.EgressGateway.EgressIP, gw.EgressIP))
		}
	}
	return resp
}

// checkEgressIP returns why ip cannot be used, or "".
func (h *Handlers) checkEgressIP(ctx context.Context, s string) (string, error) {
	if s == "" {
		return "egressGateway.egressIP must be set", nil
	}
	ip, err := iprange.ParseIPv4(s)
	if err != nil {
		return fmt.Sprintf("egressGateway.egressIP: %v", err), nil
	}
	egw, err := h.getEGWHA(ctx)
	if apierrors.IsNotFound(err) {
		return "EgressGatewayHA default not found", nil
	}
	if err != nil {
		return "", err
	}
	allowed, err := iprange.Parse(egw.Spec.EgressIPs)
	if err != nil {
		return fmt.Sprintf("EgressGatewayHA default has invalid spec.egressIPs: %v", err), nil
	}
	if !allowed.Contains(ip) {
		return fmt.Sprintf("egressGateway.egressIP %s is not in EgressGatewayHA spec.egressIPs %v", s, egw.Spec.EgressIPs), nil
	}
	nodes := &corev1.NodeList{}
	if err := h.Client.List(ctx, nodes); err != nil {
		return "", err
	}
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP && a.Address == ip.String() {
				return fmt.Sprintf("egressGateway.egressIP %s is the InternalIP of node %s", s, n.Name), nil
			}
		}
	}
	return "", nil
}

// ValidateEGWHA validates changes to EgressGatewayHA made by users. Requests
// from the controller are excluded by the webhook matchConditions.
func (h *Handlers) ValidateEGWHA(ctx context.Context, req admission.Request) admission.Response {
	obj := &v1alpha1.EgressGatewayHA{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	var old *v1alpha1.EgressGatewayHA
	if req.Operation == admissionv1.Update {
		old = &v1alpha1.EgressGatewayHA{}
		if err := h.Decoder.DecodeRaw(req.OldObject, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	}
	spec := &obj.Spec
	var errs []string

	allowed, err := iprange.Parse(spec.EgressIPs)
	if err != nil {
		errs = append(errs, fmt.Sprintf("spec.egressIPs: %v", err))
	}
	if spec.DefaultEgressIP != "" {
		ip, err := iprange.ParseIPv4(spec.DefaultEgressIP)
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("spec.defaultEgressIP: %v", err))
		case allowed != nil && !allowed.Contains(ip):
			errs = append(errs, fmt.Sprintf("spec.defaultEgressIP %s is not in spec.egressIPs", spec.DefaultEgressIP))
		}
	}
	sel, err := metav1.LabelSelectorAsSelector(&spec.EgressNodeSelector)
	if err != nil {
		errs = append(errs, fmt.Sprintf("spec.egressNodeSelector: %v", err))
	}

	// Only a changed activeNode is checked, so other edits still work after the
	// active node is deleted.
	if spec.ActiveNode != "" && sel != nil && (old == nil || old.Spec.ActiveNode != spec.ActiveNode) {
		n := &corev1.Node{}
		if err := h.Client.Get(ctx, client.ObjectKey{Name: spec.ActiveNode}, n); apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Sprintf("spec.activeNode: node %s not found", spec.ActiveNode))
		} else if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		} else if !sel.Matches(labels.Set(n.Labels)) {
			errs = append(errs, fmt.Sprintf("spec.activeNode: node %s does not match spec.egressNodeSelector", spec.ActiveNode))
		}
	}

	if old != nil && allowed != nil && !slices.Equal(old.Spec.EgressIPs, spec.EgressIPs) {
		policies := &ciliumv2.CiliumEgressGatewayPolicyList{}
		if err := h.Client.List(ctx, policies); err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		var inUse []string
		for i := range policies.Items {
			p := &policies.Items[i]
			if !h.ActiveLabel.IsManaged(p) || p.Spec.EgressGateway.EgressIP == "" {
				continue
			}
			if ip, err := iprange.ParseIPv4(p.Spec.EgressGateway.EgressIP); err == nil && !allowed.Contains(ip) {
				inUse = append(inUse, fmt.Sprintf("%s (%s)", p.Name, ip))
			}
		}
		if len(inUse) > 0 {
			slices.Sort(inUse)
			errs = append(errs, fmt.Sprintf("spec.egressIPs: egress IPs still used by policies: %s", strings.Join(inUse, ", ")))
		}
	}

	if len(errs) > 0 {
		return admission.Denied(strings.Join(errs, "; "))
	}
	return admission.Allowed("")
}
