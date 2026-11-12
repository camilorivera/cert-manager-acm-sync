package controller_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsacm "github.com/aws/aws-sdk-go-v2/service/acm"
	acmtypes "github.com/aws/aws-sdk-go-v2/service/acm/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	acmclient "github.com/camilorivera/cert-manager-acm-sync/internal/acm"
	"github.com/camilorivera/cert-manager-acm-sync/internal/annotations"
	cloudfrontclient "github.com/camilorivera/cert-manager-acm-sync/internal/cloudfront"
	"github.com/camilorivera/cert-manager-acm-sync/internal/controller"
	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
	"github.com/camilorivera/cert-manager-acm-sync/internal/fingerprint"
)

const (
	dnsHost   = "app.example.com"
	dnsARN    = "arn:aws:acm:us-east-1:123:certificate/dns"
	dnsTarget = "lb.example.net"
	dnsOwner  = "test"
)

var dnsZone = dns.Zone{Name: "example.com", ID: "Z1"}

var certGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

type dnsHarness struct {
	r        *controller.SecretReconciler
	recorder *events.FakeRecorder
	acm      *acmclient.MockACMAPI
	provider *dns.MockProvider
	cf       *cloudfrontclient.MockCloudFrontAPI
}

func newDNSHarness(t *testing.T, deletePolicy string, withCF bool) *dnsHarness {
	t.Helper()
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	fakeClient := fake.NewClientBuilder().WithScheme(sc).Build()
	h := &dnsHarness{
		recorder: events.NewFakeRecorder(64),
		acm:      &acmclient.MockACMAPI{},
		provider: &dns.MockProvider{},
	}
	h.provider.On("Name").Return("mock").Maybe()
	h.acm.On("DescribeCertificate", mock.Anything, mock.Anything).
		Return(&awsacm.DescribeCertificateOutput{
			Certificate: &acmtypes.CertificateDetail{CertificateArn: aws.String(dnsARN)},
		}, nil).Maybe()
	h.acm.On("ImportCertificate", mock.Anything, mock.Anything).
		Return(&awsacm.ImportCertificateOutput{CertificateArn: aws.String(dnsARN)}, nil).Maybe()
	h.r = &controller.SecretReconciler{
		Client:        fakeClient,
		Reader:        fakeClient,
		Recorder:      h.recorder,
		ACMPool:       &acmclient.MockPool{Client: h.acm},
		DefaultRegion: "us-east-1",
		DNS: &dns.Syncer{
			Provider:       h.provider,
			Zones:          dns.ZoneSet{dnsZone},
			OwnerID:        dnsOwner,
			WildcardPolicy: dns.WildcardSkip,
			DeletePolicy:   deletePolicy,
		},
		DNSDefaults: controller.DNSDefaults{Target: dnsTarget, TTL: 300, RecordType: dns.TypeCNAME},
	}
	if withCF {
		h.cf = &cloudfrontclient.MockCloudFrontAPI{}
		h.r.CloudFrontClient = h.cf
	}
	return h
}

func (h *dnsHarness) expectFreshName() {
	h.provider.On("GetRecords", mock.Anything, dnsZone, mock.Anything).Return(nil, nil)
}

func (h *dnsHarness) expectCreate(target string) {
	h.provider.On("CreateRecords", mock.Anything, dnsZone, mock.MatchedBy(func(recs []dns.Record) bool {
		return len(recs) == 2 && recs[1].Name == dnsHost && recs[1].Target == target
	})).Return(nil).Once()
}

func (h *dnsHarness) createSynced(t *testing.T, ann map[string]string, owned bool) {
	t.Helper()
	certPEM, keyPEM := generateCertWithSANs(t, []string{dnsHost})
	fp, err := fingerprint.Compute(certPEM)
	require.NoError(t, err)
	all := map[string]string{annotations.Enabled: "true", annotations.ARN: dnsARN, annotations.Fingerprint: fp}
	for k, v := range ann {
		all[k] = v
	}
	secret := tlsSecret(all, certPEM, keyPEM)
	if owned {
		secret.OwnerReferences = []metav1.OwnerReference{{APIVersion: "cert-manager.io/v1", Kind: "Certificate", Name: "my-cert"}}
	}
	require.NoError(t, h.r.Create(context.Background(), secret))
}

