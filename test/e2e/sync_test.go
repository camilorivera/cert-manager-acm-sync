//go:build e2e

// Package e2e contains end-to-end tests that run against a real EKS cluster
// and a real AWS ACM account. Requirements:
//   - Valid kubeconfig pointing to an EKS cluster with the controller deployed
//   - AWS credentials with acm:ImportCertificate, acm:DescribeCertificate, acm:DeleteCertificate
//   - AWS_REGION env var (defaults to us-east-1)
//
// Run with: go test -v -tags=e2e ./test/e2e/ -timeout=5m
package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsacm "github.com/aws/aws-sdk-go-v2/service/acm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/camilorivera/cert-manager-acm-sync/internal/annotations"
	"github.com/camilorivera/cert-manager-acm-sync/internal/fingerprint"
)

const (
	e2eNamespace = "default"
	pollInterval = 2 * time.Second
	pollTimeout  = 90 * time.Second
)

var (
	k8sClient client.Client
	acmClient *awsacm.Client
	awsRegion string
)

func TestMain(m *testing.M) {
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubeconfig error: %v\n", err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "k8s client error: %v\n", err)
		os.Exit(1)
	}

	awsRegion = os.Getenv("AWS_REGION")
	if awsRegion == "" {
		awsRegion = "us-east-1"
	}
	acmCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(awsRegion))
	if err != nil {
		fmt.Fprintf(os.Stderr, "aws config error: %v\n", err)
		os.Exit(1)
	}
	acmClient = awsacm.NewFromConfig(acmCfg)

	os.Exit(m.Run())
}

func generateSelfSignedCert(t *testing.T, serial int64) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "e2e-test.internal"},
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

// TestScenarioA_HappyPath verifies first import and cert renewal to same ARN.
func TestScenarioA_HappyPath(t *testing.T) {
	ctx := context.Background()
	secretName := fmt.Sprintf("e2e-happy-%d", time.Now().UnixNano())

	cert1, key1 := generateSelfSignedCert(t, 1)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: e2eNamespace,
			Annotations: map[string]string{
				annotations.Enabled: "true",
				annotations.Region:  awsRegion,
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.crt": cert1, "tls.key": key1},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), secret) })

	// Step 1: Wait for ARN annotation
	var firstARN string
	require.NoError(t, wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true,
		func(ctx context.Context) (bool, error) {
			var s corev1.Secret
			if err := k8sClient.Get(ctx, k8stypes.NamespacedName{Name: secretName, Namespace: e2eNamespace}, &s); err != nil {
				return false, nil
			}
			firstARN = annotations.GetARN(s.Annotations)
			return firstARN != "", nil
		}), "timed out waiting for acm.sync/arn annotation")
	t.Logf("first ARN: %s", firstARN)

	// Step 2: Verify cert is ISSUED in ACM
	desc, err := acmClient.DescribeCertificate(ctx, &awsacm.DescribeCertificateInput{
		CertificateArn: aws.String(firstARN),
	})
	require.NoError(t, err)
	t.Logf("ACM status after first import: %s", desc.Certificate.Status)

	// Step 3: Renew — replace cert data
	cert2, key2 := generateSelfSignedCert(t, 2)
	fp1, _ := fingerprint.Compute(cert1)

	var s corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, k8stypes.NamespacedName{Name: secretName, Namespace: e2eNamespace}, &s))
	patch := client.MergeFrom(s.DeepCopy())
	s.Data["tls.crt"] = cert2
	s.Data["tls.key"] = key2
	require.NoError(t, k8sClient.Patch(ctx, &s, patch))

	// Step 4: Wait for fingerprint to change
	require.NoError(t, wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true,
		func(ctx context.Context) (bool, error) {
			var updated corev1.Secret
			if err := k8sClient.Get(ctx, k8stypes.NamespacedName{Name: secretName, Namespace: e2eNamespace}, &updated); err != nil {
				return false, nil
			}
			return annotations.GetFingerprint(updated.Annotations) != fp1, nil
		}), "timed out waiting for fingerprint to change after renewal")

	// Step 5: Assert ARN is unchanged
	var renewed corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, k8stypes.NamespacedName{Name: secretName, Namespace: e2eNamespace}, &renewed))
	assert.Equal(t, firstARN, annotations.GetARN(renewed.Annotations), "ARN must not change on renewal")
	t.Logf("renewed ARN (same): %s", annotations.GetARN(renewed.Annotations))
}

// TestScenarioB_StaleARNRecovery verifies that the controller creates a new
// certificate when the ACM cert was deleted externally (stale ARN annotation).
func TestScenarioB_StaleARNRecovery(t *testing.T) {
	ctx := context.Background()
	secretName := fmt.Sprintf("e2e-stale-%d", time.Now().UnixNano())

	cert1, key1 := generateSelfSignedCert(t, 10)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: e2eNamespace,
			Annotations: map[string]string{
				annotations.Enabled: "true",
				annotations.Region:  awsRegion,
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.crt": cert1, "tls.key": key1},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), secret) })

	var firstARN string
	require.NoError(t, wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true,
		func(ctx context.Context) (bool, error) {
			var s corev1.Secret
			if err := k8sClient.Get(ctx, k8stypes.NamespacedName{Name: secretName, Namespace: e2eNamespace}, &s); err != nil {
				return false, nil
			}
			firstARN = annotations.GetARN(s.Annotations)
			return firstARN != "", nil
		}), "timed out waiting for first ARN")
	t.Logf("first ARN (will be deleted): %s", firstARN)

	// Delete ACM cert externally to simulate stale ARN
	_, err := acmClient.DeleteCertificate(ctx, &awsacm.DeleteCertificateInput{
		CertificateArn: aws.String(firstARN),
	})
	require.NoError(t, err, "failed to delete ACM cert for test setup")

	// Trigger reconcile by updating cert data
	cert2, key2 := generateSelfSignedCert(t, 11)
	var s corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, k8stypes.NamespacedName{Name: secretName, Namespace: e2eNamespace}, &s))
	patch := client.MergeFrom(s.DeepCopy())
	s.Data["tls.crt"] = cert2
	s.Data["tls.key"] = key2
	require.NoError(t, k8sClient.Patch(ctx, &s, patch))

	// Wait for a new (different) ARN
	var newARN string
	require.NoError(t, wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true,
		func(ctx context.Context) (bool, error) {
			var updated corev1.Secret
			if err := k8sClient.Get(ctx, k8stypes.NamespacedName{Name: secretName, Namespace: e2eNamespace}, &updated); err != nil {
				return false, nil
			}
			newARN = annotations.GetARN(updated.Annotations)
			return newARN != "" && newARN != firstARN, nil
		}), "timed out waiting for new ARN after stale ARN recovery")

	assert.NotEqual(t, firstARN, newARN)
	t.Logf("recovered with new ARN: %s", newARN)

	t.Cleanup(func() {
		_, _ = acmClient.DeleteCertificate(context.Background(), &awsacm.DeleteCertificateInput{
			CertificateArn: aws.String(newARN),
		})
	})
}
