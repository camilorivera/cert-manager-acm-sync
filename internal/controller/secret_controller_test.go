package controller_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsacm "github.com/aws/aws-sdk-go-v2/service/acm"
	acmtypes "github.com/aws/aws-sdk-go-v2/service/acm/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	acmclient "github.com/camilorivera/cert-manager-acm-sync/internal/acm"
	"github.com/camilorivera/cert-manager-acm-sync/internal/annotations"
	cloudfrontclient "github.com/camilorivera/cert-manager-acm-sync/internal/cloudfront"
	"github.com/camilorivera/cert-manager-acm-sync/internal/controller"
	"github.com/camilorivera/cert-manager-acm-sync/internal/fingerprint"
)

func generateCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	return generateCertWithSANs(t, []string{"test.local"})
}

func generateCertWithSANs(t *testing.T, dnsNames []string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return
}

func buildReconciler(t *testing.T, acmMock *acmclient.MockACMAPI) (*controller.SecretReconciler, *events.FakeRecorder) {
	t.Helper()
	return buildReconcilerWithCF(t, acmMock, nil)
}

func buildReconcilerWithCF(t *testing.T, acmMock *acmclient.MockACMAPI, cfMock *cloudfrontclient.MockCloudFrontAPI) (*controller.SecretReconciler, *events.FakeRecorder) {
	t.Helper()
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	fakeClient := fake.NewClientBuilder().WithScheme(sc).Build()
	recorder := events.NewFakeRecorder(32)
	var cfClient cloudfrontclient.CloudFrontAPI
	if cfMock != nil {
		cfClient = cfMock
	}
	return &controller.SecretReconciler{
		Client:           fakeClient,
		Reader:           fakeClient,
		Recorder:         recorder,
		ACMPool:          &acmclient.MockPool{Client: acmMock},
		DefaultRegion:    "us-east-1",
		CloudFrontClient: cfClient,
	}, recorder
}

func tlsSecret(ann map[string]string, certPEM, keyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "s",
			Namespace:   "default",
			Annotations: ann,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}
}

func nn() ctrl.Request {
	return ctrl.Request{NamespacedName: k8stypes.NamespacedName{Name: "s", Namespace: "default"}}
}

func TestReconcile_SecretWithoutAnnotation_SkipsACM(t *testing.T) {
	certPEM, keyPEM := generateCert(t)
	m := &acmclient.MockACMAPI{}
	r, _ := buildReconciler(t, m)

	require.NoError(t, r.Create(context.Background(),
		tlsSecret(nil, certPEM, keyPEM)))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	m.AssertNotCalled(t, "ImportCertificate")
	m.AssertNotCalled(t, "DescribeCertificate")
}

func TestReconcile_FirstImport_WritesARNAnnotation(t *testing.T) {
	certPEM, keyPEM := generateCert(t)
	const arn = "arn:aws:acm:us-east-1:123:certificate/new"

	m := &acmclient.MockACMAPI{}
	m.On("ImportCertificate", mock.Anything, mock.MatchedBy(func(in *awsacm.ImportCertificateInput) bool {
		return in.CertificateArn == nil
	})).Return(&awsacm.ImportCertificateOutput{CertificateArn: aws.String(arn)}, nil)

	r, recorder := buildReconciler(t, m)
	require.NoError(t, r.Create(context.Background(),
		tlsSecret(map[string]string{annotations.Enabled: "true"}, certPEM, keyPEM)))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)

	var updated corev1.Secret
	require.NoError(t, r.Get(context.Background(),
		k8stypes.NamespacedName{Name: "s", Namespace: "default"}, &updated))
	assert.Equal(t, arn, updated.Annotations[annotations.ARN])
	assert.NotEmpty(t, updated.Annotations[annotations.Fingerprint])
	assert.NotEmpty(t, updated.Annotations[annotations.LastSync])

	evt := <-recorder.Events
	assert.Contains(t, evt, "Synced")
}

