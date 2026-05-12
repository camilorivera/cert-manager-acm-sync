package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	SyncTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "acm_sync_total",
		Help: "Total number of ACM sync operations.",
	}, []string{"region", "action"})

	SyncErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "acm_sync_errors_total",
		Help: "Total number of ACM sync errors.",
	}, []string{"region", "action"})

	LastSyncTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "acm_sync_last_sync_timestamp",
		Help: "Unix timestamp of the last successful sync per secret.",
	}, []string{"region", "secret"})

	CloudFrontSyncTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cloudfront_sync_total",
		Help: "Total number of CloudFront distribution sync operations.",
	}, []string{"action"})

	CloudFrontSyncErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cloudfront_sync_errors_total",
		Help: "Total number of CloudFront distribution sync errors.",
	}, []string{"error_type"})
)

func init() {
	metrics.Registry.MustRegister(
		SyncTotal, SyncErrorsTotal, LastSyncTimestamp,
		CloudFrontSyncTotal, CloudFrontSyncErrorsTotal,
	)
}