func (h *dnsHarness) createFresh(t *testing.T, ann map[string]string) {
	t.Helper()
	certPEM, keyPEM := generateCertWithSANs(t, []string{dnsHost})
	all := map[string]string{annotations.Enabled: "true"}
	for k, v := range ann {
		all[k] = v
	}
	require.NoError(t, h.r.Create(context.Background(), tlsSecret(all, certPEM, keyPEM)))
}

func (h *dnsHarness) createCert(t *testing.T, ann map[string]string, finalizers ...string) *unstructured.Unstructured {
	t.Helper()
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certGVK)
	cert.SetName("my-cert")
	cert.SetNamespace("default")
	cert.SetAnnotations(ann)
	cert.SetFinalizers(finalizers)
	require.NoError(t, unstructured.SetNestedField(cert.Object, "s", "spec", "secretName"))
	require.NoError(t, h.r.Create(context.Background(), cert))
	return cert
}

func (h *dnsHarness) getCert(t *testing.T) (*unstructured.Unstructured, error) {
	t.Helper()
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certGVK)
	err := h.r.Get(context.Background(), k8stypes.NamespacedName{Name: "my-cert", Namespace: "default"}, cert)
	return cert, err
}

func (h *dnsHarness) getSecret(t *testing.T) corev1.Secret {
	t.Helper()
	var s corev1.Secret
	require.NoError(t, h.r.Get(context.Background(), nn().NamespacedName, &s))
	return s
}

func (h *dnsHarness) eventReasons() []string {
	var out []string
	for len(h.recorder.Events) > 0 {
		out = append(out, <-h.recorder.Events)
	}
	return out
}

func hasEvent(evts []string, reason string) bool {
	for _, e := range evts {
		if strings.Contains(e, reason) {
			return true
		}
	}
	return false
}

func TestDNS_SkipPath_CreatesRecordsAndWritesAnnotation(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	h.expectFreshName()
	h.expectCreate(dnsTarget)
	h.createSynced(t, map[string]string{annotations.DNSEnabled: "true"}, false)

	res, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	assert.Equal(t, 6*time.Hour, res.RequeueAfter)
	h.acm.AssertNotCalled(t, "ImportCertificate")
	h.provider.AssertExpectations(t)

	assert.Equal(t, dnsHost, h.getSecret(t).Annotations[annotations.DNSRecords])
	assert.True(t, hasEvent(h.eventReasons(), "DNSRecordCreated"))
}

func TestDNS_ImportPath_RunsAfterCloudFront(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, true)
	var order []string
	h.cf.On("GetDistributionConfig", mock.Anything, mock.Anything).
		Return(&cloudfront.GetDistributionConfigOutput{DistributionConfig: minimalCFConfig(), ETag: aws.String(cfETag)}, nil)
	h.cf.On("UpdateDistribution", mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { order = append(order, "cloudfront") }).
		Return(&cloudfront.UpdateDistributionOutput{}, nil)
	h.provider.On("GetRecords", mock.Anything, dnsZone, mock.Anything).Return(nil, nil)
	h.provider.On("CreateRecords", mock.Anything, dnsZone, mock.Anything).
		Run(func(mock.Arguments) { order = append(order, "dns") }).Return(nil)
	h.createFresh(t, map[string]string{
		annotations.DNSEnabled:                "true",
		annotations.CloudFrontDistributionARN: cfDistARN,
	})

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	h.acm.AssertCalled(t, "ImportCertificate", mock.Anything, mock.Anything)
	assert.Equal(t, []string{"cloudfront", "dns"}, order)
	assert.Equal(t, dnsHost, h.getSecret(t).Annotations[annotations.DNSRecords])
}

func TestDNS_InvalidViewerCertificate_SkipsDNS(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, true)
	h.cf.On("GetDistributionConfig", mock.Anything, mock.Anything).
		Return(&cloudfront.GetDistributionConfigOutput{DistributionConfig: minimalCFConfig(), ETag: aws.String(cfETag)}, nil)
	h.cf.On("UpdateDistribution", mock.Anything, mock.Anything).
		Return(nil, &smithy.GenericAPIError{Code: "InvalidViewerCertificate"})
	h.createFresh(t, map[string]string{
		annotations.DNSEnabled:                "true",
		annotations.CloudFrontDistributionARN: cfDistARN,
	})

	res, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, res.RequeueAfter)
	h.provider.AssertNotCalled(t, "GetRecords", mock.Anything, mock.Anything, mock.Anything)
	h.provider.AssertNotCalled(t, "CreateRecords", mock.Anything, mock.Anything, mock.Anything)
}

