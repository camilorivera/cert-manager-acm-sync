package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/camilorivera/cert-manager-acm-sync/internal/annotations"
	cloudfrontclient "github.com/camilorivera/cert-manager-acm-sync/internal/cloudfront"
	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
	"github.com/camilorivera/cert-manager-acm-sync/internal/fingerprint"
	"github.com/camilorivera/cert-manager-acm-sync/internal/metrics"
)

const (
	dnsRetryInterval = 5 * time.Minute
	dnsDeferInterval = time.Minute
	dnsAction        = "SyncingDNS"
	defaultDNSTTL    = 300

	certManagerCertificateName = "cert-manager.io/certificate-name"
)

// DNSDefaults are the controller-wide fallbacks for the acm.sync/dns-* annotations.
type DNSDefaults struct {
	Target     string
	TTL        int64
	RecordType dns.RecordType
}

func dnsResource(namespace, name string) string {
	return fmt.Sprintf("secret/%s/%s", namespace, name)
}

// dnsAnnotation resolves key from the Secret first, then the owning Certificate.
func dnsAnnotation(secret *corev1.Secret, certOwner *unstructured.Unstructured, key string) string {
	if v, ok := secret.Annotations[key]; ok {
		return v
	}
	if certOwner != nil {
		return certOwner.GetAnnotations()[key]
	}
	return ""
}

func (r *SecretReconciler) dnsProviderName() string {
	if r.DNS == nil || r.DNS.Provider == nil {
		return ""
	}
	return r.DNS.Provider.Name()
}

func (r *SecretReconciler) dnsFinalizerHolder(secret *corev1.Secret, certOwner *unstructured.Unstructured) client.Object {
	if certOwner != nil {
		return certOwner
	}
	return secret
}

func setFinalizer(ctx context.Context, c client.Client, obj client.Object, want bool) error {
	base, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return nil
	}
	var changed bool
	if want {
		changed = controllerutil.AddFinalizer(obj, annotations.DNSCleanupFinalizer)
	} else {
		changed = controllerutil.RemoveFinalizer(obj, annotations.DNSCleanupFinalizer)
	}
	if !changed {
		return nil
	}
	err := c.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	return client.IgnoreNotFound(err)
}

func (r *SecretReconciler) emitDNSResults(obj client.Object, results []dns.NameResult) {
	provider := r.dnsProviderName()
	for _, res := range results {
		metrics.DNSSyncTotal.WithLabelValues(provider, res.Action).Inc()
		var reason, evType string
		switch res.Action {
		case dns.ActionCreated:
			reason, evType = "DNSRecordCreated", corev1.EventTypeNormal
		case dns.ActionUpdated:
			reason, evType = "DNSRecordUpdated", corev1.EventTypeNormal
		case dns.ActionDeleted:
			reason, evType = "DNSRecordDeleted", corev1.EventTypeNormal
		case dns.ActionAdopted:
			reason, evType = "DNSRecordAdopted", corev1.EventTypeNormal
		case dns.ActionSkippedWildcard, dns.ActionSkippedTarget, dns.ActionNoZone, dns.ActionUnmanagedOK:
			reason, evType = "DNSRecordSkipped", corev1.EventTypeNormal
		case dns.ActionConflict:
			reason, evType = "DNSRecordConflict", corev1.EventTypeWarning
		case dns.ActionReleased:
			reason, evType = "DNSRecordReleased", corev1.EventTypeWarning
		default:
			continue
		}
		r.Recorder.Eventf(obj, nil, evType, reason, dnsAction, "%s: %s", res.Name, res.Detail)
	}
}

func (r *SecretReconciler) reportDNSError(obj client.Object, err error, logger logr.Logger) bool {
	retry := errors.Is(err, dns.ErrRetryable)
	errType := "sync"
	if retry {
		errType = "retryable"
	}
	metrics.DNSSyncErrorsTotal.WithLabelValues(r.dnsProviderName(), errType).Inc()
	logger.Error(err, "dns sync failed")
	r.Recorder.Eventf(obj, nil, corev1.EventTypeWarning, "DNSSyncFailed", dnsAction, err.Error())
	return retry
}

func (r *SecretReconciler) dnsTarget(ctx context.Context, secret *corev1.Secret, certOwner *unstructured.Unstructured) (string, error) {
	if t := dnsAnnotation(secret, certOwner, annotations.DNSTarget); t != "" {
		return t, nil
	}
	if r.DNSDefaults.Target != "" {
		return r.DNSDefaults.Target, nil
	}
	if r.CloudFrontClient == nil {
		return "", nil
	}
	distARN := dnsAnnotation(secret, certOwner, annotations.CloudFrontDistributionARN)
	if distARN == "" {
		return "", nil
	}
	return cloudfrontclient.DistributionDomainName(ctx, r.CloudFrontClient, distARN)
}