func TestReconcile_FingerprintMatch_SkipsImport(t *testing.T) {
	certPEM, keyPEM := generateCert(t)
	fp, err := fingerprint.Compute(certPEM)
	require.NoError(t, err)

	const arn = "arn:aws:acm:us-east-1:123:certificate/existing"
	m := &acmclient.MockACMAPI{}
	m.On("DescribeCertificate", mock.Anything, mock.Anything).
		Return(&awsacm.DescribeCertificateOutput{
			Certificate: &acmtypes.CertificateDetail{CertificateArn: aws.String(arn)},
		}, nil)

	r, _ := buildReconciler(t, m)
	require.NoError(t, r.Create(context.Background(),
		tlsSecret(map[string]string{
			annotations.Enabled:     "true",
			annotations.ARN:         arn,
			annotations.Fingerprint: fp,
		}, certPEM, keyPEM)))

	_, err = r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	m.AssertNotCalled(t, "ImportCertificate")
}

func TestReconcile_StaleARN_CreatesNewCertificate(t *testing.T) {
	certPEM, keyPEM := generateCert(t)
	const staleARN = "arn:aws:acm:us-east-1:123:certificate/deleted"
	const newARN = "arn:aws:acm:us-east-1:123:certificate/fresh"

	m := &acmclient.MockACMAPI{}
	m.On("DescribeCertificate", mock.Anything, mock.MatchedBy(func(in *awsacm.DescribeCertificateInput) bool {
		return aws.ToString(in.CertificateArn) == staleARN
	})).Return(nil, &acmtypes.ResourceNotFoundException{Message: aws.String("not found")})
	m.On("ImportCertificate", mock.Anything, mock.MatchedBy(func(in *awsacm.ImportCertificateInput) bool {
		return in.CertificateArn == nil
	})).Return(&awsacm.ImportCertificateOutput{CertificateArn: aws.String(newARN)}, nil)

	r, recorder := buildReconciler(t, m)
	require.NoError(t, r.Create(context.Background(),
		tlsSecret(map[string]string{
			annotations.Enabled:     "true",
			annotations.ARN:         staleARN,
			annotations.Fingerprint: "old-fp",
		}, certPEM, keyPEM)))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)

	var updated corev1.Secret
	require.NoError(t, r.Get(context.Background(),
		k8stypes.NamespacedName{Name: "s", Namespace: "default"}, &updated))
	assert.Equal(t, newARN, updated.Annotations[annotations.ARN])

	evt1 := <-recorder.Events
	evt2 := <-recorder.Events
	assert.Contains(t, evt1+evt2, "CertificateNotFound")
	assert.Contains(t, evt1+evt2, "Synced")
}

func TestReconcile_Renewal_ReimportsWithSameARN(t *testing.T) {
	certPEM, keyPEM := generateCert(t)
	const arn = "arn:aws:acm:us-east-1:123:certificate/existing"

	m := &acmclient.MockACMAPI{}
	m.On("DescribeCertificate", mock.Anything, mock.Anything).
		Return(&awsacm.DescribeCertificateOutput{
			Certificate: &acmtypes.CertificateDetail{CertificateArn: aws.String(arn)},
		}, nil)
	m.On("ImportCertificate", mock.Anything, mock.MatchedBy(func(in *awsacm.ImportCertificateInput) bool {
		return aws.ToString(in.CertificateArn) == arn
	})).Return(&awsacm.ImportCertificateOutput{CertificateArn: aws.String(arn)}, nil)

	r, _ := buildReconciler(t, m)
	require.NoError(t, r.Create(context.Background(),
		tlsSecret(map[string]string{
			annotations.Enabled:     "true",
			annotations.ARN:         arn,
			annotations.Fingerprint: "old-fp",
		}, certPEM, keyPEM)))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)

	var updated corev1.Secret
	require.NoError(t, r.Get(context.Background(),
		k8stypes.NamespacedName{Name: "s", Namespace: "default"}, &updated))
	assert.Equal(t, arn, updated.Annotations[annotations.ARN], "ARN must not change on renewal")
}

