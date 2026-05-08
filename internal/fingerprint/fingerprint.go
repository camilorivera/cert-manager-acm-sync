package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
)

// Compute returns the hex-encoded SHA-256 of the DER bytes of the leaf
// (first) certificate in certPEM. It changes on every cert renewal.
func Compute(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("no CERTIFICATE PEM block found")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// SplitChain separates the full PEM chain from tls.crt into the leaf cert
// (first PEM block, re-encoded) and the remaining chain (remaining blocks,
// re-encoded). ACM's ImportCertificate API expects them separately.
func SplitChain(certPEM []byte) (leafPEM []byte, chainPEM []byte) {
	rest := certPEM
	first := true
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		encoded := pem.EncodeToMemory(block)
		if first {
			leafPEM = encoded
			first = false
		} else {
			chainPEM = append(chainPEM, encoded...)
		}
	}
	return leafPEM, chainPEM
}
