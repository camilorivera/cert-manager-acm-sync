package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsacm "github.com/aws/aws-sdk-go-v2/service/acm"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	acmclient "github.com/camilorivera/cert-manager-acm-sync/internal/acm"
	"github.com/camilorivera/cert-manager-acm-sync/internal/annotations"
	cloudfrontclient "github.com/camilorivera/cert-manager-acm-sync/internal/cloudfront"
	"github.com/camilorivera/cert-manager-acm-sync/internal/fingerprint"
	"github.com/camilorivera/cert-manager-acm-sync/internal/metrics"
)

// ACMPool is satisfied by both acmclient.Pool and the test MockPool.
type ACMPool interface {
	ClientForRegion(region string) acmclient.ACMAPI
}

// SecretReconciler watches kubernetes.io/tls Secrets annotated for ACM sync
// and imports their certificate material into AWS ACM, preserving the same
// certificate ARN across renewals.
type SecretReconciler struct {
	client.Client
	// Reader is used for direct API-server reads (bypassing the cache) for
	// object types not registered in the manager scheme, e.g. cert-manager
	// Certificate resources. Set automatically by SetupWithManager; tests may
	// override it with a fake client.
	Reader        client.Reader
	Recorder      events.EventRecorder
	ACMPool       ACMPool
	DefaultRegion string
	// CloudFrontClient is optional. When nil (default), CloudFront sync is
	// skipped even if the annotation is present. Enable via --enable-cloudfront-sync.
	CloudFrontClient cloudfrontclient.CloudFrontAPI
}

var certGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

// findCertificateOwner returns the cert-manager Certificate that owns the
// Secret (via ownerReferences), or nil if none is found.
func (r *SecretReconciler) findCertificateOwner(ctx context.Context, secret *corev1.Secret) *unstructured.Unstructured {
	for _, ref := range secret.OwnerReferences {
		if ref.Kind != "Certificate" || !strings.HasPrefix(ref.APIVersion, "cert-manager.io/") {
			continue
		}
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certGVK)
		if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: secret.Namespace, Name: ref.Name}, cert); err != nil {
			return nil
		}
		return cert
	}
	return nil
}

// certificateToSecret maps a cert-manager Certificate event to a reconcile
// Request for the Secret it owns (via spec.secretName).
func (r *SecretReconciler) certificateToSecret(_ context.Context, obj client.Object) []reconcile.Request {
	cert, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	secretName, found, err := unstructured.NestedString(cert.Object, "spec", "secretName")
	if err != nil || !found || secretName == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: k8stypes.NamespacedName{
			Namespace: cert.GetNamespace(),
			Name:      secretName,
		},
	}}
}

const (
	periodicRequeueInterval = 6 * time.Hour
)