// TestReconcile_CertificateAnnotation_EnablesSync verifies that acm.sync/enabled
// on the Certificate resource (not propagated to the Secret via secretTemplate)
// is sufficient to trigger an ACM import.
func TestReconcile_CertificateAnnotation_EnablesSync(t *testing.T) {
	certPEM, keyPEM := generateCert(t)
	const arn = "arn:aws:acm:us-east-1:123:certificate/new"

	m := &acmclient.MockACMAPI{}
	m.On("ImportCertificate", mock.Anything, mock.MatchedBy(func(in *awsacm.ImportCertificateInput) bool {
		return in.CertificateArn == nil
	})).Return(&awsacm.ImportCertificateOutput{CertificateArn: aws.String(arn)}, nil)

	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	fakeClient := fake.NewClientBuilder().WithScheme(sc).Build()
	recorder := events.NewFakeRecorder(32)
	r := &controller.SecretReconciler{
		Client:        fakeClient,
		Reader:        fakeClient,
		Recorder:      recorder,
		ACMPool:       &acmclient.MockPool{Client: m},
		DefaultRegion: "us-east-1",
	}

	// Certificate carries acm.sync/enabled; Secret has no annotation.
	certObj := &unstructured.Unstructured{}
	certObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	certObj.SetName("my-cert")
	certObj.SetNamespace("default")
	certObj.SetAnnotations(map[string]string{annotations.Enabled: "true"})
	require.NoError(t, fakeClient.Create(context.Background(), certObj))

	secret := tlsSecret(nil, certPEM, keyPEM) // no acm.sync/enabled on Secret
	secret.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "cert-manager.io/v1",
		Kind:       "Certificate",
		Name:       "my-cert",
	}}
	require.NoError(t, fakeClient.Create(context.Background(), secret))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)

	var updated corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(),
		k8stypes.NamespacedName{Name: "s", Namespace: "default"}, &updated))
	assert.Equal(t, arn, updated.Annotations[annotations.ARN])
	m.AssertCalled(t, "ImportCertificate", mock.Anything, mock.Anything)
}

// TestReconcile_CertificateAnnotation_RegionFallback verifies that
// acm.sync/region on the Certificate is used when the Secret has no region
// annotation.
func TestReconcile_CertificateAnnotation_RegionFallback(t *testing.T) {
	certPEM, keyPEM := generateCert(t)
	const arn = "arn:aws:acm:eu-west-1:123:certificate/new"

	m := &acmclient.MockACMAPI{}
	m.On("ImportCertificate", mock.Anything, mock.Anything).
		Return(&awsacm.ImportCertificateOutput{CertificateArn: aws.String(arn)}, nil)

	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)

	var calledRegion string
	pool := &regionCapturingPool{
		client:  m,
		capture: func(r string) { calledRegion = r },
	}

	fakeClient := fake.NewClientBuilder().WithScheme(sc).Build()
	recorder := events.NewFakeRecorder(32)
	r := &controller.SecretReconciler{
		Client:        fakeClient,
		Reader:        fakeClient,
		Recorder:      recorder,
		ACMPool:       pool,
		DefaultRegion: "us-east-1",
	}

	certObj := &unstructured.Unstructured{}
	certObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	certObj.SetName("my-cert")
	certObj.SetNamespace("default")
	certObj.SetAnnotations(map[string]string{
		annotations.Enabled: "true",
		annotations.Region:  "eu-west-1",
	})
	require.NoError(t, fakeClient.Create(context.Background(), certObj))

	secret := tlsSecret(nil, certPEM, keyPEM)
	secret.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "cert-manager.io/v1",
		Kind:       "Certificate",
		Name:       "my-cert",
	}}
	require.NoError(t, fakeClient.Create(context.Background(), secret))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	assert.Equal(t, "eu-west-1", calledRegion)
}

// regionCapturingPool records the region passed to ClientForRegion.
type regionCapturingPool struct {
	client  acmclient.ACMAPI
	capture func(string)
}

func (p *regionCapturingPool) ClientForRegion(region string) acmclient.ACMAPI {
	p.capture(region)
	return p.client
}

