package dns_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
)

func TestOwnerName(t *testing.T) {
	tests := []struct {
		fqdn string
		want string
	}{
		{"www.example.com", "_acm-sync.www.example.com"},
		{"example.com", "_acm-sync.example.com"},
		{"*.example.com", "_acm-sync._wildcard.example.com"},
		{"WWW.Example.com.", "_acm-sync.www.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.fqdn, func(t *testing.T) {
			assert.Equal(t, tt.want, dns.OwnerName(tt.fqdn))
		})
	}
}

func TestOwnerRoundTrip(t *testing.T) {
	v := dns.EncodeOwner("prod", "secret/ns/name")
	assert.Equal(t, "heritage=cert-manager-acm-sync,owner=prod,resource=secret/ns/name", v)

	for _, in := range []string{v, `"` + v + `"`, " " + v + " "} {
		id, res, ok := dns.ParseOwner(in)
		assert.True(t, ok)
		assert.Equal(t, "prod", id)
		assert.Equal(t, "secret/ns/name", res)
	}
}

func TestParseOwnerInvalid(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"foreign heritage", "heritage=external-dns,owner=a,resource=b"},
		{"missing owner", "heritage=cert-manager-acm-sync,resource=b"},
		{"missing resource", "heritage=cert-manager-acm-sync,owner=a"},
		{"spf", "v=spf1 include:_spf.example.com ~all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, ok := dns.ParseOwner(tt.in)
			assert.False(t, ok)
		})
	}
}