func TestDNS_NotOptedIn_MakesNoProviderCalls(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.createSynced(t, nil, false)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	h.provider.AssertNotCalled(t, "GetRecords", mock.Anything, mock.Anything, mock.Anything)
	h.provider.AssertNotCalled(t, "CreateRecords", mock.Anything, mock.Anything, mock.Anything)
	assert.Empty(t, h.getSecret(t).Annotations[annotations.DNSRecords])
	assert.Empty(t, h.getSecret(t).Finalizers)
}

func TestDNS_TargetPrecedence(t *testing.T) {
	const cfDomain = "d111.cloudfront.net"
	tests := []struct {
		name       string
		ann        map[string]string
		defaultTgt string
		wantTarget string
	}{
		{"annotation wins", map[string]string{annotations.DNSTarget: "ann.example.net", annotations.CloudFrontDistributionARN: cfDistARN}, "default.example.net", "ann.example.net"},
		{"default over cloudfront", map[string]string{annotations.CloudFrontDistributionARN: cfDistARN}, "default.example.net", "default.example.net"},
		{"cloudfront domain", map[string]string{annotations.CloudFrontDistributionARN: cfDistARN}, "", cfDomain},
		{"nothing skips", nil, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newDNSHarness(t, dns.DeleteRetain, true)
			h.r.DNSDefaults.Target = tc.defaultTgt
			h.cf.On("GetDistribution", mock.Anything, mock.Anything).
				Return(&cloudfront.GetDistributionOutput{Distribution: &cftypes.Distribution{DomainName: aws.String(cfDomain)}}, nil).Maybe()
			if tc.wantTarget != "" {
				h.expectFreshName()
				h.expectCreate(tc.wantTarget)
			}
			ann := map[string]string{annotations.DNSEnabled: "true"}
			for k, v := range tc.ann {
				ann[k] = v
			}
			h.createSynced(t, ann, false)

			_, err := h.r.Reconcile(context.Background(), nn())
			require.NoError(t, err)
			h.provider.AssertExpectations(t)
			if tc.wantTarget == "" {
				h.provider.AssertNotCalled(t, "CreateRecords", mock.Anything, mock.Anything, mock.Anything)
				assert.True(t, hasEvent(h.eventReasons(), "DNSRecordSkipped"))
			}
		})
	}
}

func TestDNS_SecretAnnotationWinsOverCertificate(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	h.expectFreshName()
	h.expectCreate("secret.example.net")
	h.createCert(t, map[string]string{annotations.DNSEnabled: "true", annotations.DNSTarget: "cert.example.net"})
	h.createSynced(t, map[string]string{annotations.DNSTarget: "secret.example.net"}, true)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	h.provider.AssertExpectations(t)
}

func TestDNS_RecordsAnnotationMirroredToCertificate(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	h.expectFreshName()
	h.expectCreate(dnsTarget)
	h.createCert(t, map[string]string{annotations.DNSEnabled: "true", annotations.ARN: dnsARN})
	h.createSynced(t, nil, true)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)

	cert, err := h.getCert(t)
	require.NoError(t, err)
	assert.Equal(t, dnsHost, cert.GetAnnotations()[annotations.DNSRecords])
	assert.Equal(t, dnsHost, h.getSecret(t).Annotations[annotations.DNSRecords])
}

func TestDNS_NoPatchWhenRecordsUnchanged(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	h.provider.On("GetRecords", mock.Anything, dnsZone, dnsHost).
		Return([]dns.Record{{Name: dnsHost, Type: dns.TypeCNAME, Target: dnsTarget, TTL: 300}}, nil)
	h.provider.On("GetRecords", mock.Anything, dnsZone, dns.OwnerName(dnsHost)).
		Return([]dns.Record{{Name: dns.OwnerName(dnsHost), Type: dns.TypeTXT, Target: dns.EncodeOwner(dnsOwner, "secret/default/s")}}, nil)
	h.createCert(t, map[string]string{annotations.DNSEnabled: "true", annotations.DNSRecords: dnsHost, annotations.ARN: dnsARN})
	h.createSynced(t, map[string]string{annotations.DNSRecords: dnsHost}, true)
	before, err := h.getCert(t)
	require.NoError(t, err)

	_, err = h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	after, err := h.getCert(t)
	require.NoError(t, err)
	assert.Equal(t, before.GetResourceVersion(), after.GetResourceVersion())
	h.provider.AssertNotCalled(t, "CreateRecords", mock.Anything, mock.Anything, mock.Anything)
}