// TestReconcile_Skip_BackfillsARNOntoCertificate verifies that when the
// fingerprint is unchanged (skip path), the controller still mirrors the ARN
// onto the owning Certificate if it is missing — covering the upgrade case
// where certificates were synced before the Certificate write-back was added.
func TestReconcile_Skip_BackfillsARNOntoCertificate(t *testing.T) {
	certPEM, keyPEM := generateCert(t)
	fp, err := fingerprint.Compute(certPEM)
	require.NoError(t, err)
	const arn = "arn:aws:acm:us-east-1:123:certificate/existing"

	m := &acmclient.MockACMAPI{}
	m.On("DescribeCertificate", mock.Anything, mock.Anything).
		Return(&awsacm.DescribeCertificateOutput{
			Certificate: &acmtypes.CertificateDetail{CertificateArn: aws.String(arn)},
		}, nil)

	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	fakeClient := fake.NewClientBuilder().WithScheme(sc).Build()
	recorder := events.NewFakeRecorder(32)
	r := &controller.SecretReconciler{
		Client:        fakeClient,
		Reader:        fakeClient,
		Recorder:      recorder,
		ACMPool:       &acmclient.MockPool{Client: m},
		DefaultRegion: "us-east-1",
	}

	// Certificate exists but has no ARN annotation (pre-upgrade state).
	certObj := &unstructured.Unstructured{}
	certObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	certObj.SetName("my-cert")
	certObj.SetNamespace("default")
	require.NoError(t, fakeClient.Create(context.Background(), certObj))

	// Secret is already fully synced (ARN + matching fingerprint).
	secret := tlsSecret(map[string]string{
		annotations.Enabled:     "true",
		annotations.ARN:         arn,
		annotations.Fingerprint: fp,
	}, certPEM, keyPEM)
	secret.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "cert-manager.io/v1",
		Kind:       "Certificate",
		Name:       "my-cert",
	}}
	require.NoError(t, fakeClient.Create(context.Background(), secret))

	_, err = r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	m.AssertNotCalled(t, "ImportCertificate")

	// Certificate must now have the ARN backfilled.
	var updatedCert unstructured.Unstructured
	updatedCert.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	require.NoError(t, fakeClient.Get(context.Background(),
		k8stypes.NamespacedName{Name: "my-cert", Namespace: "default"}, &updatedCert))
	assert.Equal(t, arn, updatedCert.GetAnnotations()[annotations.ARN])
}