func (r *SecretReconciler) dnsRecordType(secret *corev1.Secret, certOwner *unstructured.Unstructured) (dns.RecordType, error) {
	raw := strings.ToUpper(dnsAnnotation(secret, certOwner, annotations.DNSRecordType))
	if raw == "" {
		if r.DNSDefaults.RecordType != "" {
			return r.DNSDefaults.RecordType, nil
		}
		return dns.TypeCNAME, nil
	}
	switch t := dns.RecordType(raw); t {
	case dns.TypeCNAME, dns.TypeALIAS:
		return t, nil
	}
	return "", fmt.Errorf("unsupported %s %q", annotations.DNSRecordType, raw)
}

func (r *SecretReconciler) dnsTTL(secret *corev1.Secret, certOwner *unstructured.Unstructured) (int64, error) {
	raw := dnsAnnotation(secret, certOwner, annotations.DNSTTL)
	if raw == "" {
		if r.DNSDefaults.TTL > 0 {
			return r.DNSDefaults.TTL, nil
		}
		return defaultDNSTTL, nil
	}
	ttl, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ttl <= 0 {
		return 0, fmt.Errorf("invalid %s %q", annotations.DNSTTL, raw)
	}
	return ttl, nil
}

func normalizeDNSNames(names []string) []string {
	set := map[string]struct{}{}
	for _, n := range names {
		n = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), ".")
		if n != "" {
			set[n] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// acceptedDNSNameSets returns the SAN sets an issued certificate may carry:
// spec.dnsNames, or spec.dnsNames plus spec.commonName for issuers that add
// it. With no dnsNames the CommonName alone, mirroring fingerprint.ExtractSANs.
func acceptedDNSNameSets(cert *unstructured.Unstructured) [][]string {
	names, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	cn, _, _ := unstructured.NestedString(cert.Object, "spec", "commonName")
	if len(names) == 0 && cn != "" {
		names = []string{cn}
	}
	sets := [][]string{normalizeDNSNames(names)}
	if cn != "" {
		sets = append(sets, normalizeDNSNames(append(slices.Clone(names), cn)))
	}
	return sets
}

func certificateCondition(cert *unstructured.Unstructured, condType string) string {
	conds, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != condType {
			continue
		}
		status, _ := m["status"].(string)
		return status
	}
	return ""
}

// dnsDeferReason returns why DNS sync must wait for cert-manager to finish
// issuing, or "" when the Secret's certificate is current.
func dnsDeferReason(cert *unstructured.Unstructured, sans []string) string {
	switch {
	case certificateCondition(cert, "Issuing") == string(metav1.ConditionTrue):
		return "certificate is being issued"
	case certificateCondition(cert, "Ready") != string(metav1.ConditionTrue):
		return "certificate is not Ready"
	}
	got := normalizeDNSNames(sans)
	for _, want := range acceptedDNSNameSets(cert) {
		if slices.Equal(got, want) {
			return ""
		}
	}
	return "certificate SANs differ from spec.dnsNames"
}

// deferCertificate returns the Certificate whose issuance state gates DNS sync:
// the owner, else the one named by the cert-manager.io/certificate-name
// annotation when it targets this Secret. nil means no gating.
func (r *SecretReconciler) deferCertificate(ctx context.Context, secret *corev1.Secret, certOwner *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if certOwner != nil {
		return certOwner, nil
	}
	name := secret.Annotations[certManagerCertificateName]
	if name == "" || r.noCertificates {
		return nil, nil
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certGVK)
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: secret.Namespace, Name: name}, cert); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	if target, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName"); target != secret.Name {
		return nil, nil
	}
	return cert, nil
}

