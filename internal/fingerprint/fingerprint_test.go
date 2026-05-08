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

func generateCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return certPEM, keyPEM
}

func TestCompute_ValidPEM(t *testing.T) {
	certPEM, _ := generateCert(t)
	fp, err := fingerprint.Compute(certPEM)
	require.NoError(t, err)
	assert.Len(t, fp, 64) // hex(sha256) = 64 chars
}

func TestCompute_Deterministic(t *testing.T) {
	certPEM, _ := generateCert(t)
	fp1, err := fingerprint.Compute(certPEM)
	require.NoError(t, err)
	fp2, err := fingerprint.Compute(certPEM)
	require.NoError(t, err)
	assert.Equal(t, fp1, fp2)
}

func TestCompute_DifferentCerts(t *testing.T) {
	cert1, _ := generateCert(t)
	cert2, _ := generateCert(t)
	fp1, err := fingerprint.Compute(cert1)
	require.NoError(t, err)
	fp2, err := fingerprint.Compute(cert2)
	require.NoError(t, err)
	assert.NotEqual(t, fp1, fp2)
}

func TestCompute_ChainReturnLeafFingerprint(t *testing.T) {
	leaf, _ := generateCert(t)
	intermediate, _ := generateCert(t)

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
	certPEM, _ := generateCert(t)
	leaf, chain := fingerprint.SplitChain(certPEM)
	assert.NotEmpty(t, leaf)
	assert.Empty(t, chain)
}

func TestSplitChain_FullChain(t *testing.T) {
	leaf, _ := generateCert(t)
	intermediate, _ := generateCert(t)
	full := append(leaf, intermediate...)

	splitLeaf, splitChain := fingerprint.SplitChain(full)
	assert.Equal(t, leaf, splitLeaf)
	assert.Equal(t, intermediate, splitChain)
}