func TestDNS_Retryable_RequeuesAfterFiveMinutes(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	h.provider.On("GetRecords", mock.Anything, dnsZone, mock.Anything).
		Return(nil, fmt.Errorf("rate limited: %w", dns.ErrRetryable))
	h.createSynced(t, map[string]string{annotations.DNSEnabled: "true"}, false)

	res, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, res.RequeueAfter)
	assert.True(t, hasEvent(h.eventReasons(), "DNSSyncFailed"))
}

func TestDNS_Failure_IsNonFatal(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	h.provider.On("GetRecords", mock.Anything, dnsZone, mock.Anything).Return(nil, errors.New("boom"))
	h.createFresh(t, map[string]string{annotations.DNSEnabled: "true"})

	res, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, res.RequeueAfter)
	assert.Equal(t, dnsARN, h.getSecret(t).Annotations[annotations.ARN])
	assert.True(t, hasEvent(h.eventReasons(), "DNSSyncFailed"))
}

func TestDNS_Conflict_EmitsWarning(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	h.provider.On("GetRecords", mock.Anything, dnsZone, dnsHost).
		Return([]dns.Record{{Name: dnsHost, Type: dns.TypeCNAME, Target: "other.example.net", TTL: 60}}, nil)
	h.provider.On("GetRecords", mock.Anything, dnsZone, dns.OwnerName(dnsHost)).Return(nil, nil)
	h.createSynced(t, map[string]string{annotations.DNSEnabled: "true"}, false)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	assert.True(t, hasEvent(h.eventReasons(), "DNSRecordConflict"))
	assert.Empty(t, h.getSecret(t).Annotations[annotations.DNSRecords])
}

func TestDNS_Finalizer_AddedOnCertificateWithDeletePolicy(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.expectFreshName()
	h.expectCreate(dnsTarget)
	h.createCert(t, map[string]string{annotations.DNSEnabled: "true"})
	h.createSynced(t, nil, true)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)

	cert, err := h.getCert(t)
	require.NoError(t, err)
	assert.Contains(t, cert.GetFinalizers(), annotations.DNSCleanupFinalizer)
	assert.NotContains(t, h.getSecret(t).Finalizers, annotations.DNSCleanupFinalizer)
}

func TestDNS_Finalizer_FallsBackToSecretWithoutCertificate(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.expectFreshName()
	h.expectCreate(dnsTarget)
	h.createSynced(t, map[string]string{annotations.DNSEnabled: "true"}, false)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	assert.Contains(t, h.getSecret(t).Finalizers, annotations.DNSCleanupFinalizer)
}

func TestDNS_Finalizer_NeverAddedWithRetainPolicy(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	h.expectFreshName()
	h.expectCreate(dnsTarget)
	h.createCert(t, map[string]string{annotations.DNSEnabled: "true"})
	h.createSynced(t, nil, true)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	cert, err := h.getCert(t)
	require.NoError(t, err)
	assert.Empty(t, cert.GetFinalizers())
}

func TestDNS_Finalizer_RemovedWhenPolicyIsRetain(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	h.expectFreshName()
	h.expectCreate(dnsTarget)
	h.createCert(t, map[string]string{annotations.DNSEnabled: "true"}, annotations.DNSCleanupFinalizer)
	h.createSynced(t, nil, true)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	cert, err := h.getCert(t)
	require.NoError(t, err)
	assert.Empty(t, cert.GetFinalizers())
}

func TestDNS_Finalizer_RemovedWhenNotOptedIn(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.createCert(t, nil, annotations.DNSCleanupFinalizer)
	h.createSynced(t, nil, true)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	cert, err := h.getCert(t)
	require.NoError(t, err)
	assert.Empty(t, cert.GetFinalizers())
	h.provider.AssertNotCalled(t, "GetRecords", mock.Anything, mock.Anything, mock.Anything)
}