func (r *SecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("secret", req.NamespacedName)

	var secret corev1.Secret
	if err := r.Get(ctx, req.NamespacedName, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Look up the owning Certificate early — needed for both the enabled check
	// and region fallback when the annotation lives on the Certificate rather
	// than being propagated to the Secret via secretTemplate.
	certOwner := r.findCertificateOwner(ctx, &secret)

	// Belt-and-suspenders: the predicate passes all TLS Secrets; bail quickly
	// if neither the Secret nor its Certificate owner has opted in.
	enabled := annotations.IsEnabled(secret.Annotations) ||
		(certOwner != nil && annotations.IsEnabled(certOwner.GetAnnotations()))
	if secret.Type != corev1.SecretTypeTLS || !enabled {
		return ctrl.Result{}, nil
	}

	certPEM := secret.Data["tls.crt"]
	keyPEM := secret.Data["tls.key"]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		r.Recorder.Eventf(&secret, nil, corev1.EventTypeWarning, "MissingData",
			"Reconciling", "tls.crt or tls.key is absent; will retry")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	currentFP, err := fingerprint.Compute(certPEM)
	if err != nil {
		r.Recorder.Eventf(&secret, nil, corev1.EventTypeWarning, "FingerprintError", "Reconciling", err.Error())
		return ctrl.Result{}, err
	}

	region := r.DefaultRegion
	if override := annotations.GetRegion(secret.Annotations); override != "" {
		region = override
	} else if certOwner != nil {
		if override := annotations.GetRegion(certOwner.GetAnnotations()); override != "" {
			region = override
		}
	}

	acmClient := r.ACMPool.ClientForRegion(region)

	existingARN := annotations.GetARN(secret.Annotations)
	existingFP := annotations.GetFingerprint(secret.Annotations)

	// If the Secret was deleted and recreated by cert-manager, it loses the
	// ACM ARN annotation. Recover it from the owning Certificate resource so
	// we reimport to the same ARN instead of creating a new certificate.
	if existingARN == "" && certOwner != nil {
		if recovered := certOwner.GetAnnotations()[annotations.ARN]; recovered != "" {
			logger.Info("recovered ARN from Certificate owner", "arn", recovered)
			existingARN = recovered
		}
	}

	// If we have a stored ARN, verify the certificate still exists in ACM.
	// This handles the case where a cert was deleted from ACM externally.
	if existingARN != "" {
		_, err := acmClient.DescribeCertificate(ctx, &awsacm.DescribeCertificateInput{
			CertificateArn: aws.String(existingARN),
		})
		if err != nil {
			if acmclient.IsNotFound(err) {
				logger.Info("ACM certificate not found; will create a new one",
					"previousARN", existingARN)
				r.Recorder.Eventf(&secret, nil, corev1.EventTypeWarning, "CertificateNotFound",
					"Reconciling", "ACM certificate %s not found; creating a new certificate", existingARN)
				existingARN = "" // treat as first import
			} else {
				metrics.SyncErrorsTotal.WithLabelValues(region, "describe").Inc()
				return ctrl.Result{}, fmt.Errorf("DescribeCertificate: %w", err)
			}
		}
	}

	// After the existence check, decide the action.
	if existingARN != "" && existingFP == currentFP {
		logger.V(1).Info("fingerprint unchanged, skipping import", "arn", existingARN)
		metrics.SyncTotal.WithLabelValues(region, "skipped").Inc()
		// Best-effort: mirror the ARN onto the Certificate owner if it is
		// missing it (e.g. pre-existing certificates synced before this
		// feature was introduced, or after a controller upgrade).
		if certOwner != nil && certOwner.GetAnnotations()[annotations.ARN] == "" {
			certPatch := certOwner.DeepCopy()
			certAnns := certOwner.GetAnnotations()
			if certAnns == nil {
				certAnns = map[string]string{}
			}
			certAnns[annotations.ARN] = existingARN
			certOwner.SetAnnotations(certAnns)
			if err := r.Patch(ctx, certOwner, client.MergeFrom(certPatch)); err != nil {
				logger.Error(err, "failed to backfill ARN annotation onto Certificate", "arn", existingARN)
			}
		}
		return ctrl.Result{RequeueAfter: periodicRequeueInterval}, nil
	}

	action := "import"
	if existingARN != "" {
		action = "reimport"
	}

	// Prepare the import payload.
	// tls.crt may contain the full chain; ACM expects the leaf and chain separately.
	leafPEM, chainPEM := fingerprint.SplitChain(certPEM)
	// Merge ca.crt (if present) into the chain
	if caPEM := secret.Data["ca.crt"]; len(caPEM) > 0 {
		chainPEM = append(chainPEM, caPEM...)
	}

	input := &awsacm.ImportCertificateInput{
		Certificate: leafPEM,
		PrivateKey:  keyPEM, // never logged
	}
	if len(chainPEM) > 0 {
		input.CertificateChain = chainPEM
	}
	if existingARN != "" {
		input.CertificateArn = aws.String(existingARN)
	}

	result, err := acmclient.ImportWithRetry(ctx, acmClient, input)
	if err != nil {
		metrics.SyncErrorsTotal.WithLabelValues(region, action).Inc()
		r.Recorder.Eventf(&secret, nil, corev1.EventTypeWarning, "SyncFailed", "Importing", err.Error())
		return ctrl.Result{}, fmt.Errorf("ImportCertificate (%s): %w", action, err)
	}

	newARN := aws.ToString(result.CertificateArn)

	// Patch annotations back onto the Secret using a MergePatch so we only
	// touch the annotation keys we own and do not risk overwriting other changes.
	patchBase := secret.DeepCopy()
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[annotations.ARN] = newARN
	secret.Annotations[annotations.Fingerprint] = currentFP
	secret.Annotations[annotations.LastSync] = time.Now().UTC().Format(time.RFC3339)

	if err := r.Patch(ctx, &secret, client.MergeFrom(patchBase)); err != nil {
		// The import succeeded but we could not persist the ARN annotation.
		// The next reconcile will call DescribeCertificate and may re-import
		// (idempotent). Log but do not treat as a fatal error.
		logger.Error(err, "imported to ACM but failed to patch annotations; will retry",
			"arn", newARN)
		return ctrl.Result{}, err
	}

	// Mirror the ARN onto the owning Certificate so it survives Secret
	// deletion and can be recovered on the next reconcile.
	if certOwner != nil {
		certPatch := certOwner.DeepCopy()
		certAnns := certOwner.GetAnnotations()
		if certAnns == nil {
			certAnns = map[string]string{}
		}
		certAnns[annotations.ARN] = newARN
		certOwner.SetAnnotations(certAnns)
		if err := r.Patch(ctx, certOwner, client.MergeFrom(certPatch)); err != nil {
			logger.Error(err, "imported to ACM but failed to patch Certificate with ARN",
				"arn", newARN)
		}
	}

	r.Recorder.Eventf(&secret, nil, corev1.EventTypeNormal, "Synced", "Importing",
		"Certificate synced to ACM (action=%s, arn=%s, region=%s)", action, newARN, region)
	metrics.SyncTotal.WithLabelValues(region, action).Inc()
	metrics.LastSyncTimestamp.WithLabelValues(region, req.NamespacedName.String()).SetToCurrentTime()

	logger.Info("sync complete", "action", action, "arn", newARN, "region", region)

	r.maybeSyncCloudFront(ctx, &secret, certOwner, newARN, certPEM, logger)

	return ctrl.Result{RequeueAfter: periodicRequeueInterval}, nil
}

func (r *SecretReconciler) maybeSyncCloudFront(
	ctx context.Context,
	secret *corev1.Secret,
	certOwner *unstructured.Unstructured,
	acmCertARN string,
	certPEM []byte,
	logger logr.Logger,
) {
	if r.CloudFrontClient == nil {
		return
	}

	distARN := annotations.GetCloudFrontDistributionARN(secret.Annotations)
	if distARN == "" && certOwner != nil {
		distARN = annotations.GetCloudFrontDistributionARN(certOwner.GetAnnotations())
	}
	if distARN == "" {
		return
	}

	sans, err := fingerprint.ExtractSANs(certPEM)
	if err != nil {
		metrics.CloudFrontSyncErrorsTotal.WithLabelValues("extract_sans").Inc()
		logger.Error(err, "cloudfront sync: failed to extract SANs")
		r.Recorder.Eventf(secret, nil, corev1.EventTypeWarning, "CloudFrontSyncFailed",
			"SyncingCloudFront", "failed to extract SANs for CloudFront sync: %v", err)
		return
	}

	if err := cloudfrontclient.SyncDistribution(ctx, r.CloudFrontClient, distARN, acmCertARN, sans); err != nil {
		errType := "update"
		if strings.Contains(err.Error(), "GetDistributionConfig") {
			errType = "get_config"
		} else if strings.Contains(err.Error(), "extract distribution ID") {
			errType = "extract_id"
		}
		metrics.CloudFrontSyncErrorsTotal.WithLabelValues(errType).Inc()
		logger.Error(err, "cloudfront sync failed; ACM import succeeded",
			"distributionARN", distARN, "acmCertARN", acmCertARN)
		r.Recorder.Eventf(secret, nil, corev1.EventTypeWarning, "CloudFrontSyncFailed", "SyncingCloudFront", err.Error())
		return
	}

	metrics.CloudFrontSyncTotal.WithLabelValues("synced").Inc()
	logger.Info("cloudfront sync complete", "distributionARN", distARN, "acmCertARN", acmCertARN, "sans", sans)
	r.Recorder.Eventf(secret, nil, corev1.EventTypeNormal, "CloudFrontSynced",
		"SyncingCloudFront", "CloudFront distribution %s updated (acmCertARN=%s)", distARN, acmCertARN)
}

func (r *SecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Reader == nil {
		r.Reader = mgr.GetAPIReader()
	}

	b := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{},
			builder.WithPredicates(TLSAnnotatedPredicate{}),
		).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 5,
		})

	// Watch cert-manager Certificate resources so that adding acm.sync/enabled
	// directly to a Certificate (instead of secretTemplate.annotations)
	// triggers reconciliation of the owned Secret. Skipped gracefully if the
	// cert-manager CRD is not installed.
	if _, err := mgr.GetRESTMapper().RESTMapping(certGVK.GroupKind(), certGVK.Version); err == nil {
		certObj := &unstructured.Unstructured{}
		certObj.SetGroupVersionKind(certGVK)
		b = b.Watches(
			certObj,
			handler.EnqueueRequestsFromMapFunc(r.certificateToSecret),
			builder.WithPredicates(CertificateAnnotatedPredicate{}),
		)
	}

	return b.Complete(r)
}