// TestReconcile_SecretRecreated_RecoverARNFromCertificate verifies that when a
// Secret is deleted and recreated by cert-manager (losing the acm.sync/arn
// annotation), the controller recovers the ARN from the owning Certificate and
// reimports to the same ACM certificate instead of creating a new one.
func TestReconcile_SecretRecreated_RecoverARNFromCertificate(t *testing.T) {
	certPEM, keyPEM := generateCert(t)
	const existingARN = "arn:aws:acm:us-east-1:123:certificate/existing"

	m := &acmclient.MockACMAPI{}
	m.On("DescribeCertificate", mock.Anything, mock.MatchedBy(func(in *awsacm.DescribeCertificateInput) bool {
		return aws.ToString(in.CertificateArn) == existingARN
	})).Return(&awsacm.DescribeCertificateOutput{
		Certificate: &acmtypes.CertificateDetail{CertificateArn: aws.String(existingARN)},
	}, nil)
	m.On("ImportCertificate", mock.Anything, mock.MatchedBy(func(in *awsacm.ImportCertificateInput) bool {
		return aws.ToString(in.CertificateArn) == existingARN
	})).Return(&awsacm.ImportCertificateOutput{CertificateArn: aws.String(existingARN)}, nil)

	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	fakeClient := fake.NewClientBuilder().WithScheme(sc).Build()
	recorder := events.NewFakeRecorder(32)
	r := &controller.SecretReconciler{
		Client:        fakeClient,
		Reader:        fakeClient,
		Recorder:      recorder,
		ACMPool:       &acmclient.MockPool{Client: m},
		DefaultRegion: "us-east-1",
	}

	// Create the Certificate owner with the ARN already persisted.
	certObj := &unstructured.Unstructured{}
	certObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	certObj.SetName("my-cert")
	certObj.SetNamespace("default")
	certObj.SetAnnotations(map[string]string{annotations.ARN: existingARN})
	require.NoError(t, fakeClient.Create(context.Background(), certObj))

	// Recreated Secret: has no ARN annotation but points to the Certificate owner.
	secret := tlsSecret(map[string]string{annotations.Enabled: "true"}, certPEM, keyPEM)
	secret.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "cert-manager.io/v1",
		Kind:       "Certificate",
		Name:       "my-cert",
	}}
	require.NoError(t, fakeClient.Create(context.Background(), secret))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)

	// Secret must have the recovered ARN written back.
	var updatedSecret corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(),
		k8stypes.NamespacedName{Name: "s", Namespace: "default"}, &updatedSecret))
	assert.Equal(t, existingARN, updatedSecret.Annotations[annotations.ARN])

	// Certificate must still carry the ARN (write-back confirmed).
	var updatedCert unstructured.Unstructured
	updatedCert.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	require.NoError(t, fakeClient.Get(context.Background(),
		k8stypes.NamespacedName{Name: "my-cert", Namespace: "default"}, &updatedCert))
	assert.Equal(t, existingARN, updatedCert.GetAnnotations()[annotations.ARN])

	// Must have reimported to the SAME ARN, not created a fresh certificate.
	m.AssertCalled(t, "ImportCertificate", mock.Anything, mock.MatchedBy(func(in *awsacm.ImportCertificateInput) bool {
		return aws.ToString(in.CertificateArn) == existingARN
	}))
	m.AssertNotCalled(t, "ImportCertificate", mock.Anything, mock.MatchedBy(func(in *awsacm.ImportCertificateInput) bool {
		return in.CertificateArn == nil
	}))
}

// ── CloudFront sync tests ─────────────────────────────────────────────────────

const (
	cfDistARN = "arn:aws:cloudfront::123456789012:distribution/EDFDVBD6EXAMPLE"
	cfACMARN  = "arn:aws:acm:us-east-1:123:certificate/cf-cert"
	cfETag    = "E2QWRUHEXAMPLE"
)

func minimalCFConfig() *cftypes.DistributionConfig {
	return &cftypes.DistributionConfig{
		CallerReference: aws.String("ref"),
		Comment:         aws.String(""),
		DefaultCacheBehavior: &cftypes.DefaultCacheBehavior{
			ViewerProtocolPolicy: cftypes.ViewerProtocolPolicyRedirectToHttps,
			TargetOriginId:       aws.String("origin"),
			AllowedMethods: &cftypes.AllowedMethods{
				Quantity: aws.Int32(2),
				Items:    []cftypes.Method{cftypes.MethodGet, cftypes.MethodHead},
			},
		},
		Enabled: aws.Bool(true),
		Origins: &cftypes.Origins{
			Quantity: aws.Int32(1),
			Items:    []cftypes.Origin{{Id: aws.String("origin"), DomainName: aws.String("s3.example.com")}},
		},
		ViewerCertificate: &cftypes.ViewerCertificate{
			SSLSupportMethod:             cftypes.SSLSupportMethodSniOnly,
			MinimumProtocolVersion:       cftypes.MinimumProtocolVersionTLSv122021,
			CloudFrontDefaultCertificate: aws.Bool(false),
		},
	}
}

func setupCFImportMock(acmMock *acmclient.MockACMAPI) {
	acmMock.On("ImportCertificate", mock.Anything, mock.Anything).
		Return(&awsacm.ImportCertificateOutput{CertificateArn: aws.String(cfACMARN)}, nil)
}

