package route53client_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
	route53client "github.com/camilorivera/cert-manager-acm-sync/internal/route53"
)

var zone = dns.Zone{Name: "example.com", ID: "ZONE1"}

func listOut(sets ...r53types.ResourceRecordSet) *route53.ListResourceRecordSetsOutput {
	return &route53.ListResourceRecordSetsOutput{ResourceRecordSets: sets}
}

func rrSet(name string, t r53types.RRType, ttl int64, values ...string) r53types.ResourceRecordSet {
	rs := r53types.ResourceRecordSet{Name: aws.String(name), Type: t, TTL: aws.Int64(ttl)}
	for _, v := range values {
		rs.ResourceRecords = append(rs.ResourceRecords, r53types.ResourceRecord{Value: aws.String(v)})
	}
	return rs
}

func aliasSet(t r53types.RRType, target string) r53types.ResourceRecordSet {
	return r53types.ResourceRecordSet{
		Name:        aws.String("example.com."),
		Type:        t,
		AliasTarget: &r53types.AliasTarget{DNSName: aws.String(target), HostedZoneId: aws.String("Z2FDTNDATAQYW2")},
	}
}

func captureChange(m *route53client.MockRoute53API, err error) **route53.ChangeResourceRecordSetsInput {
	var got *route53.ChangeResourceRecordSetsInput
	m.On("ChangeResourceRecordSets", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { got = args.Get(1).(*route53.ChangeResourceRecordSetsInput) }).
		Return(&route53.ChangeResourceRecordSetsOutput{}, err).Once()
	return &got
}

func summarize(in *route53.ChangeResourceRecordSetsInput) []string {
	var out []string
	for _, c := range in.ChangeBatch.Changes {
		out = append(out, string(c.Action)+" "+string(c.ResourceRecordSet.Type)+" "+aws.ToString(c.ResourceRecordSet.Name))
	}
	return out
}

func TestProvider_Name(t *testing.T) {
	assert.Equal(t, "route53", route53client.NewProvider(&route53client.MockRoute53API{}).Name())
}

