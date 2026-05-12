package fingerprint_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/camilorivera/cert-manager-acm-sync/internal/fingerprint"
)

func generateCert(t *testing.T) []byte {
	t.Helper()
	return generateCertWithSANs(t, nil)
}

func generateCertWithSANs(t *testing.T, dnsNames []string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestCompute_ValidPEM(t *testing.T) {
	certPEM := generateCert(t)
	fp, err := fingerprint.Compute(certPEM)
	require.NoError(t, err)
	assert.Len(t, fp, 64) // hex(sha256) = 64 chars
}

func TestCompute_Deterministic(t *testing.T) {
	certPEM := generateCert(t)
	fp1, err := fingerprint.Compute(certPEM)
	require.NoError(t, err)
	fp2, err := fingerprint.Compute(certPEM)
	require.NoError(t, err)
	assert.Equal(t, fp1, fp2)
}

func TestCompute_DifferentCerts(t *testing.T) {
	cert1 := generateCert(t)
	cert2 := generateCert(t)
	fp1, err := fingerprint.Compute(cert1)
	require.NoError(t, err)
	fp2, err := fingerprint.Compute(cert2)
	require.NoError(t, err)
	assert.NotEqual(t, fp1, fp2)
}

func TestCompute_ChainReturnLeafFingerprint(t *testing.T) {
	leaf := generateCert(t)
	intermediate := generateCert(t)

	// tls.crt with full chain: leaf + intermediate
	chain := append(leaf, intermediate...)

	fpChain, err := fingerprint.Compute(chain)
	require.NoError(t, err)
	fpLeaf, err := fingerprint.Compute(leaf)
	require.NoError(t, err)

	assert.Equal(t, fpLeaf, fpChain, "fingerprint should be of the leaf cert only")
}

func TestCompute_EmptyInput(t *testing.T) {
	_, err := fingerprint.Compute([]byte{})
	assert.Error(t, err)
}

func TestCompute_InvalidPEM(t *testing.T) {
	_, err := fingerprint.Compute([]byte("not valid pem"))
	assert.Error(t, err)
}

func TestSplitChain_SingleCert(t *testing.T) {
	certPEM := generateCert(t)
	leaf, chain := fingerprint.SplitChain(certPEM)
	assert.NotEmpty(t, leaf)
	assert.Empty(t, chain)
}

func TestExtractSANs_WithDNSNames(t *testing.T) {
	certPEM := generateCertWithSANs(t, []string{"example.com", "www.example.com"})
	sans, err := fingerprint.ExtractSANs(certPEM)
	require.NoError(t, err)
	assert.Equal(t, []string{"example.com", "www.example.com"}, sans)
}

func TestExtractSANs_FallbackToCommonName(t *testing.T) {
	certPEM := generateCertWithSANs(t, nil) // no SANs, only CN
	sans, err := fingerprint.ExtractSANs(certPEM)
	require.NoError(t, err)
	assert.Equal(t, []string{"test.example.com"}, sans)
}

func TestExtractSANs_LeafOnlyFromChain(t *testing.T) {
	leaf := generateCertWithSANs(t, []string{"leaf.example.com"})
	intermediate := generateCertWithSANs(t, []string{"intermediate.example.com"})
	chain := append(leaf, intermediate...)
	sans, err := fingerprint.ExtractSANs(chain)
	require.NoError(t, err)
	assert.Equal(t, []string{"leaf.example.com"}, sans)
}

func TestExtractSANs_EmptyInput(t *testing.T) {
	_, err := fingerprint.ExtractSANs([]byte{})
	assert.Error(t, err)
}

func TestSplitChain_FullChain(t *testing.T) {
	leaf := generateCert(t)
	intermediate := generateCert(t)
	full := append(leaf, intermediate...)

	splitLeaf, splitChain := fingerprint.SplitChain(full)
	assert.Equal(t, leaf, splitLeaf)
	assert.Equal(t, intermediate, splitChain)
}
