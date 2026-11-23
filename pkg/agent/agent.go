// Package agent configures egress IPs on the local node. It only executes the
// decision in EgressGatewayHA.spec.activeNode and never decides by itself.
package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/iprange"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/k8s"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/netops"
)

const (
	FieldManager = "egressgateway-ha-agent"

	// SyncedAnnotation is patched on the local CiliumNode after egress IPs are
	// added, so that Cilium looks up the egress IPs again.
	SyncedAnnotation = "egressgateway-ha.spidernet.io/egress-ip-synced"
)

// NetOps is the subset of netops.Manager used by the agent.
type NetOps interface {
	List(iface string) ([]netip.Addr, error)
	ListAll() (map[string][]netip.Addr, error)
	Add(iface string, ip netip.Addr) error
	Del(iface string, ip netip.Addr) error
	Detect(ctx context.Context, iface string, ip netip.Addr) error
	Announce(iface string, ip netip.Addr) error
}

// StatusWriter writes the fields of EgressGatewayHA status owned by the agent.
type StatusWriter interface {
	Apply(ctx context.Context, active *v1alpha1.ActiveNodeStatus, cond metav1.Condition) error
}

type Config struct {
	NodeName          string
	ReconcileInterval time.Duration
	// GARPInterval re-announces configured egress IPs periodically; 0 disables it.
	GARPInterval time.Duration
}

type Reconciler struct {
	Client client.Client
	Ops    NetOps
	Status StatusWriter
	Config Config
	Now    func() time.Time

	synced   atomic.Bool
	lastGARP time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func singletonRequest() reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: v1alpha1.SingletonName}}
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{singletonRequest()}
	})
	// Reconcile once at start even if no object exists yet, so readiness is reported.
	start := make(chan event.GenericEvent, 1)
	start <- event.GenericEvent{Object: &v1alpha1.EgressGatewayHA{}}
	return ctrl.NewControllerManagedBy(mgr).
		Named("agent").
		Watches(&v1alpha1.EgressGatewayHA{}, enqueue, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&ciliumv2.CiliumEgressGatewayPolicy{}, enqueue, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(source.Channel(start, enqueue)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// ReadyCheck reports ready after the first successful read from the API server.
func (r *Reconciler) ReadyCheck(*http.Request) error {
	if !r.synced.Load() {
		return errors.New("not synced with the API server yet")
	}
	return nil
}

func (r *Reconciler) requeue() ctrl.Result {
	d := r.Config.ReconcileInterval
	if g := r.Config.GARPInterval; g > 0 && (d <= 0 || g < d) {
		d = g
	}
	return ctrl.Result{RequeueAfter: d}
}

// result returns the periodic requeue on success. On errors controller-runtime
// retries with backoff, and a non-zero result must not be returned together.
func (r *Reconciler) result(err error) (ctrl.Result, error) {
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.requeue(), nil
}

// Reconcile converges the local egress IPs to the desired state. Any error
// reading from the API server leaves the interfaces untouched.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	egw := &v1alpha1.EgressGatewayHA{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, egw); err != nil {
		if apierrors.IsNotFound(err) {
			// Without the object there is no decision to execute; keep the current state.
			r.synced.Store(true)
			log.Info("EgressGatewayHA not found, keeping egress IPs unchanged")
			return r.requeue(), nil
		}
		return ctrl.Result{}, err
	}
	policies := &ciliumv2.CiliumEgressGatewayPolicyList{}
	if err := r.Client.List(ctx, policies); err != nil {
		return ctrl.Result{}, err
	}
	r.synced.Store(true)

	if egw.Spec.ActiveNode != r.Config.NodeName {
		return r.result(r.releaseAll(ctx))
	}

	allowed, err := iprange.Parse(egw.Spec.EgressIPs)
	if err != nil {
		log.Error(err, "invalid spec.egressIPs, keeping egress IPs unchanged")
		return r.requeue(), nil
	}
	return r.result(r.configure(ctx, egw, desiredIPs(ctx, policies.Items, allowed)))
}

