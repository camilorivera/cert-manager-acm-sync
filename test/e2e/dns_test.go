//go:build e2e

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
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/camilorivera/cert-manager-acm-sync/internal/annotations"
	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
	"github.com/camilorivera/cert-manager-acm-sync/internal/dnsimple"
	route53client "github.com/camilorivera/cert-manager-acm-sync/internal/route53"
)

// E2E_DNS_OWNER_ID and the zone must match the deployed controller's --dns-owner-id and --dns-zones.
func dnsProviderFromEnv(t *testing.T) (dns.Provider, dns.Zone) {
	t.Helper()
	zoneName := os.Getenv("E2E_DNS_ZONE")
	if zoneName == "" {
		t.Skip("E2E_DNS_ZONE not set")
	}
	zone := dns.Zone{Name: zoneName, ID: os.Getenv("E2E_DNS_ZONE_ID")}

	switch p := os.Getenv("E2E_DNS_PROVIDER"); p {
	case "", "route53":
		if zone.ID == "" {
			t.Skip("E2E_DNS_ZONE_ID not set")
		}
		cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(awsRegion))
		require.NoError(t, err)
		return route53client.NewProvider(route53client.NewClient(cfg)), zone
	case "dnsimple":
		token, account := os.Getenv("DNSIMPLE_TOKEN"), os.Getenv("E2E_DNSIMPLE_ACCOUNT_ID")
		if token == "" || account == "" {
			t.Skip("DNSIMPLE_TOKEN or E2E_DNSIMPLE_ACCOUNT_ID not set")
		}
		api := dnsimple.NewClient("https://api.dnsimple.com", account, token, &http.Client{Timeout: 30 * time.Second})
		return dnsimple.NewProvider(api), zone
	default:
		t.Skipf("unsupported E2E_DNS_PROVIDER %q", p)
		return nil, zone
	}
}

func generateCertWithSANs(t *testing.T, serial int64, names ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
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

func TestScenarioC_DNSRecords(t *testing.T) {
	provider, zone := dnsProviderFromEnv(t)
	ctx := context.Background()

	const target = "e2e-target.example.net"
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	ownedName := fmt.Sprintf("e2e-owned-%s.%s", suffix, zone.Name)
	foreignName := fmt.Sprintf("e2e-foreign-%s.%s", suffix, zone.Name)
	secretName := fmt.Sprintf("e2e-dns-%s", suffix)

	seeded := dns.Record{Name: foreignName, Type: dns.TypeCNAME, Target: "unrelated.example.org", TTL: 300}
	require.NoError(t, provider.CreateRecords(ctx, zone, []dns.Record{seeded}))
	t.Cleanup(func() { _ = provider.DeleteRecords(context.Background(), zone, []dns.Record{seeded}) })

	certPEM, keyPEM := generateCertWithSANs(t, 20, ownedName, foreignName)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: e2eNamespace,
			Annotations: map[string]string{
				annotations.Enabled:       "true",
				annotations.Region:        awsRegion,
				annotations.DNSEnabled:    "true",
				annotations.DNSTarget:     target,
				annotations.DNSRecordType: "CNAME",
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))
	t.Cleanup(func() {
		recs, _ := provider.GetRecords(context.Background(), zone, ownedName)
		txts, _ := provider.GetRecords(context.Background(), zone, dns.OwnerName(ownedName))
		_ = provider.DeleteRecords(context.Background(), zone, append(recs, txts...))
		_ = k8sClient.Delete(context.Background(), secret)
	})

	require.NoError(t, wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true,
		func(ctx context.Context) (bool, error) {
			var s corev1.Secret
			if err := k8sClient.Get(ctx, k8stypes.NamespacedName{Name: secretName, Namespace: e2eNamespace}, &s); err != nil {
				return false, nil
			}
			return strings.Contains(s.Annotations[annotations.DNSRecords], ownedName), nil
		}), "timed out waiting for acm.sync/dns-records annotation")

	recs, err := provider.GetRecords(ctx, zone, ownedName)
	require.NoError(t, err)
	var found bool
	for _, r := range recs {
		if r.Type == dns.TypeCNAME && strings.TrimSuffix(r.Target, ".") == target {
			found = true
		}
	}
	assert.True(t, found, "expected CNAME %s -> %s, got %+v", ownedName, target, recs)

	txts, err := provider.GetRecords(ctx, zone, dns.OwnerName(ownedName))
	require.NoError(t, err)
	var owner bool
	for _, r := range txts {
		if r.Type != dns.TypeTXT {
			continue
		}
		_, resource, ok := dns.ParseOwner(r.Target)
		owner = owner || (ok && resource == fmt.Sprintf("secret/%s/%s", e2eNamespace, secretName))
	}
	assert.True(t, owner, "expected owner TXT at %s, got %+v", dns.OwnerName(ownedName), txts)

	foreign, err := provider.GetRecords(ctx, zone, foreignName)
	require.NoError(t, err)
	require.Len(t, foreign, 1, "pre-seeded record must remain the only record at %s", foreignName)
	assert.Equal(t, "unrelated.example.org", strings.TrimSuffix(foreign[0].Target, "."))
	foreignTXT, err := provider.GetRecords(ctx, zone, dns.OwnerName(foreignName))
	require.NoError(t, err)
	assert.Empty(t, foreignTXT, "no owner TXT may be written for an unowned conflicting record")
}
