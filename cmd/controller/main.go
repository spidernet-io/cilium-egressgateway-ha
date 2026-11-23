package main

import (
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/spidernet-io/cilium-egressgateway-ha/api/v1alpha1"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/controller"
	"github.com/spidernet-io/cilium-egressgateway-ha/pkg/k8s"
)

func main() {
	var (
		cfg                            controller.Config
		ciliumSelector, agentSelector  string
		defaultSpecPath                string
		probeAddr, metricsAddr         string
		leaderElect                    bool
		leaderElectionNS, leaderElecID string
	)
	podNS := os.Getenv("POD_NAMESPACE")
	flag.StringVar(&cfg.CiliumNamespace, "cilium-namespace", "kube-system", "namespace of the cilium-agent pods")
	flag.StringVar(&ciliumSelector, "cilium-agent-selector", "k8s-app=cilium", "label selector of the cilium-agent pods")
	flag.StringVar(&cfg.AgentNamespace, "agent-namespace", podNS, "namespace of the agent pods")
	flag.StringVar(&agentSelector, "agent-selector", "app.kubernetes.io/component=agent", "label selector of the agent pods")
	flag.DurationVar(&cfg.ResyncInterval, "resync-interval", 10*time.Second, "interval of periodic reconcile")
	flag.StringVar(&defaultSpecPath, "default-spec", "", "file with the EgressGatewayHA spec used to create it when missing")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address of the health probe endpoint")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address of the metrics endpoint, 0 to disable")
	flag.BoolVar(&leaderElect, "leader-elect", true, "elect one working replica with a Lease")
	flag.StringVar(&leaderElectionNS, "leader-election-namespace", podNS, "namespace of the leader election Lease")
	flag.StringVar(&leaderElecID, "leader-election-id", "egressgateway-ha-controller", "name of the leader election Lease")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("controller")

	fail := func(err error, msg string) {
		log.Error(err, msg)
		os.Exit(1)
	}
	var err error
	if cfg.CiliumSelector, err = labels.Parse(ciliumSelector); err != nil {
		fail(err, "invalid --cilium-agent-selector")
	}
	if cfg.AgentSelector, err = labels.Parse(agentSelector); err != nil {
		fail(err, "invalid --agent-selector")
	}
	if cfg.AgentNamespace == "" {
		fail(nil, "--agent-namespace or POD_NAMESPACE is required")
	}
	if defaultSpecPath != "" {
		if cfg.Default, err = controller.LoadDefaultSpec(defaultSpecPath); err != nil {
			fail(err, "unable to load the default spec")
		}
	}

	// Only cache the cilium-agent and agent pods.
	podNamespaces := map[string]cache.Config{cfg.CiliumNamespace: {LabelSelector: cfg.CiliumSelector}}
	if cfg.AgentNamespace == cfg.CiliumNamespace {
		podNamespaces[cfg.AgentNamespace] = cache.Config{}
	} else {
		podNamespaces[cfg.AgentNamespace] = cache.Config{LabelSelector: cfg.AgentSelector}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  k8s.NewScheme(),
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        leaderElecID,
		LeaderElectionNamespace: leaderElectionNS,
		// Release the Lease on shutdown so that the other replica takes over quickly.
		LeaderElectionReleaseOnCancel: true,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&v1alpha1.EgressGatewayHA{}: {Field: fields.OneTermEqualSelector("metadata.name", v1alpha1.SingletonName)},
			&corev1.Pod{}:               {Namespaces: podNamespaces},
		}},
	})
	if err != nil {
		fail(err, "unable to create manager")
	}

	r := &controller.Reconciler{
		Client:   mgr.GetClient(),
		Reader:   mgr.GetAPIReader(),
		Status:   &controller.ClientStatusWriter{Client: mgr.GetClient()},
		Recorder: mgr.GetEventRecorder("egressgateway-ha-controller"),
		Config:   cfg,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		fail(err, "unable to set up controller")
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		fail(err, "unable to add health check")
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		fail(err, "unable to add ready check")
	}

	log.Info("starting controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fail(err, "manager exited")
	}
}