// desiredIPs returns the sorted, deduplicated egress IPs of managed policies
// that are allowed by spec.egressIPs.
func desiredIPs(ctx context.Context, policies []ciliumv2.CiliumEgressGatewayPolicy, allowed iprange.List) []netip.Addr {
	var out []netip.Addr
	for i := range policies {
		p := &policies[i]
		if !k8s.IsManagedPolicy(p) || p.Spec.EgressGateway.EgressIP == "" {
			continue
		}
		ip, err := iprange.ParseIPv4(p.Spec.EgressGateway.EgressIP)
		if err != nil || !allowed.Contains(ip) {
			logf.FromContext(ctx).Info("ignoring egress IP not allowed by spec.egressIPs",
				"policy", p.Name, "egressIP", p.Spec.EgressGateway.EgressIP)
			continue
		}
		out = append(out, ip)
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return slices.Compact(out)
}

// releaseAll removes every managed egress IP from every interface.
func (r *Reconciler) releaseAll(ctx context.Context) error {
	all, err := r.Ops.ListAll()
	if err != nil {
		return err
	}
	var errs []error
	for iface, ips := range all {
		for _, ip := range ips {
			if err := r.Ops.Del(iface, ip); err != nil {
				errs = append(errs, err)
				continue
			}
			logf.FromContext(ctx).Info("removed egress IP", "interface", iface, "ip", ip)
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) configure(ctx context.Context, egw *v1alpha1.EgressGatewayHA, desired []netip.Addr) error {
	log := logf.FromContext(ctx)
	iface := egw.Spec.Interface

	// Remove managed IPs that are on another interface or no longer used.
	all, err := r.Ops.ListAll()
	if err != nil {
		return err
	}
	var errs []error
	for name, ips := range all {
		for _, ip := range ips {
			if name == iface && slices.Contains(desired, ip) {
				continue
			}
			if err := r.Ops.Del(name, ip); err != nil {
				errs = append(errs, err)
				continue
			}
			log.Info("removed egress IP", "interface", name, "ip", ip)
		}
	}

	current, err := r.Ops.List(iface)
	if errors.Is(err, netops.ErrInterfaceNotFound) {
		cond := r.condition(egw, metav1.ConditionFalse, v1alpha1.ReasonInterfaceNotFound,
			fmt.Sprintf("interface %s not found on %s", iface, r.Config.NodeName))
		return errors.Join(append(errs, r.writeStatus(ctx, egw, nil, false, cond))...)
	}
	if err != nil {
		return errors.Join(append(errs, err)...)
	}

	var configured []netip.Addr
	var conflicts, failures []string
	added := false
	for _, ip := range desired {
		if slices.Contains(current, ip) {
			configured = append(configured, ip)
			continue
		}
		if err := r.Ops.Detect(ctx, iface, ip); err != nil {
			if c, ok := errors.AsType[*netops.Conflict](err); ok {
				conflicts = append(conflicts, c.Error()+", not added")
			} else {
				failures = append(failures, fmt.Sprintf("%s: %v", ip, err))
			}
			continue
		}
		if err := r.Ops.Add(iface, ip); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", ip, err))
			continue
		}
		log.Info("added egress IP", "interface", iface, "ip", ip)
		if err := r.Ops.Announce(iface, ip); err != nil {
			log.Error(err, "failed to send gratuitous ARP", "ip", ip)
		}
		configured = append(configured, ip)
		added = true
	}

	now := r.now()
	if added {
		r.lastGARP = now
		if err := r.patchCiliumNode(ctx, now); err != nil {
			errs = append(errs, err)
		}
	} else if r.Config.GARPInterval > 0 && now.Sub(r.lastGARP) >= r.Config.GARPInterval {
		r.lastGARP = now
		for _, ip := range configured {
			if err := r.Ops.Announce(iface, ip); err != nil {
				log.Error(err, "failed to send gratuitous ARP", "ip", ip)
			}
		}
	}

	var cond metav1.Condition
	switch {
	case len(conflicts) > 0:
		cond = r.condition(egw, metav1.ConditionFalse, v1alpha1.ReasonEgressIPConflict, strings.Join(conflicts, "; "))
	case len(failures) > 0:
		cond = r.condition(egw, metav1.ConditionFalse, v1alpha1.ReasonConfigureFailed, strings.Join(failures, "; "))
	default:
		cond = r.condition(egw, metav1.ConditionTrue, v1alpha1.ReasonEgressIPsConfigured,
			fmt.Sprintf("%d egress IP(s) configured on %s of %s", len(configured), iface, r.Config.NodeName))
	}
	ready := len(conflicts) == 0 && len(failures) == 0
	errs = append(errs, r.writeStatus(ctx, egw, configured, ready, cond))
	return errors.Join(errs...)
}

func (r *Reconciler) condition(egw *v1alpha1.EgressGatewayHA, status metav1.ConditionStatus, reason, msg string) metav1.Condition {
	c := metav1.Condition{
		Type:               v1alpha1.ConditionGatewayReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: egw.Generation,
		LastTransitionTime: metav1.NewTime(r.now()),
	}
	if old := meta.FindStatusCondition(egw.Status.Conditions, c.Type); old != nil && old.Status == status {
		c.LastTransitionTime = old.LastTransitionTime
	}
	return c
}

// writeStatus applies status.activeNode and the GatewayReady condition when they changed.
func (r *Reconciler) writeStatus(ctx context.Context, egw *v1alpha1.EgressGatewayHA, ips []netip.Addr, ready bool, cond metav1.Condition) error {
	active := &v1alpha1.ActiveNodeStatus{Name: r.Config.NodeName, Ready: ready}
	for _, ip := range ips {
		active.EgressIPs = append(active.EgressIPs, ip.String())
	}
	old := meta.FindStatusCondition(egw.Status.Conditions, cond.Type)
	if old != nil && old.Status == cond.Status && old.Reason == cond.Reason && old.Message == cond.Message &&
		old.ObservedGeneration == cond.ObservedGeneration && activeEqual(egw.Status.ActiveNode, active) {
		return nil
	}
	return r.Status.Apply(ctx, active, cond)
}

func activeEqual(a, b *v1alpha1.ActiveNodeStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Name == b.Name && a.Ready == b.Ready && slices.Equal(a.EgressIPs, b.EgressIPs)
}

func (r *Reconciler) patchCiliumNode(ctx context.Context, now time.Time) error {
	cn := &ciliumv2.CiliumNode{ObjectMeta: metav1.ObjectMeta{Name: r.Config.NodeName}}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, SyncedAnnotation, now.UTC().Format(time.RFC3339Nano))
	if err := r.Client.Patch(ctx, cn, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		return fmt.Errorf("patch CiliumNode %s: %w", r.Config.NodeName, err)
	}
	return nil
}

// CleanupOnExit removes the egress IPs on shutdown only when the node is
// neither the active node nor a candidate anymore. Otherwise, and on any read
// error, the egress IPs are kept so that agent restarts do not affect traffic.
func (r *Reconciler) CleanupOnExit(ctx context.Context, reader client.Reader) error {
	egw := &v1alpha1.EgressGatewayHA{}
	if err := reader.Get(ctx, client.ObjectKey{Name: v1alpha1.SingletonName}, egw); err != nil {
		return client.IgnoreNotFound(err)
	}
	if egw.Spec.ActiveNode == r.Config.NodeName {
		return nil
	}
	node := &corev1.Node{}
	if err := reader.Get(ctx, client.ObjectKey{Name: r.Config.NodeName}, node); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	sel, err := metav1.LabelSelectorAsSelector(&egw.Spec.CandidateNodeSelector)
	if err != nil {
		return err
	}
	if node.Name != "" && sel.Matches(labels.Set(node.Labels)) {
		return nil
	}
	return r.releaseAll(ctx)
}

// ClientStatusWriter applies status with Server-Side Apply using FieldManager.
type ClientStatusWriter struct {
	Client client.Client
}

func (w *ClientStatusWriter) Apply(ctx context.Context, active *v1alpha1.ActiveNodeStatus, cond metav1.Condition) error {
	ips := make([]any, 0, len(active.EgressIPs))
	for _, ip := range active.EgressIPs {
		ips = append(ips, ip)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "EgressGatewayHA",
		"metadata":   map[string]any{"name": v1alpha1.SingletonName},
		"status": map[string]any{
			"activeNode": map[string]any{"name": active.Name, "egressIPs": ips, "ready": active.Ready},
			"conditions": []any{map[string]any{
				"type":               cond.Type,
				"status":             string(cond.Status),
				"reason":             cond.Reason,
				"message":            cond.Message,
				"observedGeneration": cond.ObservedGeneration,
				"lastTransitionTime": cond.LastTransitionTime.UTC().Format(time.RFC3339),
			}},
		},
	}}
	return w.Client.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(FieldManager), client.ForceOwnership)
}
