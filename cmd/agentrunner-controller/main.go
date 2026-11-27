// Command agentrunner-controller runs the Kubernetes controller watching
// AgenticRunnerDeployment and AgenticRunnerToolchain CRDs and reconciling them
// into ARC v2 AutoscalingRunnerSets and supporting resources.
package main

import (
	"flag"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/wrossmorrow/github-agent-runner/pkg/controller"
	"github.com/wrossmorrow/github-agent-runner/pkg/render"
)

func main() {
	var (
		metricsAddr     string
		probeAddr       string
		leaderElect     bool
		arcVersion      string
		arcControllerNS string
		arcControllerSA string
		githubAuthImage string
		imagePullPolicy string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&arcVersion, "arc-version", "", "ARC controller version override (empty = auto-detect from arc-systems).")
	flag.StringVar(&arcControllerNS, "arc-controller-namespace", render.DefaultARCControllerNamespace, "Namespace of the ARC controller ServiceAccount.")
	flag.StringVar(&arcControllerSA, "arc-controller-sa", render.DefaultARCControllerSA, "ARC controller ServiceAccount name.")
	flag.StringVar(&githubAuthImage, "github-auth-image", render.DefaultGitHubAuthImage, "Image used for the github-auth sidecar.")
	flag.StringVar(&imagePullPolicy, "image-pull-policy", render.DefaultImagePullPolicy, "ImagePullPolicy applied to agentrunner sidecars.")

	zapOpts := zap.Options{Development: true}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "agentrunner-controller.agentic.example.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	reconciler := &controller.DeploymentReconciler{
		Client:               mgr.GetClient(),
		AutoDetectARCVersion: true,
		RenderOptions: render.Options{
			ARCVersion:                  arcVersion,
			ARCControllerNamespace:      arcControllerNS,
			ARCControllerServiceAccount: arcControllerSA,
			DefaultGitHubAuthImage:      githubAuthImage,
			ImagePullPolicy:             imagePullPolicy,
		},
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting agentrunner-controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
