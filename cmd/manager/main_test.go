package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
)

func validFlags() dnsFlags {
	return dnsFlags{
		provider:       "route53",
		zones:          "example.com=Z1",
		ownerID:        "prod-us-east-1",
		defaultTTL:     300,
		defaultType:    "CNAME",
		wildcardPolicy: dns.WildcardSkip,
		deletePolicy:   dns.DeleteRetain,
	}
}

func TestBuildDNS_Disabled(t *testing.T) {
	syncer, _, err := buildDNS(dnsFlags{}, aws.Config{})
	require.NoError(t, err)
	assert.Nil(t, syncer)
}

func TestBuildDNS_OwnerID(t *testing.T) {
	tests := []struct {
		name    string
		owner   string
		wantErr bool
	}{
		{"valid", "prod-us-east-1", false},
		{"empty", "", true},
		{"default", "default", true},
		{"comma", "a,b", true},
		{"equals", "a=b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := validFlags()
			f.ownerID = tt.owner

			syncer, _, err := buildDNS(f, aws.Config{})

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "--dns-owner-id")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.owner, syncer.OwnerID)
		})
	}
}