func TestProvider_GetRecords(t *testing.T) {
	tests := []struct {
		name string
		fqdn string
		sets []r53types.ResourceRecordSet
		want []dns.Record
	}{
		{
			name: "cname normalised",
			fqdn: "WWW.Example.com",
			sets: []r53types.ResourceRecordSet{rrSet("www.example.com.", r53types.RRTypeCname, 300, "D1.CloudFront.net.")},
			want: []dns.Record{{Name: "www.example.com", Type: dns.TypeCNAME, Target: "d1.cloudfront.net", TTL: 300}},
		},
		{
			name: "alias pair collapses to one ALIAS with TTL 0",
			fqdn: "example.com",
			sets: []r53types.ResourceRecordSet{
				aliasSet(r53types.RRTypeA, "d1.cloudfront.net."),
				aliasSet(r53types.RRTypeAaaa, "d1.cloudfront.net."),
			},
			want: []dns.Record{{Name: "example.com", Type: dns.TypeALIAS, Target: "d1.cloudfront.net"}},
		},
		{
			name: "alias pair with different targets stays two records",
			fqdn: "example.com",
			sets: []r53types.ResourceRecordSet{
				aliasSet(r53types.RRTypeA, "a.cloudfront.net."),
				aliasSet(r53types.RRTypeAaaa, "b.cloudfront.net."),
			},
			want: []dns.Record{
				{Name: "example.com", Type: dns.TypeALIAS, Target: "a.cloudfront.net"},
				{Name: "example.com", Type: dns.TypeALIAS, Target: "b.cloudfront.net"},
			},
		},
		{
			name: "plain A and AAAA returned as is, NS and SOA ignored",
			fqdn: "example.com",
			sets: []r53types.ResourceRecordSet{
				rrSet("example.com.", r53types.RRTypeA, 60, "1.1.1.1", "2.2.2.2"),
				rrSet("example.com.", r53types.RRTypeAaaa, 60, "::1"),
				rrSet("example.com.", r53types.RRTypeNs, 172800, "ns-1.awsdns.com."),
				rrSet("example.com.", r53types.RRTypeSoa, 900, "x"),
			},
			want: []dns.Record{
				{Name: "example.com", Type: "A", Target: "1.1.1.1", TTL: 60},
				{Name: "example.com", Type: "A", Target: "2.2.2.2", TTL: 60},
				{Name: "example.com", Type: "AAAA", Target: "::1", TTL: 60},
			},
		},
		{
			name: "txt unquoted",
			fqdn: "_acm-sync.www.example.com",
			sets: []r53types.ResourceRecordSet{rrSet("_acm-sync.www.example.com.", r53types.RRTypeTxt, 300, `"heritage=x,owner=y"`)},
			want: []dns.Record{{Name: "_acm-sync.www.example.com", Type: dns.TypeTXT, Target: "heritage=x,owner=y", TTL: 300}},
		},
		{
			name: "wildcard escape decoded",
			fqdn: "*.example.com",
			sets: []r53types.ResourceRecordSet{rrSet(`\052.example.com.`, r53types.RRTypeCname, 300, "t.example.net")},
			want: []dns.Record{{Name: "*.example.com", Type: dns.TypeCNAME, Target: "t.example.net", TTL: 300}},
		},
		{
			name: "following name ignored",
			fqdn: "www.example.com",
			sets: []r53types.ResourceRecordSet{rrSet("www2.example.com.", r53types.RRTypeCname, 300, "t.example.net")},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &route53client.MockRoute53API{}
			api.On("ListResourceRecordSets", mock.Anything, mock.Anything).Return(listOut(tt.sets...), nil)

			got, err := route53client.NewProvider(api).GetRecords(context.Background(), zone, tt.fqdn)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestProvider_GetRecords_RequestAndPagination(t *testing.T) {
	api := &route53client.MockRoute53API{}
	first := listOut(rrSet("www.example.com.", r53types.RRTypeA, 60, "1.1.1.1"))
	first.IsTruncated = true
	first.NextRecordName = aws.String("www.example.com.")
	first.NextRecordType = r53types.RRTypeAaaa
	api.On("ListResourceRecordSets", mock.Anything, mock.MatchedBy(func(in *route53.ListResourceRecordSetsInput) bool {
		return aws.ToString(in.HostedZoneId) == "ZONE1" && aws.ToString(in.StartRecordName) == "www.example.com." && in.StartRecordType == ""
	})).Return(first, nil).Once()
	api.On("ListResourceRecordSets", mock.Anything, mock.MatchedBy(func(in *route53.ListResourceRecordSetsInput) bool {
		return in.StartRecordType == r53types.RRTypeAaaa
	})).Return(listOut(rrSet("www.example.com.", r53types.RRTypeAaaa, 60, "::1")), nil).Once()

	got, err := route53client.NewProvider(api).GetRecords(context.Background(), zone, "www.example.com")
	require.NoError(t, err)
	assert.Len(t, got, 2)
	api.AssertExpectations(t)
}

func TestProvider_GetRecords_WildcardStartName(t *testing.T) {
	api := &route53client.MockRoute53API{}
	api.On("ListResourceRecordSets", mock.Anything, mock.MatchedBy(func(in *route53.ListResourceRecordSetsInput) bool {
		return aws.ToString(in.StartRecordName) == `\052.example.com.`
	})).Return(listOut(), nil)

	_, err := route53client.NewProvider(api).GetRecords(context.Background(), zone, "*.example.com")
	require.NoError(t, err)
	api.AssertExpectations(t)
}

func TestProvider_CreateRecords(t *testing.T) {
	txt := dns.Record{Name: "_acm-sync.www.example.com", Type: dns.TypeTXT, Target: "heritage=x", TTL: 300}
	cname := dns.Record{Name: "www.example.com", Type: dns.TypeCNAME, Target: "d1.cloudfront.net", TTL: 300}

	t.Run("single batch with CREATE and quoted txt", func(t *testing.T) {
		api := &route53client.MockRoute53API{}
		got := captureChange(api, nil)

		require.NoError(t, route53client.NewProvider(api).CreateRecords(context.Background(), zone, []dns.Record{txt, cname}))
		api.AssertNumberOfCalls(t, "ChangeResourceRecordSets", 1)
		assert.Equal(t, "ZONE1", aws.ToString((*got).HostedZoneId))
		assert.Equal(t, []string{"CREATE TXT _acm-sync.www.example.com.", "CREATE CNAME www.example.com."}, summarize(*got))
		ch := (*got).ChangeBatch.Changes
		assert.Equal(t, `"heritage=x"`, aws.ToString(ch[0].ResourceRecordSet.ResourceRecords[0].Value))
		assert.Equal(t, "d1.cloudfront.net", aws.ToString(ch[1].ResourceRecordSet.ResourceRecords[0].Value))
		assert.Equal(t, int64(300), aws.ToInt64(ch[1].ResourceRecordSet.TTL))
	})

	t.Run("alias creates A and AAAA with cloudfront zone", func(t *testing.T) {
		api := &route53client.MockRoute53API{}
		got := captureChange(api, nil)

		alias := dns.Record{Name: "example.com", Type: dns.TypeALIAS, Target: "d1.cloudfront.net", TTL: 300}
		require.NoError(t, route53client.NewProvider(api).CreateRecords(context.Background(), zone, []dns.Record{alias}))
		assert.Equal(t, []string{"CREATE A example.com.", "CREATE AAAA example.com."}, summarize(*got))
		for _, c := range (*got).ChangeBatch.Changes {
			assert.Equal(t, "Z2FDTNDATAQYW2", aws.ToString(c.ResourceRecordSet.AliasTarget.HostedZoneId))
			assert.Equal(t, "d1.cloudfront.net", aws.ToString(c.ResourceRecordSet.AliasTarget.DNSName))
			assert.Nil(t, c.ResourceRecordSet.TTL)
		}
	})

	t.Run("alias inside zone uses zone id", func(t *testing.T) {
		api := &route53client.MockRoute53API{}
		got := captureChange(api, nil)

		alias := dns.Record{Name: "example.com", Type: dns.TypeALIAS, Target: "lb.example.com", TTL: 300}
		require.NoError(t, route53client.NewProvider(api).CreateRecords(context.Background(), zone, []dns.Record{alias}))
		assert.Equal(t, "ZONE1", aws.ToString((*got).ChangeBatch.Changes[0].ResourceRecordSet.AliasTarget.HostedZoneId))
	})

	t.Run("alias to foreign target is unsupported", func(t *testing.T) {
		api := &route53client.MockRoute53API{}

		alias := dns.Record{Name: "example.com", Type: dns.TypeALIAS, Target: "lb.elsewhere.org", TTL: 300}
		err := route53client.NewProvider(api).CreateRecords(context.Background(), zone, []dns.Record{alias})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported alias target")
		api.AssertNotCalled(t, "ChangeResourceRecordSets", mock.Anything, mock.Anything)
	})

	t.Run("unsupported record type", func(t *testing.T) {
		err := route53client.NewProvider(&route53client.MockRoute53API{}).CreateRecords(context.Background(), zone, []dns.Record{{Name: "x.example.com", Type: "MX"}})
		assert.Error(t, err)
	})
}

func TestProvider_ErrorMapping(t *testing.T) {
	rec := dns.Record{Name: "www.example.com", Type: dns.TypeCNAME, Target: "t.example.net", TTL: 300}
	tests := []struct {
		name          string
		err           error
		wantExists    bool
		wantRetryable bool
	}{
		{
			name:       "already exists",
			err:        &r53types.InvalidChangeBatch{Message: aws.String("[Tried to create resource record set [name='www.example.com.', type='CNAME'] but it already exists]")},
			wantExists: true,
		},
		{
			name:       "already exists in messages",
			err:        &r53types.InvalidChangeBatch{Messages: []string{"Tried to create resource record set but it already exists"}},
			wantExists: true,
		},
		{name: "other invalid batch", err: &r53types.InvalidChangeBatch{Message: aws.String("bad value")}},
		{name: "throttling", err: &smithy.GenericAPIError{Code: "Throttling", Message: "Rate exceeded"}, wantRetryable: true},
		{name: "prior request", err: &r53types.PriorRequestNotComplete{Message: aws.String("busy")}, wantRetryable: true},
		{name: "access denied", err: &smithy.GenericAPIError{Code: "AccessDenied"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &route53client.MockRoute53API{}
			captureChange(api, tt.err)

			err := route53client.NewProvider(api).CreateRecords(context.Background(), zone, []dns.Record{rec})
			require.Error(t, err)
			assert.Equal(t, tt.wantExists, errors.Is(err, dns.ErrAlreadyExists))
			assert.Equal(t, tt.wantRetryable, errors.Is(err, dns.ErrRetryable))
		})
	}
}

func TestProvider_UpdateRecord(t *testing.T) {
	old := dns.Record{Name: "www.example.com", Type: dns.TypeCNAME, Target: "old.example.net", TTL: 300}

	t.Run("same type upserts in one batch without listing", func(t *testing.T) {
		api := &route53client.MockRoute53API{}
		got := captureChange(api, nil)

		updated := dns.Record{Name: "www.example.com", Type: dns.TypeCNAME, Target: "new.example.net", TTL: 600}
		require.NoError(t, route53client.NewProvider(api).UpdateRecord(context.Background(), zone, old, updated))
		assert.Equal(t, []string{"UPSERT CNAME www.example.com."}, summarize(*got))
		api.AssertNotCalled(t, "ListResourceRecordSets", mock.Anything, mock.Anything)
	})

	t.Run("type change deletes old and upserts new in one batch", func(t *testing.T) {
		api := &route53client.MockRoute53API{}
		api.On("ListResourceRecordSets", mock.Anything, mock.Anything).Return(listOut(rrSet("www.example.com.", r53types.RRTypeCname, 300, "old.example.net.")), nil)
		got := captureChange(api, nil)

		updated := dns.Record{Name: "www.example.com", Type: dns.TypeALIAS, Target: "lb.example.com"}
		require.NoError(t, route53client.NewProvider(api).UpdateRecord(context.Background(), zone, old, updated))
		assert.Equal(t, []string{"DELETE CNAME www.example.com.", "UPSERT A www.example.com.", "UPSERT AAAA www.example.com."}, summarize(*got))
		assert.Equal(t, "old.example.net.", aws.ToString((*got).ChangeBatch.Changes[0].ResourceRecordSet.ResourceRecords[0].Value))
	})
}

func TestProvider_DeleteRecords(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes exact existing values", func(t *testing.T) {
		api := &route53client.MockRoute53API{}
		api.On("ListResourceRecordSets", mock.Anything, mock.MatchedBy(func(in *route53.ListResourceRecordSetsInput) bool {
			return aws.ToString(in.StartRecordName) == "www.example.com."
		})).Return(listOut(rrSet("www.example.com.", r53types.RRTypeCname, 300, "D1.cloudfront.net.")), nil)
		api.On("ListResourceRecordSets", mock.Anything, mock.MatchedBy(func(in *route53.ListResourceRecordSetsInput) bool {
			return aws.ToString(in.StartRecordName) == "_acm-sync.www.example.com."
		})).Return(listOut(rrSet("_acm-sync.www.example.com.", r53types.RRTypeTxt, 120, `"heritage=x"`)), nil)
		got := captureChange(api, nil)

		recs := []dns.Record{
			{Name: "www.example.com", Type: dns.TypeCNAME, Target: "d1.cloudfront.net"},
			{Name: "_acm-sync.www.example.com", Type: dns.TypeTXT, Target: "heritage=x"},
		}
		require.NoError(t, route53client.NewProvider(api).DeleteRecords(ctx, zone, recs))
		assert.Equal(t, []string{"DELETE CNAME www.example.com.", "DELETE TXT _acm-sync.www.example.com."}, summarize(*got))
		ch := (*got).ChangeBatch.Changes
		assert.Equal(t, "D1.cloudfront.net.", aws.ToString(ch[0].ResourceRecordSet.ResourceRecords[0].Value))
		assert.Equal(t, int64(120), aws.ToInt64(ch[1].ResourceRecordSet.TTL))
	})

	t.Run("alias deletes both A and AAAA", func(t *testing.T) {
		api := &route53client.MockRoute53API{}
		api.On("ListResourceRecordSets", mock.Anything, mock.Anything).Return(listOut(
			aliasSet(r53types.RRTypeA, "d1.cloudfront.net."),
			aliasSet(r53types.RRTypeAaaa, "d1.cloudfront.net."),
		), nil)
		got := captureChange(api, nil)

		alias := dns.Record{Name: "example.com", Type: dns.TypeALIAS, Target: "d1.cloudfront.net"}
		require.NoError(t, route53client.NewProvider(api).DeleteRecords(ctx, zone, []dns.Record{alias}))
		assert.Equal(t, []string{"DELETE A example.com.", "DELETE AAAA example.com."}, summarize(*got))
		assert.Equal(t, "Z2FDTNDATAQYW2", aws.ToString((*got).ChangeBatch.Changes[0].ResourceRecordSet.AliasTarget.HostedZoneId))
	})

	t.Run("txt set with foreign values is rewritten not deleted", func(t *testing.T) {
		api := &route53client.MockRoute53API{}
		api.On("ListResourceRecordSets", mock.Anything, mock.Anything).Return(listOut(rrSet("_acm-sync.www.example.com.", r53types.RRTypeTxt, 300, `"heritage=x"`, `"other"`)), nil)
		got := captureChange(api, nil)

		txt := dns.Record{Name: "_acm-sync.www.example.com", Type: dns.TypeTXT, Target: "heritage=x"}
		require.NoError(t, route53client.NewProvider(api).DeleteRecords(ctx, zone, []dns.Record{txt}))
		assert.Equal(t, []string{"UPSERT TXT _acm-sync.www.example.com."}, summarize(*got))
		rr := (*got).ChangeBatch.Changes[0].ResourceRecordSet.ResourceRecords
		require.Len(t, rr, 1)
		assert.Equal(t, `"other"`, aws.ToString(rr[0].Value))
	})

	t.Run("nothing to delete makes no change call", func(t *testing.T) {
		api := &route53client.MockRoute53API{}
		api.On("ListResourceRecordSets", mock.Anything, mock.Anything).Return(listOut(), nil)

		cname := dns.Record{Name: "www.example.com", Type: dns.TypeCNAME, Target: "x"}
		require.NoError(t, route53client.NewProvider(api).DeleteRecords(ctx, zone, []dns.Record{cname}))
		api.AssertNotCalled(t, "ChangeResourceRecordSets", mock.Anything, mock.Anything)
	})
}

func TestProvider_ListErrorMapped(t *testing.T) {
	api := &route53client.MockRoute53API{}
	api.On("ListResourceRecordSets", mock.Anything, mock.Anything).Return(nil, &smithy.GenericAPIError{Code: "Throttling"})

	_, err := route53client.NewProvider(api).GetRecords(context.Background(), zone, "www.example.com")
	assert.ErrorIs(t, err, dns.ErrRetryable)
}