func (r *SecretReconciler) maybeSyncDNS(
	ctx context.Context,
	secret *corev1.Secret,
	certOwner *unstructured.Unstructured,
	certPEM []byte,
	logger logr.Logger,
) (time.Duration, error) {
	optedIn := dnsAnnotation(secret, certOwner, annotations.DNSEnabled) == "true"
	wantFinalizer := r.DNS != nil && r.DNS.DeletePolicy == dns.DeleteDelete && optedIn

	holder := r.dnsFinalizerHolder(secret, certOwner)
	holderDeleting := holder.GetDeletionTimestamp() != nil
	if !wantFinalizer || !holderDeleting {
		if err := setFinalizer(ctx, r.Client, holder, wantFinalizer); err != nil {
			if wantFinalizer {
				r.reportDNSError(secret, fmt.Errorf("set %s finalizer: %w", annotations.DNSCleanupFinalizer, err), logger)
				return dnsRetryInterval, nil
			}
			logger.Error(err, "failed to remove dns cleanup finalizer")
		}
	}
	if certOwner != nil && secret.DeletionTimestamp == nil {
		if err := setFinalizer(ctx, r.Client, secret, false); err != nil {
			logger.Error(err, "failed to remove stale dns cleanup finalizer from secret")
		}
	}

	if r.DNS == nil || !optedIn || holderDeleting {
		return 0, nil
	}

	skip := func(format string, args ...any) (time.Duration, error) {
		r.Recorder.Eventf(secret, nil, corev1.EventTypeNormal, "DNSRecordSkipped", dnsAction, format, args...)
		return 0, nil
	}

	sans, err := fingerprint.ExtractSANs(certPEM)
	if err != nil {
		metrics.DNSSyncErrorsTotal.WithLabelValues(r.dnsProviderName(), "extract_sans").Inc()
		r.Recorder.Eventf(secret, nil, corev1.EventTypeWarning, "DNSSyncFailed", dnsAction,
			"failed to extract SANs for DNS sync: %v", err)
		return 0, nil
	}

	issuing, err := r.deferCertificate(ctx, secret, certOwner)
	if err != nil {
		return 0, err
	}
	if issuing != nil {
		if reason := dnsDeferReason(issuing, sans); reason != "" {
			metrics.DNSSyncTotal.WithLabelValues(r.dnsProviderName(), "deferred").Inc()
			r.Recorder.Eventf(secret, nil, corev1.EventTypeNormal, "DNSSyncDeferred", dnsAction,
				"DNS sync deferred: %s; retrying in %s", reason, dnsDeferInterval)
			return dnsDeferInterval, nil
		}
	}

	target, err := r.dnsTarget(ctx, secret, certOwner)
	if err != nil {
		metrics.DNSSyncErrorsTotal.WithLabelValues(r.dnsProviderName(), "target").Inc()
		logger.Error(err, "dns target lookup failed")
		r.Recorder.Eventf(secret, nil, corev1.EventTypeWarning, "DNSSyncFailed", dnsAction, err.Error())
		return dnsRetryInterval, nil
	}
	if target == "" {
		return skip("no DNS target: set %s, --dns-default-target or a CloudFront distribution", annotations.DNSTarget)
	}
	recType, err := r.dnsRecordType(secret, certOwner)
	if err != nil {
		return skip("%v", err)
	}
	ttl, err := r.dnsTTL(secret, certOwner)
	if err != nil {
		return skip("%v", err)
	}

	previous := annotations.GetDNSRecords(secret.Annotations)
	if len(previous) == 0 && certOwner != nil {
		previous = annotations.GetDNSRecords(certOwner.GetAnnotations())
	}

	results, owned, syncErr := r.DNS.Sync(ctx, dns.Request{
		Resource: dnsResource(secret.Namespace, secret.Name),
		Names:    sans,
		Target:   target,
		Type:     recType,
		TTL:      ttl,
		Previous: previous,
		Adopt:    dnsAnnotation(secret, certOwner, annotations.DNSAdopt) == "true",
	})
	r.emitDNSResults(secret, results)

	retry := time.Duration(0)
	if syncErr != nil {
		r.reportDNSError(secret, syncErr, logger)
		retry = dnsRetryInterval
	}
	if err := r.writeDNSRecords(ctx, secret, certOwner, strings.Join(owned, ",")); err != nil {
		logger.Error(err, "failed to persist dns-records annotation")
		return dnsRetryInterval, nil
	}
	return retry, nil
}

