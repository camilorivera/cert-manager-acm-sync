package main

import (
	"context"
	"flag"
	"os"

	"github.com/aws/aws-sdk-go-v2/config"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	acmclient "github.com/camilorivera/cert-manager-acm-sync/internal/acm"
	cloudfrontclient "github.com/camilorivera/cert-manager-acm-sync/internal/cloudfront"
	"github.com/camilorivera/cert-manager-acm-sync/internal/controller"
	_ "github.com/camilorivera/cert-manager-acm-sync/internal/metrics" // register Prometheus metrics
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
}

func main() {
	var (
		defaultRegion        string
		namespace            string
		leaderElect          bool
		metricsAddr          string
		healthProbeAddr      string
		leaderElectID        string
		enableCloudFrontSync bool
	)

	flag.StringVar(&defaultRegion, "default-region", "us-east-1",
		"Default AWS region for ACM imports when acm.sync/region is not set.")
	flag.StringVar(&namespace, "namespace", "",
		"Namespace to watch for TLS Secrets. Empty watches all namespaces (requires ClusterRole).")
	flag.BoolVar(&leaderElect, "leader-elect", true,
		"Enable leader election to prevent multiple active controllers.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"Address the Prometheus metrics endpoint binds to.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":8081",
		"Address the health probe endpoint binds to.")
	flag.StringVar(&leaderElectID, "leader-election-id", "cert-manager-acm-sync.acm.sync",
		"Name of the Lease resource used for leader election.")
	flag.BoolVar(&enableCloudFrontSync, "enable-cloudfront-sync", false,
		"Enable automatic CloudFront distribution alias sync after ACM imports. "+
			"Requires cloudfront:GetDistributionConfig and cloudfront:UpdateDistribution IAM permissions.")

	opts := zap.Options{
		Development: false,
		TimeEncoder: zapcore.RFC3339TimeEncoder,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	// config.LoadDefaultConfig reads IRSA credentials on EKS automatically
	// (AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN injected by EKS).
	awsCfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(defaultRegion),
	)
	if err != nil {
		setupLog.Error(err, "unable to load AWS SDK config")
		os.Exit(1)
	}

	mgrOpts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: healthProbeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       leaderElectID,
	}
	if namespace != "" {
		mgrOpts.Cache = cache.Options{
			DefaultNamespaces: map[string]cache.Config{namespace: {}},
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	var cfClient cloudfrontclient.CloudFrontAPI
	if enableCloudFrontSync {
		cfClient = cloudfrontclient.NewClient(awsCfg)
		setupLog.Info("CloudFront sync enabled")
	}

	if err := (&controller.SecretReconciler{
		Client:           mgr.GetClient(),
		Recorder:         mgr.GetEventRecorder("cert-manager-acm-sync"),
		ACMPool:          acmclient.NewPool(awsCfg),
		DefaultRegion:    defaultRegion,
		CloudFrontClient: cfClient,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up readiness check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "defaultRegion", defaultRegion, "namespace", func() string {
		if namespace == "" {
			return "<all>"
		}
		return namespace
	}())
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
