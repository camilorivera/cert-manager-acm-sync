package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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
	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
	"github.com/camilorivera/cert-manager-acm-sync/internal/dnsimple"
	_ "github.com/camilorivera/cert-manager-acm-sync/internal/metrics" // register Prometheus metrics
	route53client "github.com/camilorivera/cert-manager-acm-sync/internal/route53"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
}

type dnsFlags struct {
	provider       string
	zones          string
	ownerID        string
	defaultTarget  string
	defaultTTL     int64
	defaultType    string
	wildcardPolicy string
	deletePolicy   string
	dnsimpleAcct   string
	dnsimpleURL    string
}

func buildDNS(f dnsFlags, awsCfg aws.Config) (*dns.Syncer, controller.DNSDefaults, error) {
	if f.provider == "" {
		return nil, controller.DNSDefaults{}, nil
	}

	var entries []string
	for _, z := range strings.Split(f.zones, ",") {
		if z = strings.TrimSpace(z); z != "" {
			entries = append(entries, z)
		}
	}
	zones, err := dns.ParseZones(entries)
	if err != nil {
		return nil, controller.DNSDefaults{}, err
	}
	if len(zones) == 0 {
		return nil, controller.DNSDefaults{}, errors.New("--dns-zones is required when --dns-provider is set")
	}
	if f.ownerID == "" {
		return nil, controller.DNSDefaults{}, errors.New("--dns-owner-id must not be empty")
	}
	if f.defaultTTL <= 0 {
		return nil, controller.DNSDefaults{}, errors.New("--dns-default-ttl must be positive")
	}
	recType := dns.RecordType(strings.ToUpper(f.defaultType))
	if recType != dns.TypeCNAME && recType != dns.TypeALIAS {
		return nil, controller.DNSDefaults{}, fmt.Errorf("--dns-default-record-type must be CNAME or ALIAS, got %q", f.defaultType)
	}
	if f.wildcardPolicy != dns.WildcardSkip && f.wildcardPolicy != dns.WildcardCreate {
		return nil, controller.DNSDefaults{}, fmt.Errorf("--dns-wildcard-policy must be %s or %s, got %q", dns.WildcardSkip, dns.WildcardCreate, f.wildcardPolicy)
	}
	if f.deletePolicy != dns.DeleteRetain && f.deletePolicy != dns.DeleteDelete {
		return nil, controller.DNSDefaults{}, fmt.Errorf("--dns-delete-policy must be %s or %s, got %q", dns.DeleteRetain, dns.DeleteDelete, f.deletePolicy)
	}

	var provider dns.Provider
	switch f.provider {
	case "route53":
		for _, z := range zones {
			if z.ID == "" {
				return nil, controller.DNSDefaults{}, fmt.Errorf("--dns-zones entry %q needs a hosted zone ID (name=ID) for route53", z.Name)
			}
		}
		provider = route53client.NewProvider(route53client.NewClient(awsCfg))
	case "dnsimple":
		token := os.Getenv("DNSIMPLE_TOKEN")
		if token == "" {
			return nil, controller.DNSDefaults{}, errors.New("DNSIMPLE_TOKEN is required for the dnsimple provider")
		}
		if f.dnsimpleAcct == "" {
			return nil, controller.DNSDefaults{}, errors.New("--dnsimple-account-id is required for the dnsimple provider")
		}
		api := dnsimple.NewClient(f.dnsimpleURL, f.dnsimpleAcct, token, &http.Client{Timeout: 30 * time.Second})
		provider = dnsimple.NewProvider(api)
	default:
		return nil, controller.DNSDefaults{}, fmt.Errorf("--dns-provider must be route53 or dnsimple, got %q", f.provider)
	}

	syncer := &dns.Syncer{
		Provider:       provider,
		Zones:          zones,
		OwnerID:        f.ownerID,
		WildcardPolicy: f.wildcardPolicy,
		DeletePolicy:   f.deletePolicy,
	}
	defaults := controller.DNSDefaults{Target: f.defaultTarget, TTL: f.defaultTTL, RecordType: recType}
	return syncer, defaults, nil
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
		dnsCfg               dnsFlags
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
	flag.StringVar(&dnsCfg.provider, "dns-provider", "",
		"DNS provider for record sync: route53 or dnsimple. Empty disables DNS sync. The dnsimple token is read from DNSIMPLE_TOKEN.")
	flag.StringVar(&dnsCfg.zones, "dns-zones", "",
		"Comma-separated managed zones as name[=id]. Required when --dns-provider is set; id is required for route53.")
	flag.StringVar(&dnsCfg.ownerID, "dns-owner-id", "default",
		"Owner ID written into the ownership TXT records; must be unique per controller deployment.")
	flag.StringVar(&dnsCfg.defaultTarget, "dns-default-target", "",
		"Default record target when acm.sync/dns-target is not set. Falls back to the CloudFront distribution domain.")
	flag.Int64Var(&dnsCfg.defaultTTL, "dns-default-ttl", 300,
		"Default record TTL in seconds when acm.sync/dns-ttl is not set.")
	flag.StringVar(&dnsCfg.defaultType, "dns-default-record-type", "CNAME",
		"Default record type when acm.sync/dns-record-type is not set: CNAME or ALIAS.")
	flag.StringVar(&dnsCfg.wildcardPolicy, "dns-wildcard-policy", dns.WildcardSkip,
		"Wildcard SAN handling: skip or create.")
	flag.StringVar(&dnsCfg.deletePolicy, "dns-delete-policy", dns.DeleteRetain,
		"Record removal handling: retain or delete. delete prunes dropped names and cleans up when the Certificate is deleted.")
	flag.StringVar(&dnsCfg.dnsimpleAcct, "dnsimple-account-id", "",
		"DNSimple account ID. Required for the dnsimple provider.")
	flag.StringVar(&dnsCfg.dnsimpleURL, "dnsimple-base-url", "https://api.dnsimple.com",
		"DNSimple API base URL.")

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

	dnsSyncer, dnsDefaults, err := buildDNS(dnsCfg, awsCfg)
	if err != nil {
		setupLog.Error(err, "invalid DNS configuration")
		os.Exit(1)
	}
	if dnsSyncer != nil {
		setupLog.Info("DNS sync enabled", "provider", dnsCfg.provider, "ownerID", dnsCfg.ownerID)
	}

	if err := (&controller.SecretReconciler{
		Client:           mgr.GetClient(),
		Recorder:         mgr.GetEventRecorder("cert-manager-acm-sync"),
		ACMPool:          acmclient.NewPool(awsCfg),
		DefaultRegion:    defaultRegion,
		CloudFrontClient: cfClient,
		DNS:              dnsSyncer,
		DNSDefaults:      dnsDefaults,
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
