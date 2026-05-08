package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsacm "github.com/aws/aws-sdk-go-v2/service/acm"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	acmclient "github.com/camilorivera/cert-manager-acm-sync/internal/acm"
	"github.com/camilorivera/cert-manager-acm-sync/internal/annotations"
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
	Recorder      record.EventRecorder
	ACMPool       ACMPool
	DefaultRegion string
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

	// Belt-and-suspenders: predicate should already filter these out
	if secret.Type != corev1.SecretTypeTLS || !annotations.IsEnabled(secret.Annotations) {
		return ctrl.Result{}, nil
	}

	certPEM := secret.Data["tls.crt"]
	keyPEM := secret.Data["tls.key"]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		r.recorder().Event(&secret, corev1.EventTypeWarning, "MissingData",
			"tls.crt or tls.key is absent; will retry")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	currentFP, err := fingerprint.Compute(certPEM)
	if err != nil {
		r.recorder().Event(&secret, corev1.EventTypeWarning, "FingerprintError", err.Error())
		return ctrl.Result{}, err
	}

	region := r.DefaultRegion
	if override := annotations.GetRegion(secret.Annotations); override != "" {
		region = override
	}

	acmClient := r.ACMPool.ClientForRegion(region)

	existingARN := annotations.GetARN(secret.Annotations)
	existingFP := annotations.GetFingerprint(secret.Annotations)

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
				r.recorder().Event(&secret, corev1.EventTypeWarning, "CertificateNotFound",
					fmt.Sprintf("ACM certificate %s not found; creating a new certificate", existingARN))
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
		r.recorder().Event(&secret, corev1.EventTypeWarning, "SyncFailed", err.Error())
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

	r.recorder().Event(&secret, corev1.EventTypeNormal, "Synced",
		fmt.Sprintf("Certificate synced to ACM (action=%s, arn=%s, region=%s)", action, newARN, region))
	metrics.SyncTotal.WithLabelValues(region, action).Inc()
	metrics.LastSyncTimestamp.WithLabelValues(region, req.NamespacedName.String()).SetToCurrentTime()

	logger.Info("sync complete", "action", action, "arn", newARN, "region", region)
	return ctrl.Result{RequeueAfter: periodicRequeueInterval}, nil
}

func (r *SecretReconciler) recorder() record.EventRecorder {
	return r.Recorder
}

func (r *SecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{},
			builder.WithPredicates(TLSAnnotatedPredicate{}),
		).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 5,
		}).
		Complete(r)
}