func (r *SecretReconciler) writeDNSRecords(ctx context.Context, secret *corev1.Secret, certOwner *unstructured.Unstructured, value string) error {
	if secret.Annotations[annotations.DNSRecords] != value {
		base := secret.DeepCopy()
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		if value == "" {
			delete(secret.Annotations, annotations.DNSRecords)
		} else {
			secret.Annotations[annotations.DNSRecords] = value
		}
		if err := r.Patch(ctx, secret, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	if certOwner != nil && certOwner.GetAnnotations()[annotations.DNSRecords] != value {
		base := certOwner.DeepCopy()
		anns := certOwner.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		if value == "" {
			delete(anns, annotations.DNSRecords)
		} else {
			anns[annotations.DNSRecords] = value
		}
		certOwner.SetAnnotations(anns)
		if err := r.Patch(ctx, certOwner, client.MergeFrom(base)); err != nil {
			return err
		}
	}
	return nil
}

func hasDNSFinalizer(obj client.Object) bool {
	return controllerutil.ContainsFinalizer(obj, annotations.DNSCleanupFinalizer)
}

// finalizeDNS runs DNS cleanup for a terminating Certificate or Secret that
// still holds the cleanup finalizer. handled is true when the reconcile must stop.
func (r *SecretReconciler) finalizeDNS(ctx context.Context, secret *corev1.Secret, certOwner *unstructured.Unstructured) (res ctrl.Result, handled bool, err error) {
	var holder client.Object
	switch {
	case certOwner != nil && certOwner.GetDeletionTimestamp() != nil && hasDNSFinalizer(certOwner):
		holder = certOwner
	case secret.DeletionTimestamp != nil && hasDNSFinalizer(secret):
		live, err := r.hasLiveCertificate(ctx, secret, certOwner)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if live {
			return ctrl.Result{}, true, setFinalizer(ctx, r.Client, secret, false)
		}
		holder = secret
	default:
		cert, err := r.terminatingCertificate(ctx, secret, certOwner)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if cert == nil {
			return ctrl.Result{}, false, nil
		}
		holder = cert
	}
	names := annotations.GetDNSRecords(holder.GetAnnotations())
	if len(names) == 0 {
		names = annotations.GetDNSRecords(secret.Annotations)
	}
	res, err = r.cleanupDNS(ctx, holder, dnsResource(secret.Namespace, secret.Name), names)
	return res, true, err
}

func (r *SecretReconciler) cleanupDNS(ctx context.Context, holder client.Object, resource string, names []string) (ctrl.Result, error) {
	if r.DNS != nil && r.DNS.DeletePolicy == dns.DeleteDelete {
		results, err := r.DNS.Cleanup(ctx, resource, names)
		r.emitDNSResults(holder, results)
		if err != nil {
			if r.reportDNSError(holder, err, log.FromContext(ctx)) {
				return ctrl.Result{RequeueAfter: dnsRetryInterval}, nil
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, setFinalizer(ctx, r.Client, holder, false)
}

func (r *SecretReconciler) listCertificates(ctx context.Context, namespace string) ([]unstructured.Unstructured, error) {
	if r.noCertificates {
		return nil, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(certGVK.GroupVersion().WithKind("CertificateList"))
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	return list.Items, nil
}

// hasLiveCertificate reports whether a non-terminating Certificate still
// manages the Secret, in which case its records must outlive the Secret.
func (r *SecretReconciler) hasLiveCertificate(ctx context.Context, secret *corev1.Secret, certOwner *unstructured.Unstructured) (bool, error) {
	if certOwner != nil && certOwner.GetDeletionTimestamp() == nil {
		return true, nil
	}
	items, err := r.listCertificates(ctx, secret.Namespace)
	if err != nil {
		return false, err
	}
	for i := range items {
		name, _, _ := unstructured.NestedString(items[i].Object, "spec", "secretName")
		if name == secret.Name && items[i].GetDeletionTimestamp() == nil {
			return true, nil
		}
	}
	return false, nil
}

func (r *SecretReconciler) findTerminatingHolder(ctx context.Context, namespace, secretName string) (*unstructured.Unstructured, error) {
	items, err := r.listCertificates(ctx, namespace)
	if err != nil {
		return nil, err
	}
	for i := range items {
		cert := &items[i]
		name, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
		if name == secretName && cert.GetDeletionTimestamp() != nil && hasDNSFinalizer(cert) {
			return cert, nil
		}
	}
	return nil, nil
}

// terminatingCertificate finds a terminating Certificate holding the cleanup
// finalizer by spec.secretName when the Secret carries no usable ownerReference.
func (r *SecretReconciler) terminatingCertificate(ctx context.Context, secret *corev1.Secret, certOwner *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if r.DNS == nil || certOwner != nil {
		return nil, nil
	}
	return r.findTerminatingHolder(ctx, secret.Namespace, secret.Name)
}

// finalizeOrphanedCertificate releases the cleanup finalizer on a terminating
// Certificate whose Secret is already gone.
func (r *SecretReconciler) finalizeOrphanedCertificate(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cert, err := r.findTerminatingHolder(ctx, req.Namespace, req.Name)
	if err != nil || cert == nil {
		return ctrl.Result{}, err
	}
	return r.cleanupDNS(ctx, cert, dnsResource(req.Namespace, req.Name), annotations.GetDNSRecords(cert.GetAnnotations()))
}
