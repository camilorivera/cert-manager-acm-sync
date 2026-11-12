package dnsimple_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
	"github.com/camilorivera/cert-manager-acm-sync/internal/dnsimple"
)

var zone = dns.Zone{Name: "example.com"}

func TestProvider_Name(t *testing.T) {
	assert.Equal(t, "dnsimple", dnsimple.NewProvider(&dnsimple.MockAPI{}).Name())
}

func TestProvider_GetRecords(t *testing.T) {
	tests := []struct {
		name string
		fqdn string
		rel  string
		api  []dnsimple.Record
		want []dns.Record
	}{
		{
			name: "cname",
			fqdn: "www.example.com",
			rel:  "www",
			api:  []dnsimple.Record{{ID: 1, Name: "www", Type: "CNAME", Content: "D1.CloudFront.net.", TTL: 300}},
			want: []dns.Record{{Name: "www.example.com", Type: dns.TypeCNAME, Target: "d1.cloudfront.net", TTL: 300}},
		},
		{
			name: "apex alias ignores companion txt and system records",
			fqdn: "example.com",
			rel:  "",
			api: []dnsimple.Record{
				{ID: 1, Name: "", Type: "ALIAS", Content: "d1.cloudfront.net", TTL: 300},
				{ID: 2, Name: "", Type: "TXT", Content: "ALIAS for d1.cloudfront.net", ParentID: 1},
				{ID: 3, Name: "", Type: "NS", Content: "ns1.dnsimple.com", SystemRecord: true},
				{ID: 4, Name: "", Type: "SOA", Content: "x", SystemRecord: true},
				{ID: 5, Name: "", Type: "MX", Content: "mail"},
			},
			want: []dns.Record{{Name: "example.com", Type: dns.TypeALIAS, Target: "d1.cloudfront.net", TTL: 300}},
		},
		{
			name: "owner txt quotes stripped",
			fqdn: "_acm-sync.www.example.com",
			rel:  "_acm-sync.www",
			api:  []dnsimple.Record{{ID: 1, Name: "_acm-sync.www", Type: "TXT", Content: `"heritage=x"`, TTL: 300}},
			want: []dns.Record{{Name: "_acm-sync.www.example.com", Type: dns.TypeTXT, Target: "heritage=x", TTL: 300}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &dnsimple.MockAPI{}
			api.On("ListRecords", mock.Anything, "example.com", tt.rel).Return(tt.api, nil)

			got, err := dnsimple.NewProvider(api).GetRecords(context.Background(), zone, tt.fqdn)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestProvider_GetRecords_OutsideZone(t *testing.T) {
	_, err := dnsimple.NewProvider(&dnsimple.MockAPI{}).GetRecords(context.Background(), zone, "www.other.org")
	assert.Error(t, err)
}

func TestProvider_GetRecords_UsesZoneID(t *testing.T) {
	api := &dnsimple.MockAPI{}
	api.On("ListRecords", mock.Anything, "99", "www").Return(nil, nil)

	_, err := dnsimple.NewProvider(api).GetRecords(context.Background(), dns.Zone{Name: "example.com", ID: "99"}, "www.example.com")
	require.NoError(t, err)
	api.AssertExpectations(t)
}

func TestProvider_CreateRecords(t *testing.T) {
	ctx := context.Background()
	cname := dns.Record{Name: "www.example.com", Type: dns.TypeCNAME, Target: "d1.cloudfront.net", TTL: 300}
	txt := dns.Record{Name: "_acm-sync.www.example.com", Type: dns.TypeTXT, Target: "heritage=x", TTL: 300}

	t.Run("creates txt then record", func(t *testing.T) {
		api := &dnsimple.MockAPI{}
		api.On("ListRecords", mock.Anything, "example.com", "_acm-sync.www").Return(nil, nil)
		api.On("ListRecords", mock.Anything, "example.com", "www").Return(nil, nil)
		api.On("CreateRecord", mock.Anything, "example.com", dnsimple.RecordInput{Name: "_acm-sync.www", Type: "TXT", Content: "heritage=x", TTL: 300}).Return(dnsimple.Record{}, nil).Once()
		api.On("CreateRecord", mock.Anything, "example.com", dnsimple.RecordInput{Name: "www", Type: "CNAME", Content: "d1.cloudfront.net", TTL: 300}).Return(dnsimple.Record{}, nil).Once()

		require.NoError(t, dnsimple.NewProvider(api).CreateRecords(ctx, zone, []dns.Record{txt, cname}))
		api.AssertExpectations(t)
	})

	t.Run("apex alias uses empty relative name", func(t *testing.T) {
		api := &dnsimple.MockAPI{}
		api.On("ListRecords", mock.Anything, "example.com", "").Return([]dnsimple.Record{
			{ID: 3, Name: "", Type: "NS", SystemRecord: true},
			{ID: 4, Name: "", Type: "MX", Content: "mail"},
		}, nil)
		api.On("CreateRecord", mock.Anything, "example.com", dnsimple.RecordInput{Name: "", Type: "ALIAS", Content: "d1.cloudfront.net", TTL: 300}).Return(dnsimple.Record{}, nil).Once()

		alias := dns.Record{Name: "example.com", Type: dns.TypeALIAS, Target: "d1.cloudfront.net", TTL: 300}
		require.NoError(t, dnsimple.NewProvider(api).CreateRecords(ctx, zone, []dns.Record{alias}))
		api.AssertExpectations(t)
	})

	t.Run("existing record conflicts and nothing is created", func(t *testing.T) {
		api := &dnsimple.MockAPI{}
		api.On("ListRecords", mock.Anything, "example.com", "_acm-sync.www").Return(nil, nil)
		api.On("ListRecords", mock.Anything, "example.com", "www").Return([]dnsimple.Record{{ID: 1, Name: "www", Type: "A", Content: "1.1.1.1"}}, nil)

		err := dnsimple.NewProvider(api).CreateRecords(ctx, zone, []dns.Record{txt, cname})
		assert.ErrorIs(t, err, dns.ErrAlreadyExists)
		api.AssertNotCalled(t, "CreateRecord", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("existing txt conflicts", func(t *testing.T) {
		api := &dnsimple.MockAPI{}
		api.On("ListRecords", mock.Anything, "example.com", "_acm-sync.www").Return([]dnsimple.Record{{ID: 1, Type: "TXT", Content: "other"}}, nil)

		err := dnsimple.NewProvider(api).CreateRecords(ctx, zone, []dns.Record{txt})
		assert.ErrorIs(t, err, dns.ErrAlreadyExists)
	})

	t.Run("api error propagates", func(t *testing.T) {
		api := &dnsimple.MockAPI{}
		api.On("ListRecords", mock.Anything, "example.com", "www").Return(nil, nil)
		api.On("CreateRecord", mock.Anything, "example.com", mock.Anything).Return(dnsimple.Record{}, dns.ErrRetryable)

		err := dnsimple.NewProvider(api).CreateRecords(ctx, zone, []dns.Record{cname})
		assert.ErrorIs(t, err, dns.ErrRetryable)
	})
}

func TestProvider_UpdateRecord(t *testing.T) {
	ctx := context.Background()
	old := dns.Record{Name: "www.example.com", Type: dns.TypeCNAME, Target: "old.example.net", TTL: 300}
	existing := []dnsimple.Record{
		{ID: 5, Name: "www", Type: "CNAME", Content: "old.example.net.", TTL: 300},
	}

	t.Run("same type patches by id", func(t *testing.T) {
		api := &dnsimple.MockAPI{}
		api.On("ListRecords", mock.Anything, "example.com", "www").Return(existing, nil)
		api.On("UpdateRecord", mock.Anything, "example.com", int64(5), dnsimple.RecordInput{Name: "www", Content: "new.example.net", TTL: 600}).Return(dnsimple.Record{}, nil).Once()

		updated := dns.Record{Name: "www.example.com", Type: dns.TypeCNAME, Target: "new.example.net", TTL: 600}
		require.NoError(t, dnsimple.NewProvider(api).UpdateRecord(ctx, zone, old, updated))
		api.AssertExpectations(t)
	})

	t.Run("type change deletes then creates", func(t *testing.T) {
		api := &dnsimple.MockAPI{}
		api.On("ListRecords", mock.Anything, "example.com", "www").Return(existing, nil)
		api.On("DeleteRecord", mock.Anything, "example.com", int64(5)).Return(nil).Once()
		api.On("CreateRecord", mock.Anything, "example.com", dnsimple.RecordInput{Name: "www", Type: "ALIAS", Content: "new.example.net", TTL: 300}).Return(dnsimple.Record{}, nil).Once()

		updated := dns.Record{Name: "www.example.com", Type: dns.TypeALIAS, Target: "new.example.net", TTL: 300}
		require.NoError(t, dnsimple.NewProvider(api).UpdateRecord(ctx, zone, old, updated))
		api.AssertExpectations(t)
	})

	t.Run("missing record errors", func(t *testing.T) {
		api := &dnsimple.MockAPI{}
		api.On("ListRecords", mock.Anything, "example.com", "www").Return(nil, nil)

		err := dnsimple.NewProvider(api).UpdateRecord(ctx, zone, old, old)
		assert.Error(t, err)
	})
}

func TestProvider_DeleteRecords(t *testing.T) {
	ctx := context.Background()
	cname := dns.Record{Name: "www.example.com", Type: dns.TypeCNAME, Target: "d1.cloudfront.net", TTL: 300}
	txt := dns.Record{Name: "_acm-sync.www.example.com", Type: dns.TypeTXT, Target: "heritage=x", TTL: 300}

	t.Run("deletes by looked-up id and skips missing", func(t *testing.T) {
		api := &dnsimple.MockAPI{}
		api.On("ListRecords", mock.Anything, "example.com", "www").Return([]dnsimple.Record{
			{ID: 8, Name: "www", Type: "CNAME", Content: "d1.cloudfront.net"},
			{ID: 9, Name: "www", Type: "CNAME", Content: "other.example.net"},
		}, nil)
		api.On("ListRecords", mock.Anything, "example.com", "_acm-sync.www").Return(nil, nil)
		api.On("DeleteRecord", mock.Anything, "example.com", int64(8)).Return(nil).Once()

		require.NoError(t, dnsimple.NewProvider(api).DeleteRecords(ctx, zone, []dns.Record{cname, txt}))
		api.AssertExpectations(t)
	})

	t.Run("not found on delete is ignored", func(t *testing.T) {
		api := &dnsimple.MockAPI{}
		api.On("ListRecords", mock.Anything, "example.com", "www").Return([]dnsimple.Record{{ID: 8, Name: "www", Type: "CNAME", Content: "d1.cloudfront.net"}}, nil)
		api.On("DeleteRecord", mock.Anything, "example.com", int64(8)).Return(&dnsimple.APIError{Status: http.StatusNotFound})

		require.NoError(t, dnsimple.NewProvider(api).DeleteRecords(ctx, zone, []dns.Record{cname}))
	})

	t.Run("delete error propagates", func(t *testing.T) {
		api := &dnsimple.MockAPI{}
		api.On("ListRecords", mock.Anything, "example.com", "www").Return([]dnsimple.Record{{ID: 8, Name: "www", Type: "CNAME", Content: "d1.cloudfront.net"}}, nil)
		api.On("DeleteRecord", mock.Anything, "example.com", int64(8)).Return(errors.New("boom"))

		assert.Error(t, dnsimple.NewProvider(api).DeleteRecords(ctx, zone, []dns.Record{cname}))
	})
}
