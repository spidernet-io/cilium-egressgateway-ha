package main

import (
	"context"
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/agent"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/k8s"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/metrics"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/netops"
)

func main() {
	cfg := agent.Config{}
	var probeAddr, metricsAddr string
	flag.StringVar(&cfg.NodeName, "node-name", os.Getenv("NODE_NAME"), "name of the local node")
	flag.DurationVar(&cfg.ReconcileInterval, "reconcile-interval", 10*time.Second, "interval of periodic reconcile")
	flag.DurationVar(&cfg.GARPInterval, "garp-interval", 0, "interval of periodic gratuitous ARP, 0 to only send it when an egress IP is added")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":9790", "address of the health probe endpoint")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":9791", "address of the metrics endpoint, 0 to disable")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("agent")

	if cfg.NodeName == "" {
		log.Error(nil, "--node-name or NODE_NAME is required")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 k8s.NewScheme(),
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&v1alpha1.EgressGatewayHA{}: {Field: fields.OneTermEqualSelector("metadata.name", v1alpha1.SingletonName)},
		}},
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	metrics.RegisterAgent()
	r := &agent.Reconciler{
		Client: mgr.GetClient(),
		Ops:    netops.New(),
		Status: &agent.ClientStatusWriter{Client: mgr.GetClient()},
		Config: cfg,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up agent")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to add health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("synced", r.ReadyCheck); err != nil {
		log.Error(err, "unable to add ready check")
		os.Exit(1)
	}

	log.Info("starting agent", "node", cfg.NodeName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
	}

	// Egress IPs are kept on exit so that upgrades do not affect traffic,
	// unless this node is no longer a candidate nor the active node.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.CleanupOnExit(ctx, mgr.GetAPIReader()); err != nil {
		log.Error(err, "cleanup on exit failed, egress IPs are kept")
	}
	log.Info("agent stopped")
}