func TestReconcile_CloudFront_HappyPath(t *testing.T) {
	certPEM, keyPEM := generateCertWithSANs(t, []string{"example.com", "www.example.com"})
	acmMock := &acmclient.MockACMAPI{}
	setupCFImportMock(acmMock)
	cfMock := &cloudfrontclient.MockCloudFrontAPI{}
	cfMock.On("GetDistributionConfig", mock.Anything, mock.Anything).
		Return(&cloudfront.GetDistributionConfigOutput{
			DistributionConfig: minimalCFConfig(),
			ETag:               aws.String(cfETag),
		}, nil)
	cfMock.On("UpdateDistribution", mock.Anything, mock.MatchedBy(func(in *cloudfront.UpdateDistributionInput) bool {
		return aws.ToString(in.DistributionConfig.ViewerCertificate.ACMCertificateArn) == cfACMARN &&
			len(in.DistributionConfig.Aliases.Items) == 2
	})).Return(&cloudfront.UpdateDistributionOutput{}, nil)

	r, recorder := buildReconcilerWithCF(t, acmMock, cfMock)
	require.NoError(t, r.Create(context.Background(),
		tlsSecret(map[string]string{
			annotations.Enabled:                   "true",
			annotations.CloudFrontDistributionARN: cfDistARN,
		}, certPEM, keyPEM)))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	cfMock.AssertExpectations(t)

	// Drain events and check CloudFrontSynced is present.
	var evts []string
	for len(recorder.Events) > 0 {
		evts = append(evts, <-recorder.Events)
	}
	assert.True(t, func() bool {
		for _, e := range evts {
			if strings.Contains(e, "CloudFrontSynced") {
				return true
			}
		}
		return false
	}(), "expected CloudFrontSynced event")
}

func TestReconcile_CloudFront_AnnotationOnCertificate(t *testing.T) {
	certPEM, keyPEM := generateCertWithSANs(t, []string{"cert-owner.example.com"})
	acmMock := &acmclient.MockACMAPI{}
	setupCFImportMock(acmMock)
	cfMock := &cloudfrontclient.MockCloudFrontAPI{}
	cfMock.On("GetDistributionConfig", mock.Anything, mock.Anything).
		Return(&cloudfront.GetDistributionConfigOutput{
			DistributionConfig: minimalCFConfig(),
			ETag:               aws.String(cfETag),
		}, nil)
	cfMock.On("UpdateDistribution", mock.Anything, mock.Anything).
		Return(&cloudfront.UpdateDistributionOutput{}, nil)

	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	fakeClient := fake.NewClientBuilder().WithScheme(sc).Build()
	recorder := events.NewFakeRecorder(32)
	r := &controller.SecretReconciler{
		Client:           fakeClient,
		Reader:           fakeClient,
		Recorder:         recorder,
		ACMPool:          &acmclient.MockPool{Client: acmMock},
		DefaultRegion:    "us-east-1",
		CloudFrontClient: cfMock,
	}

	certObj := &unstructured.Unstructured{}
	certObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	certObj.SetName("my-cert")
	certObj.SetNamespace("default")
	certObj.SetAnnotations(map[string]string{
		annotations.Enabled:                   "true",
		annotations.CloudFrontDistributionARN: cfDistARN,
	})
	require.NoError(t, fakeClient.Create(context.Background(), certObj))

	secret := tlsSecret(nil, certPEM, keyPEM)
	secret.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "cert-manager.io/v1",
		Kind:       "Certificate",
		Name:       "my-cert",
	}}
	require.NoError(t, fakeClient.Create(context.Background(), secret))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	cfMock.AssertCalled(t, "UpdateDistribution", mock.Anything, mock.Anything)
}

func TestReconcile_CloudFront_UpdateFails_ReconcileSucceeds(t *testing.T) {
	certPEM, keyPEM := generateCertWithSANs(t, []string{"example.com"})
	acmMock := &acmclient.MockACMAPI{}
	setupCFImportMock(acmMock)
	cfMock := &cloudfrontclient.MockCloudFrontAPI{}
	cfMock.On("GetDistributionConfig", mock.Anything, mock.Anything).
		Return(&cloudfront.GetDistributionConfigOutput{
			DistributionConfig: minimalCFConfig(),
			ETag:               aws.String(cfETag),
		}, nil)
	cfMock.On("UpdateDistribution", mock.Anything, mock.Anything).
		Return(nil, errors.New("precondition failed"))

	r, recorder := buildReconcilerWithCF(t, acmMock, cfMock)
	require.NoError(t, r.Create(context.Background(),
		tlsSecret(map[string]string{
			annotations.Enabled:                   "true",
			annotations.CloudFrontDistributionARN: cfDistARN,
		}, certPEM, keyPEM)))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err, "CloudFront failure must not fail the reconcile")

	var evts []string
	for len(recorder.Events) > 0 {
		evts = append(evts, <-recorder.Events)
	}
	assert.True(t, func() bool {
		for _, e := range evts {
			if strings.Contains(e, "CloudFrontSyncFailed") {
				return true
			}
		}
		return false
	}(), "expected CloudFrontSyncFailed warning event")
}