func (h *dnsHarness) expectOwnedRecord() {
	h.provider.On("GetRecords", mock.Anything, dnsZone, dnsHost).
		Return([]dns.Record{{Name: dnsHost, Type: dns.TypeCNAME, Target: dnsTarget, TTL: 300}}, nil)
	h.provider.On("GetRecords", mock.Anything, dnsZone, dns.OwnerName(dnsHost)).
		Return([]dns.Record{{Name: dns.OwnerName(dnsHost), Type: dns.TypeTXT, Target: dns.EncodeOwner(dnsOwner, "secret/default/s"), TTL: 300}}, nil)
}

func (h *dnsHarness) deleteCert(t *testing.T, cert *unstructured.Unstructured) {
	t.Helper()
	require.NoError(t, h.r.Delete(context.Background(), cert))
}

func TestDNS_Cleanup_OnCertificateDeletion(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.expectOwnedRecord()
	h.provider.On("DeleteRecords", mock.Anything, dnsZone, mock.Anything).Return(nil).Twice()
	cert := h.createCert(t, map[string]string{annotations.DNSEnabled: "true", annotations.DNSRecords: dnsHost}, annotations.DNSCleanupFinalizer)
	h.createSynced(t, nil, true)
	h.deleteCert(t, cert)

	res, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
	h.provider.AssertNumberOfCalls(t, "DeleteRecords", 2)
	h.acm.AssertNotCalled(t, "DescribeCertificate")

	_, err = h.getCert(t)
	assert.True(t, apierrors.IsNotFound(err), "certificate must be released once cleanup succeeds")
	assert.True(t, hasEvent(h.eventReasons(), "DNSRecordDeleted"))
}

func TestDNS_Cleanup_RetryableKeepsFinalizer(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.expectOwnedRecord()
	h.provider.On("DeleteRecords", mock.Anything, dnsZone, mock.Anything).
		Return(fmt.Errorf("throttled: %w", dns.ErrRetryable))
	cert := h.createCert(t, map[string]string{annotations.DNSRecords: dnsHost}, annotations.DNSCleanupFinalizer)
	h.createSynced(t, nil, true)
	h.deleteCert(t, cert)

	res, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, res.RequeueAfter)
	got, err := h.getCert(t)
	require.NoError(t, err)
	assert.Contains(t, got.GetFinalizers(), annotations.DNSCleanupFinalizer)
}

func TestDNS_Cleanup_ErrorKeepsFinalizerAndFails(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.provider.On("GetRecords", mock.Anything, dnsZone, mock.Anything).Return(nil, errors.New("boom"))
	cert := h.createCert(t, map[string]string{annotations.DNSRecords: dnsHost}, annotations.DNSCleanupFinalizer)
	h.createSynced(t, nil, true)
	h.deleteCert(t, cert)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.Error(t, err)
	got, err := h.getCert(t)
	require.NoError(t, err)
	assert.Contains(t, got.GetFinalizers(), annotations.DNSCleanupFinalizer)
}

