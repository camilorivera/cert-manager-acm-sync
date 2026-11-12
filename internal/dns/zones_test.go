package dns_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
)

func TestParseZones(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    dns.ZoneSet
		wantErr bool
	}{
		{name: "name only", in: []string{"example.com"}, want: dns.ZoneSet{{Name: "example.com"}}},
		{name: "name and id", in: []string{"example.com=Z123"}, want: dns.ZoneSet{{Name: "example.com", ID: "Z123"}}},
		{name: "normalised", in: []string{"Example.COM.=Z1"}, want: dns.ZoneSet{{Name: "example.com", ID: "Z1"}}},
		{name: "several", in: []string{"a.com", "b.com=Z2"}, want: dns.ZoneSet{{Name: "a.com"}, {Name: "b.com", ID: "Z2"}}},
		{name: "empty name", in: []string{"=Z1"}, wantErr: true},
		{name: "duplicate", in: []string{"a.com", "A.com."}, wantErr: true},
		{name: "none", in: nil, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dns.ParseZones(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestZoneSetMatch(t *testing.T) {
	zs := dns.ZoneSet{
		{Name: "leadiq.com", ID: "Z1"},
		{Name: "app.leadiq.com", ID: "Z2"},
	}
	tests := []struct {
		name   string
		fqdn   string
		wantID string
		wantOK bool
	}{
		{name: "apex", fqdn: "leadiq.com", wantID: "Z1", wantOK: true},
		{name: "subdomain", fqdn: "www.leadiq.com", wantID: "Z1", wantOK: true},
		{name: "longest suffix wins", fqdn: "x.app.leadiq.com", wantID: "Z2", wantOK: true},
		{name: "nested zone apex", fqdn: "app.leadiq.com", wantID: "Z2", wantOK: true},
		{name: "label boundary", fqdn: "notleadiq.com"},
		{name: "unrelated", fqdn: "example.org"},
		{name: "case and trailing dot", fqdn: "WWW.LeadIQ.com.", wantID: "Z1", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			z, ok := zs.Match(tt.fqdn)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantID, z.ID)
		})
	}
}
