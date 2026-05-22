// Command manager is the operator binary for the Parti Kubernetes controller.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/arloliu/parti/v2/k8s/api/v1alpha1"
	"github.com/arloliu/parti/v2/k8s/internal/controller"
)

// leaderElectionLockName is the fixed leader-election lock resource name.
// It is scoped to the operator's CRD group so it cannot collide with other
// operators sharing the same cluster namespace.
const leaderElectionLockName = "provisionedpartienv.parti.io"

var setupLog = ctrl.Log.WithName("setup")

// newScheme builds a *runtime.Scheme that includes the core Kubernetes types
// (via clientgoscheme) and the operator's own v1alpha1 types. It is extracted
// from main so tests can assert scheme registration without running the manager.
func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	return scheme, nil
}

func main() {
	// --- flags ---------------------------------------------------------------
	var (
		metricsAddr     string
		healthProbeAddr string
		leaderElect     bool
		resyncPeriod    time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metrics endpoint binds to. Set to '0' to disable.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":8081",
		"The address the health-probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", true,
		"Enable leader election for the operator. Disabling is only safe for single-replica deployments.")
	flag.DurationVar(&resyncPeriod, "resync-period", 5*time.Minute,
		"How often a converged ProvisionedPartiEnv is re-reconciled to detect drift.")

	// zap flags (-zap-log-level, -zap-encoder, etc.)
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	// --- scheme --------------------------------------------------------------
	scheme, err := newScheme()
	if err != nil {
		setupLog.Error(err, "unable to set up scheme")
		os.Exit(1)
	}

	// --- manager -------------------------------------------------------------
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress:        healthProbeAddr,
		LeaderElection:                leaderElect,
		LeaderElectionID:              leaderElectionLockName,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// --- reconciler ----------------------------------------------------------
	if err = (&controller.ProvisionedPartiEnvReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		ResyncPeriod: resyncPeriod,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ProvisionedPartiEnv")
		os.Exit(1)
	}

	// --- health checks -------------------------------------------------------
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// --- start ---------------------------------------------------------------
	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