func TestDNS_Cleanup_RetainPolicyReleasesWithoutDeleting(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	cert := h.createCert(t, map[string]string{annotations.DNSRecords: dnsHost}, annotations.DNSCleanupFinalizer)
	h.createSynced(t, nil, true)
	h.deleteCert(t, cert)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	h.provider.AssertNotCalled(t, "DeleteRecords", mock.Anything, mock.Anything, mock.Anything)
	_, err = h.getCert(t)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestDNS_Cleanup_OnSecretDeletionFallback(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.expectOwnedRecord()
	h.provider.On("DeleteRecords", mock.Anything, dnsZone, mock.Anything).Return(nil)
	h.createSynced(t, map[string]string{annotations.DNSRecords: dnsHost}, false)
	secret := h.getSecret(t)
	secret.Finalizers = []string{annotations.DNSCleanupFinalizer}
	require.NoError(t, h.r.Update(context.Background(), &secret))
	require.NoError(t, h.r.Delete(context.Background(), &secret))

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	h.provider.AssertNumberOfCalls(t, "DeleteRecords", 2)
	var gone corev1.Secret
	err = h.r.Get(context.Background(), nn().NamespacedName, &gone)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestDNS_Cleanup_CertificateDeletedAfterSecretIsGone(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.expectOwnedRecord()
	h.provider.On("DeleteRecords", mock.Anything, dnsZone, mock.Anything).Return(nil)
	cert := h.createCert(t, map[string]string{annotations.DNSRecords: dnsHost}, annotations.DNSCleanupFinalizer)
	h.deleteCert(t, cert)

	_, err := h.r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	h.provider.AssertNumberOfCalls(t, "DeleteRecords", 2)
	_, err = h.getCert(t)
	assert.True(t, apierrors.IsNotFound(err))
}

type failingGetReader struct {
	client.Reader
	err error
}

func (f failingGetReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return f.err
}

func TestDNS_OwnerLookupError_RequeuesWithError(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.createSynced(t, map[string]string{annotations.DNSEnabled: "true"}, true)
	h.r.Reader = failingGetReader{Reader: h.r.Reader, err: errors.New("apiserver down")}

	_, err := h.r.Reconcile(context.Background(), nn())

	require.Error(t, err)
	h.provider.AssertNotCalled(t, "GetRecords", mock.Anything, mock.Anything, mock.Anything)
}

func TestDNS_OwnerNotFound_IsNotAnError(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	h.createSynced(t, nil, true)

	_, err := h.r.Reconcile(context.Background(), nn())

	require.NoError(t, err)
}

func TestNoDNS_OwnerLookupError_IsSwallowed(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteRetain, false)
	h.r.DNS = nil
	h.createSynced(t, nil, true)
	h.r.Reader = failingGetReader{Reader: h.r.Reader, err: errors.New("apiserver down")}

	res, err := h.r.Reconcile(context.Background(), nn())

	require.NoError(t, err)
	assert.Equal(t, 6*time.Hour, res.RequeueAfter)
}

func TestDNS_Cleanup_TerminatingCertificateWithoutOwnerReference(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.expectOwnedRecord()
	h.provider.On("DeleteRecords", mock.Anything, dnsZone, mock.Anything).Return(nil)
	cert := h.createCert(t, map[string]string{annotations.DNSRecords: dnsHost}, annotations.DNSCleanupFinalizer)
	h.createSynced(t, nil, false)
	h.deleteCert(t, cert)

	_, err := h.r.Reconcile(context.Background(), nn())

	require.NoError(t, err)
	h.provider.AssertNumberOfCalls(t, "DeleteRecords", 2)
	_, err = h.getCert(t)
	assert.True(t, apierrors.IsNotFound(err), "certificate must be released")
}

func TestDNS_Cleanup_UnrelatedTerminatingCertificateIsIgnored(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	cert := h.createCert(t, map[string]string{annotations.DNSRecords: dnsHost}, annotations.DNSCleanupFinalizer)
	require.NoError(t, unstructured.SetNestedField(cert.Object, "other", "spec", "secretName"))
	require.NoError(t, h.r.Update(context.Background(), cert))
	h.createSynced(t, nil, false)
	h.deleteCert(t, cert)

	_, err := h.r.Reconcile(context.Background(), nn())

	require.NoError(t, err)
	h.provider.AssertNotCalled(t, "DeleteRecords", mock.Anything, mock.Anything, mock.Anything)
	got, err := h.getCert(t)
	require.NoError(t, err)
	assert.Contains(t, got.GetFinalizers(), annotations.DNSCleanupFinalizer)
}

func TestDNS_Finalizer_StaleSecretFinalizerRemovedWhenCertificateHolds(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.expectFreshName()
	h.expectCreate(dnsTarget)
	h.createCert(t, map[string]string{annotations.DNSEnabled: "true"})
	h.createSynced(t, nil, true)
	secret := h.getSecret(t)
	secret.Finalizers = []string{annotations.DNSCleanupFinalizer}
	require.NoError(t, h.r.Update(context.Background(), &secret))

	_, err := h.r.Reconcile(context.Background(), nn())

	require.NoError(t, err)
	assert.NotContains(t, h.getSecret(t).Finalizers, annotations.DNSCleanupFinalizer)
	cert, err := h.getCert(t)
	require.NoError(t, err)
	assert.Contains(t, cert.GetFinalizers(), annotations.DNSCleanupFinalizer)
}

func TestDNS_Finalizer_NotAddedToTerminatingHolder(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.expectFreshName()
	h.provider.On("CreateRecords", mock.Anything, dnsZone, mock.Anything).Return(nil)
	cert := h.createCert(t, map[string]string{annotations.DNSEnabled: "true"}, "example.com/other")
	h.createSynced(t, nil, true)
	h.deleteCert(t, cert)

	_, err := h.r.Reconcile(context.Background(), nn())

	require.NoError(t, err)
	got, err := h.getCert(t)
	require.NoError(t, err)
	assert.NotContains(t, got.GetFinalizers(), annotations.DNSCleanupFinalizer)
}

func (h *dnsHarness) createTerminatingSecretHolder(t *testing.T) {
	t.Helper()
	h.createSynced(t, map[string]string{annotations.DNSRecords: dnsHost}, false)
	secret := h.getSecret(t)
	secret.Finalizers = []string{annotations.DNSCleanupFinalizer}
	require.NoError(t, h.r.Update(context.Background(), &secret))
	require.NoError(t, h.r.Delete(context.Background(), &secret))
}

func TestDNS_Cleanup_SecretDeletionKeepsRecordsWhileLiveCertificateExists(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.createCert(t, map[string]string{annotations.DNSEnabled: "true"})
	h.createTerminatingSecretHolder(t)

	_, err := h.r.Reconcile(context.Background(), nn())

	require.NoError(t, err)
	h.provider.AssertNotCalled(t, "DeleteRecords", mock.Anything, mock.Anything, mock.Anything)
	var gone corev1.Secret
	assert.True(t, apierrors.IsNotFound(h.r.Get(context.Background(), nn().NamespacedName, &gone)), "finalizer must be released")
}

func TestDNS_Cleanup_SecretDeletionStillCleansUpWhenCertificateIsTerminating(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.expectOwnedRecord()
	h.provider.On("DeleteRecords", mock.Anything, dnsZone, mock.Anything).Return(nil)
	cert := h.createCert(t, map[string]string{annotations.DNSEnabled: "true"}, "example.com/other")
	h.deleteCert(t, cert)
	h.createTerminatingSecretHolder(t)

	_, err := h.r.Reconcile(context.Background(), nn())

	require.NoError(t, err)
	h.provider.AssertNumberOfCalls(t, "DeleteRecords", 2)
}

func TestDNS_Cleanup_SecretDeletionKeepsRecordsWhenOwnerReferenceCertificateIsLive(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	h.createCert(t, map[string]string{annotations.DNSEnabled: "true"})
	h.createSynced(t, map[string]string{annotations.DNSRecords: dnsHost}, true)
	secret := h.getSecret(t)
	secret.Finalizers = []string{annotations.DNSCleanupFinalizer}
	require.NoError(t, h.r.Update(context.Background(), &secret))
	require.NoError(t, h.r.Delete(context.Background(), &secret))

	_, err := h.r.Reconcile(context.Background(), nn())

	require.NoError(t, err)
	h.provider.AssertNotCalled(t, "DeleteRecords", mock.Anything, mock.Anything, mock.Anything)
}

func TestDNS_Sync_SkippedForTerminatingHolder(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	cert := h.createCert(t, map[string]string{annotations.DNSEnabled: "true"}, "example.com/other")
	h.createSynced(t, nil, true)
	h.deleteCert(t, cert)

	_, err := h.r.Reconcile(context.Background(), nn())

	require.NoError(t, err)
	h.provider.AssertNotCalled(t, "GetRecords", mock.Anything, mock.Anything, mock.Anything)
	h.provider.AssertNotCalled(t, "CreateRecords", mock.Anything, mock.Anything, mock.Anything)
}

func TestNoDNS_MissingSecret_MakesNoCertificateCalls(t *testing.T) {
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	var calls int
	fc := fake.NewClientBuilder().WithScheme(sc).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			calls++
			return errors.New("unexpected list")
		},
	}).Build()
	r := &controller.SecretReconciler{Client: fc, Reader: fc, Recorder: events.NewFakeRecorder(8)}

	res, err := r.Reconcile(context.Background(), nn())

	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
	assert.Zero(t, calls)
}

func TestDNS_MissingSecret_ListErrorIsReturned(t *testing.T) {
	h := newDNSHarness(t, dns.DeleteDelete, false)
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	fc := fake.NewClientBuilder().WithScheme(sc).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("boom")
		},
	}).Build()
	h.r.Client, h.r.Reader = fc, fc

	_, err := h.r.Reconcile(context.Background(), nn())

	require.Error(t, err)
}