func TestReconcile_CloudFront_GetConfigFails_ReconcileSucceeds(t *testing.T) {
	certPEM, keyPEM := generateCertWithSANs(t, []string{"example.com"})
	acmMock := &acmclient.MockACMAPI{}
	setupCFImportMock(acmMock)
	cfMock := &cloudfrontclient.MockCloudFrontAPI{}
	cfMock.On("GetDistributionConfig", mock.Anything, mock.Anything).
		Return(nil, errors.New("access denied"))

	r, recorder := buildReconcilerWithCF(t, acmMock, cfMock)
	require.NoError(t, r.Create(context.Background(),
		tlsSecret(map[string]string{
			annotations.Enabled:                   "true",
			annotations.CloudFrontDistributionARN: cfDistARN,
		}, certPEM, keyPEM)))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err, "CloudFront failure must not fail the reconcile")
	cfMock.AssertNotCalled(t, "UpdateDistribution")

	var evts []string
	for len(recorder.Events) > 0 {
		evts = append(evts, <-recorder.Events)
	}
	assert.True(t, func() bool {
		for _, e := range evts {
			if strings.Contains(e, "CloudFrontSyncFailed") {
				return true
			}
		}
		return false
	}(), "expected CloudFrontSyncFailed warning event")
}

func TestReconcile_CloudFront_NoAnnotation_SkipsCF(t *testing.T) {
	certPEM, keyPEM := generateCert(t)
	acmMock := &acmclient.MockACMAPI{}
	setupCFImportMock(acmMock)
	cfMock := &cloudfrontclient.MockCloudFrontAPI{}

	r, _ := buildReconcilerWithCF(t, acmMock, cfMock)
	require.NoError(t, r.Create(context.Background(),
		tlsSecret(map[string]string{annotations.Enabled: "true"}, certPEM, keyPEM)))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	cfMock.AssertNotCalled(t, "GetDistributionConfig")
	cfMock.AssertNotCalled(t, "UpdateDistribution")
}

func TestReconcile_CloudFront_FingerprintUnchanged_SkipsCF(t *testing.T) {
	certPEM, keyPEM := generateCertWithSANs(t, []string{"example.com"})
	fp, err := fingerprint.Compute(certPEM)
	require.NoError(t, err)
	const arn = "arn:aws:acm:us-east-1:123:certificate/existing"

	acmMock := &acmclient.MockACMAPI{}
	acmMock.On("DescribeCertificate", mock.Anything, mock.Anything).
		Return(&awsacm.DescribeCertificateOutput{
			Certificate: &acmtypes.CertificateDetail{CertificateArn: aws.String(arn)},
		}, nil)
	cfMock := &cloudfrontclient.MockCloudFrontAPI{}

	r, _ := buildReconcilerWithCF(t, acmMock, cfMock)
	require.NoError(t, r.Create(context.Background(),
		tlsSecret(map[string]string{
			annotations.Enabled:                   "true",
			annotations.ARN:                       arn,
			annotations.Fingerprint:               fp,
			annotations.CloudFrontDistributionARN: cfDistARN,
		}, certPEM, keyPEM)))

	_, err = r.Reconcile(context.Background(), nn())
	require.NoError(t, err)
	cfMock.AssertNotCalled(t, "GetDistributionConfig")
	cfMock.AssertNotCalled(t, "UpdateDistribution")
}
