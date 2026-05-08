package controller_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsacm "github.com/aws/aws-sdk-go-v2/service/acm"
	acmtypes "github.com/aws/aws-sdk-go-v2/service/acm/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	acmclient "github.com/camilorivera/cert-manager-acm-sync/internal/acm"
	"github.com/camilorivera/cert-manager-acm-sync/internal/annotations"
	"github.com/camilorivera/cert-manager-acm-sync/internal/controller"
	"github.com/camilorivera/cert-manager-acm-sync/internal/fingerprint"
)

func generateCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return
}

func buildReconciler(t *testing.T, acmMock *acmclient.MockACMAPI) (*controller.SecretReconciler, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := record.NewFakeRecorder(32)
	return &controller.SecretReconciler{
		Client:        fakeClient,
		Recorder:      recorder,
		ACMPool:       &acmclient.MockPool{Client: acmMock},
		DefaultRegion: "us-east-1",
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

	require.NoError(t, r.Client.Create(context.Background(),
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
	require.NoError(t, r.Client.Create(context.Background(),
		tlsSecret(map[string]string{annotations.Enabled: "true"}, certPEM, keyPEM)))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)

	var updated corev1.Secret
	require.NoError(t, r.Client.Get(context.Background(),
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
	require.NoError(t, r.Client.Create(context.Background(),
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
	require.NoError(t, r.Client.Create(context.Background(),
		tlsSecret(map[string]string{
			annotations.Enabled:     "true",
			annotations.ARN:         staleARN,
			annotations.Fingerprint: "old-fp",
		}, certPEM, keyPEM)))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)

	var updated corev1.Secret
	require.NoError(t, r.Client.Get(context.Background(),
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
	require.NoError(t, r.Client.Create(context.Background(),
		tlsSecret(map[string]string{
			annotations.Enabled:     "true",
			annotations.ARN:         arn,
			annotations.Fingerprint: "old-fp",
		}, certPEM, keyPEM)))

	_, err := r.Reconcile(context.Background(), nn())
	require.NoError(t, err)

	var updated corev1.Secret
	require.NoError(t, r.Client.Get(context.Background(),
		k8stypes.NamespacedName{Name: "s", Namespace: "default"}, &updated))
	assert.Equal(t, arn, updated.Annotations[annotations.ARN], "ARN must not change on renewal")
}
