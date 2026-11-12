package dns_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
)

const (
	owner    = "prod"
	resource = "secret/ns/tls"
	target   = "d123.cloudfront.net"
)

var (
	zone    = dns.Zone{Name: "example.com", ID: "Z1"}
	ctx     = mock.Anything
	ownTXT  = dns.EncodeOwner(owner, resource)
	otherTX = dns.EncodeOwner("other", resource)
)

func txtFor(fqdn, value string) dns.Record {
	return dns.Record{Name: dns.OwnerName(fqdn), Type: dns.TypeTXT, Target: value, TTL: 300}
}

func cname(fqdn, tgt string) dns.Record {
	return dns.Record{Name: fqdn, Type: dns.TypeCNAME, Target: tgt, TTL: 300}
}

func newSyncer(p *dns.MockProvider) *dns.Syncer {
	return &dns.Syncer{
		Provider:       p,
		Zones:          dns.ZoneSet{zone},
		OwnerID:        owner,
		WildcardPolicy: dns.WildcardSkip,
		DeletePolicy:   dns.DeleteRetain,
	}
}

func baseReq(names ...string) dns.Request {
	return dns.Request{Resource: resource, Names: names, Target: target, Type: dns.TypeCNAME, TTL: 300}
}

func expectGet(p *dns.MockProvider, fqdn string, recs []dns.Record, owners ...dns.Record) {
	p.On("GetRecords", ctx, zone, fqdn).Return(recs, nil)
	p.On("GetRecords", ctx, zone, dns.OwnerName(fqdn)).Return(owners, nil)
}

func TestSync(t *testing.T) {
	www := "www.example.com"
	wantCNAME := cname(www, target)

	tests := []struct {
		name       string
		mutate     func(*dns.Syncer, *dns.Request)
		setup      func(*dns.MockProvider)
		wantAction string
		wantOwned  []string
	}{
		{
			name: "no record, no TXT creates both",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, nil)
				p.On("CreateRecords", ctx, zone, []dns.Record{txtFor(www, ownTXT), wantCNAME}).Return(nil)
			},
			wantAction: dns.ActionCreated,
			wantOwned:  []string{www},
		},
		{
			name: "no record, our TXT creates record only",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, nil, txtFor(www, ownTXT))
				p.On("CreateRecords", ctx, zone, []dns.Record{wantCNAME}).Return(nil)
			},
			wantAction: dns.ActionCreated,
			wantOwned:  []string{www},
		},
		{
			name: "no record, foreign TXT conflicts",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, nil, txtFor(www, otherTX))
			},
			wantAction: dns.ActionConflict,
		},
		{
			name: "create race maps to conflict",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, nil)
				p.On("CreateRecords", ctx, zone, mock.Anything).Return(dns.ErrAlreadyExists)
			},
			wantAction: dns.ActionConflict,
		},
		{
			name: "owned and identical is unchanged",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{wantCNAME}, txtFor(www, ownTXT))
			},
			wantAction: dns.ActionUnchanged,
			wantOwned:  []string{www},
		},
		{
			name: "owned target differs is updated",
			setup: func(p *dns.MockProvider) {
				old := cname(www, "old.cloudfront.net")
				expectGet(p, www, []dns.Record{old}, txtFor(www, ownTXT))
				p.On("UpdateRecord", ctx, zone, old, wantCNAME).Return(nil)
			},
			wantAction: dns.ActionUpdated,
			wantOwned:  []string{www},
		},
		{
			name: "owned TTL differs is updated",
			setup: func(p *dns.MockProvider) {
				old := wantCNAME
				old.TTL = 60
				expectGet(p, www, []dns.Record{old}, txtFor(www, ownTXT))
				p.On("UpdateRecord", ctx, zone, old, wantCNAME).Return(nil)
			},
			wantAction: dns.ActionUpdated,
			wantOwned:  []string{www},
		},
		{
			name: "owned type differs is updated",
			setup: func(p *dns.MockProvider) {
				old := wantCNAME
				old.Type = dns.TypeALIAS
				expectGet(p, www, []dns.Record{old}, txtFor(www, ownTXT))
				p.On("UpdateRecord", ctx, zone, old, wantCNAME).Return(nil)
			},
			wantAction: dns.ActionUpdated,
			wantOwned:  []string{www},
		},
		{
			name: "TXT for another resource is foreign",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{cname(www, "x.example.org")}, txtFor(www, dns.EncodeOwner(owner, "secret/ns/other")))
			},
			wantAction: dns.ActionConflict,
		},
		{
			name: "unowned matching target without adopt is unmanaged_ok",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{cname(www, target+".")})
			},
			wantAction: dns.ActionUnmanagedOK,
		},
		{
			name: "unowned matching target ignores CNAME vs ALIAS",
			setup: func(p *dns.MockProvider) {
				r := wantCNAME
				r.Type = dns.TypeALIAS
				expectGet(p, www, []dns.Record{r})
			},
			wantAction: dns.ActionUnmanagedOK,
		},
		{
			name:   "unowned matching target with adopt writes TXT",
			mutate: func(_ *dns.Syncer, r *dns.Request) { r.Adopt = true },
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{wantCNAME})
				p.On("CreateRecords", ctx, zone, []dns.Record{txtFor(www, ownTXT)}).Return(nil)
			},
			wantAction: dns.ActionAdopted,
			wantOwned:  []string{www},
		},
		{
			name:   "adopt with foreign TXT conflicts",
			mutate: func(_ *dns.Syncer, r *dns.Request) { r.Adopt = true },
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{wantCNAME}, txtFor(www, otherTX))
			},
			wantAction: dns.ActionConflict,
		},
		{
			name: "foreign TXT matching target without adopt is unmanaged_ok",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{wantCNAME}, txtFor(www, otherTX))
			},
			wantAction: dns.ActionUnmanagedOK,
		},
		{
			name:   "adopt never adopts a different target",
			mutate: func(_ *dns.Syncer, r *dns.Request) { r.Adopt = true },
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{cname(www, "elsewhere.example.org")})
			},
			wantAction: dns.ActionConflict,
		},
		{
			name: "unowned different target conflicts",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{cname(www, "elsewhere.example.org")})
			},
			wantAction: dns.ActionConflict,
		},
		{
			name: "existing A record conflicts",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{{Name: www, Type: "A", Target: "1.2.3.4", TTL: 300}}, txtFor(www, ownTXT))
			},
			wantAction: dns.ActionConflict,
		},
		{
			name: "no zone",
			mutate: func(_ *dns.Syncer, r *dns.Request) {
				r.Names = []string{"www.example.org"}
			},
			setup:      func(*dns.MockProvider) {},
			wantAction: dns.ActionNoZone,
		},
		{
			name: "label boundary is no zone",
			mutate: func(_ *dns.Syncer, r *dns.Request) {
				r.Names = []string{"www.notexample.com"}
			},
			setup:      func(*dns.MockProvider) {},
			wantAction: dns.ActionNoZone,
		},
		{
			name: "wildcard skipped by default",
			mutate: func(_ *dns.Syncer, r *dns.Request) {
				r.Names = []string{"*.example.com"}
			},
			setup:      func(*dns.MockProvider) {},
			wantAction: dns.ActionSkippedWildcard,
		},
		{
			name: "wildcard created when policy is create",
			mutate: func(s *dns.Syncer, r *dns.Request) {
				s.WildcardPolicy = dns.WildcardCreate
				r.Names = []string{"*.example.com"}
			},
			setup: func(p *dns.MockProvider) {
				w := "*.example.com"
				expectGet(p, w, nil)
				p.On("CreateRecords", ctx, zone, []dns.Record{
					{Name: "_acm-sync._wildcard.example.com", Type: dns.TypeTXT, Target: ownTXT, TTL: 300},
					cname(w, target),
				}).Return(nil)
			},
			wantAction: dns.ActionCreated,
			wantOwned:  []string{"*.example.com"},
		},
		{
			name: "apex forces ALIAS",
			mutate: func(_ *dns.Syncer, r *dns.Request) {
				r.Names = []string{"example.com"}
			},
			setup: func(p *dns.MockProvider) {
				expectGet(p, "example.com", nil)
				p.On("CreateRecords", ctx, zone, []dns.Record{
					txtFor("example.com", ownTXT),
					{Name: "example.com", Type: dns.TypeALIAS, Target: target, TTL: 300},
				}).Return(nil)
			},
			wantAction: dns.ActionCreated,
			wantOwned:  []string{"example.com"},
		},
		{
			name: "apex owned ALIAS is unchanged",
			mutate: func(_ *dns.Syncer, r *dns.Request) {
				r.Names = []string{"example.com"}
			},
			setup: func(p *dns.MockProvider) {
				expectGet(p, "example.com",
					[]dns.Record{{Name: "example.com", Type: dns.TypeALIAS, Target: target, TTL: 300}},
					txtFor("example.com", ownTXT))
			},
			wantAction: dns.ActionUnchanged,
			wantOwned:  []string{"example.com"},
		},
		{
			name: "default type is CNAME",
			mutate: func(_ *dns.Syncer, r *dns.Request) {
				r.Type = ""
			},
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, nil)
				p.On("CreateRecords", ctx, zone, []dns.Record{txtFor(www, ownTXT), wantCNAME}).Return(nil)
			},
			wantAction: dns.ActionCreated,
			wantOwned:  []string{www},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &dns.MockProvider{}
			s := newSyncer(p)
			req := baseReq(www)
			if tt.mutate != nil {
				tt.mutate(s, &req)
			}
			tt.setup(p)

			results, owned, err := s.Sync(context.Background(), req)

			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, tt.wantAction, results[0].Action, results[0].Detail)
			assert.Equal(t, tt.wantOwned, owned)
			p.AssertExpectations(t)
		})
	}
}

func TestSyncProviderErrors(t *testing.T) {
	www := "www.example.com"
	boom := errors.New("boom")

	tests := []struct {
		name      string
		previous  []string
		setup     func(*dns.MockProvider)
		err       error
		wantOwned []string
	}{
		{
			name: "get error",
			setup: func(p *dns.MockProvider) {
				p.On("GetRecords", ctx, zone, www).Return(nil, boom)
			},
			err: boom,
		},
		{
			name: "retryable create error",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, nil)
				p.On("CreateRecords", ctx, zone, mock.Anything).Return(dns.ErrRetryable)
			},
			err: dns.ErrRetryable,
		},
		{
			name:     "error keeps previously owned name tracked",
			previous: []string{www},
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{cname(www, "old.example.org")}, txtFor(www, ownTXT))
				p.On("UpdateRecord", ctx, zone, mock.Anything, mock.Anything).Return(dns.ErrRetryable)
			},
			err:       dns.ErrRetryable,
			wantOwned: []string{www},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &dns.MockProvider{}
			tt.setup(p)
			req := baseReq(www)
			req.Previous = tt.previous

			results, owned, err := newSyncer(p).Sync(context.Background(), req)

			require.ErrorIs(t, err, tt.err)
			require.Len(t, results, 1)
			assert.Equal(t, dns.ActionError, results[0].Action)
			assert.Equal(t, tt.wantOwned, owned)
		})
	}
}

func TestSyncContinuesAfterError(t *testing.T) {
	a, b := "a.example.com", "b.example.com"
	p := &dns.MockProvider{}
	p.On("GetRecords", ctx, zone, a).Return(nil, dns.ErrRetryable)
	expectGet(p, b, []dns.Record{cname(b, target)}, txtFor(b, ownTXT))

	results, owned, err := newSyncer(p).Sync(context.Background(), baseReq(a, b))

	require.ErrorIs(t, err, dns.ErrRetryable)
	require.Len(t, results, 2)
	assert.Equal(t, dns.ActionError, results[0].Action)
	assert.Equal(t, dns.ActionUnchanged, results[1].Action)
	assert.Equal(t, []string{b}, owned)
}

func TestSyncPrune(t *testing.T) {
	keep, gone := "keep.example.com", "gone.example.com"

	tests := []struct {
		name        string
		policy      string
		setup       func(*dns.MockProvider)
		wantActions []string
		wantOwned   []string
		wantErr     bool
	}{
		{
			name:   "retain never deletes",
			policy: dns.DeleteRetain,
			setup: func(p *dns.MockProvider) {
				expectGet(p, keep, []dns.Record{cname(keep, target)}, txtFor(keep, ownTXT))
			},
			wantActions: []string{dns.ActionUnchanged},
			wantOwned:   []string{keep},
		},
		{
			name:   "delete prunes owned name",
			policy: dns.DeleteDelete,
			setup: func(p *dns.MockProvider) {
				expectGet(p, keep, []dns.Record{cname(keep, target)}, txtFor(keep, ownTXT))
				expectGet(p, gone, []dns.Record{cname(gone, target)}, txtFor(gone, ownTXT))
				p.On("DeleteRecords", ctx, zone, []dns.Record{cname(gone, target)}).Return(nil)
				p.On("DeleteRecords", ctx, zone, []dns.Record{txtFor(gone, ownTXT)}).Return(nil)
			},
			wantActions: []string{dns.ActionUnchanged, dns.ActionDeleted},
			wantOwned:   []string{keep},
		},
		{
			name:   "delete leaves name with foreign TXT",
			policy: dns.DeleteDelete,
			setup: func(p *dns.MockProvider) {
				expectGet(p, keep, []dns.Record{cname(keep, target)}, txtFor(keep, ownTXT))
				expectGet(p, gone, []dns.Record{cname(gone, target)}, txtFor(gone, otherTX))
			},
			wantActions: []string{dns.ActionUnchanged, dns.ActionUnmanagedOK},
			wantOwned:   []string{keep},
		},
		{
			name:   "failed prune stays tracked",
			policy: dns.DeleteDelete,
			setup: func(p *dns.MockProvider) {
				expectGet(p, keep, []dns.Record{cname(keep, target)}, txtFor(keep, ownTXT))
				expectGet(p, gone, []dns.Record{cname(gone, target)}, txtFor(gone, ownTXT))
				p.On("DeleteRecords", ctx, zone, mock.Anything).Return(dns.ErrRetryable)
			},
			wantActions: []string{dns.ActionUnchanged, dns.ActionError},
			wantOwned:   []string{keep, gone},
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &dns.MockProvider{}
			tt.setup(p)
			s := newSyncer(p)
			s.DeletePolicy = tt.policy
			req := baseReq(keep)
			req.Previous = []string{keep, gone}

			results, owned, err := s.Sync(context.Background(), req)

			if tt.wantErr {
				require.ErrorIs(t, err, dns.ErrRetryable)
			} else {
				require.NoError(t, err)
			}
			var actions []string
			for _, r := range results {
				actions = append(actions, r.Action)
			}
			assert.Equal(t, tt.wantActions, actions)
			assert.Equal(t, tt.wantOwned, owned)
			p.AssertExpectations(t)
		})
	}
}

func TestCleanup(t *testing.T) {
	www := "www.example.com"

	tests := []struct {
		name       string
		setup      func(*dns.MockProvider)
		wantAction string
		wantErr    error
	}{
		{
			name: "deletes record then owner TXT",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{cname(www, target)}, txtFor(www, ownTXT))
				p.On("DeleteRecords", ctx, zone, []dns.Record{cname(www, target)}).Return(nil).Once()
				p.On("DeleteRecords", ctx, zone, []dns.Record{txtFor(www, ownTXT)}).Return(nil).Once()
			},
			wantAction: dns.ActionDeleted,
		},
		{
			name: "record already gone deletes TXT only",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, nil, txtFor(www, ownTXT))
				p.On("DeleteRecords", ctx, zone, []dns.Record{txtFor(www, ownTXT)}).Return(nil).Once()
			},
			wantAction: dns.ActionDeleted,
		},
		{
			name: "foreign TXT is left alone",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{cname(www, target)}, txtFor(www, otherTX))
			},
			wantAction: dns.ActionUnmanagedOK,
		},
		{
			name: "no TXT is left alone",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{cname(www, target)})
			},
			wantAction: dns.ActionUnmanagedOK,
		},
		{
			name: "retryable delete error",
			setup: func(p *dns.MockProvider) {
				expectGet(p, www, []dns.Record{cname(www, target)}, txtFor(www, ownTXT))
				p.On("DeleteRecords", ctx, zone, mock.Anything).Return(dns.ErrRetryable)
			},
			wantAction: dns.ActionError,
			wantErr:    dns.ErrRetryable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &dns.MockProvider{}
			tt.setup(p)

			results, err := newSyncer(p).Cleanup(context.Background(), resource, []string{www})

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, results, 1)
			assert.Equal(t, tt.wantAction, results[0].Action)
			p.AssertExpectations(t)
		})
	}
}

func TestCleanupIgnoresDeletePolicyAndNoZone(t *testing.T) {
	p := &dns.MockProvider{}
	s := newSyncer(p)

	results, err := s.Cleanup(context.Background(), resource, []string{"www.example.org"})

	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestSyncAdoptRequiresMatchingTypeAndTTL(t *testing.T) {
	www := "www.example.com"
	tests := []struct {
		name     string
		existing dns.Record
	}{
		{"type differs", dns.Record{Name: www, Type: dns.TypeALIAS, Target: target, TTL: 300}},
		{"ttl differs", dns.Record{Name: www, Type: dns.TypeCNAME, Target: target, TTL: 60}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &dns.MockProvider{}
			expectGet(p, www, []dns.Record{tt.existing})
			req := baseReq(www)
			req.Adopt = true

			results, owned, err := newSyncer(p).Sync(context.Background(), req)

			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, dns.ActionConflict, results[0].Action)
			assert.Empty(t, owned)
			p.AssertNotCalled(t, "CreateRecords", ctx, zone, mock.Anything)
			p.AssertNotCalled(t, "UpdateRecord", ctx, zone, mock.Anything, mock.Anything)
		})
	}
}

func TestSyncAdoptKeepsAliasWithZeroTTL(t *testing.T) {
	www := "www.example.com"
	p := &dns.MockProvider{}
	expectGet(p, www, []dns.Record{{Name: www, Type: dns.TypeALIAS, Target: target}})
	p.On("CreateRecords", ctx, zone, []dns.Record{txtFor(www, ownTXT)}).Return(nil)
	req := baseReq(www)
	req.Adopt = true
	req.Type = dns.TypeALIAS

	results, owned, err := newSyncer(p).Sync(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, dns.ActionAdopted, results[0].Action)
	assert.Equal(t, []string{www}, owned)
}

func TestSyncConflictsWithOtherRecordTypes(t *testing.T) {
	www := "www.example.com"
	tests := []struct {
		name string
		recs []dns.Record
	}{
		{"url record", []dns.Record{{Name: www, Type: "URL", Target: "https://example.net", TTL: 300}}},
		{"mx record", []dns.Record{{Name: www, Type: "MX", Target: "mail.example.net", TTL: 300}}},
		{"ns delegation", []dns.Record{{Name: www, Type: "NS", Target: "ns1.example.net", TTL: 300}}},
		{"cname beside mx", []dns.Record{cname(www, target), {Name: www, Type: "MX", Target: "mail", TTL: 300}}},
		{"txt beside cname we want to create", []dns.Record{{Name: www, Type: dns.TypeTXT, Target: "v=spf1 -all", TTL: 300}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &dns.MockProvider{}
			expectGet(p, www, tt.recs)

			results, owned, err := newSyncer(p).Sync(context.Background(), baseReq(www))

			require.NoError(t, err)
			assert.Equal(t, dns.ActionConflict, results[0].Action)
			assert.Empty(t, owned)
			p.AssertNotCalled(t, "CreateRecords", ctx, zone, mock.Anything)
		})
	}
}

func TestSyncApexCoexistsWithMailAndTXT(t *testing.T) {
	apex := "example.com"
	p := &dns.MockProvider{}
	expectGet(p, apex, []dns.Record{
		{Name: apex, Type: "MX", Target: "mail", TTL: 300},
		{Name: apex, Type: dns.TypeTXT, Target: "v=spf1 -all", TTL: 300},
		{Name: apex, Type: "NS", Target: "ns1", TTL: 300},
	})
	alias := dns.Record{Name: apex, Type: dns.TypeALIAS, Target: target, TTL: 300}
	p.On("CreateRecords", ctx, zone, []dns.Record{txtFor(apex, ownTXT), alias}).Return(nil)

	results, owned, err := newSyncer(p).Sync(context.Background(), baseReq(apex))

	require.NoError(t, err)
	assert.Equal(t, dns.ActionCreated, results[0].Action)
	assert.Equal(t, []string{apex}, owned)
}

func TestSyncApexConflictsWithAddressRecords(t *testing.T) {
	apex := "example.com"
	p := &dns.MockProvider{}
	expectGet(p, apex, []dns.Record{{Name: apex, Type: "A", Target: "1.1.1.1", TTL: 300}})

	results, _, err := newSyncer(p).Sync(context.Background(), baseReq(apex))

	require.NoError(t, err)
	assert.Equal(t, dns.ActionConflict, results[0].Action)
}

func TestRemoveOnlyDeletesRecordWeManage(t *testing.T) {
	gone := "gone.example.com"
	tests := []struct {
		name       string
		target     string
		recs       []dns.Record
		wantRecord bool
		wantAction string
	}{
		{"single cname matching target", target, []dns.Record{cname(gone, target)}, true, dns.ActionDeleted},
		{"single cname unknown target", "", []dns.Record{cname(gone, "anything.example.net")}, true, dns.ActionDeleted},
		{"single alias unknown target", "", []dns.Record{{Name: gone, Type: dns.TypeALIAS, Target: target}}, true, dns.ActionDeleted},
		{"cname pointing elsewhere", target, []dns.Record{cname(gone, "other.example.net")}, false, dns.ActionConflict},
		{"extra A record beside cname", "", []dns.Record{cname(gone, target), {Name: gone, Type: "A", Target: "1.1.1.1"}}, false, dns.ActionConflict},
		{"only an A record", "", []dns.Record{{Name: gone, Type: "A", Target: "1.1.1.1"}}, false, dns.ActionConflict},
		{"mx record", target, []dns.Record{{Name: gone, Type: "MX", Target: "mail"}}, false, dns.ActionConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &dns.MockProvider{}
			expectGet(p, gone, tt.recs, txtFor(gone, ownTXT))
			if tt.wantRecord {
				p.On("DeleteRecords", ctx, zone, tt.recs).Return(nil).Once()
			}
			p.On("DeleteRecords", ctx, zone, []dns.Record{txtFor(gone, ownTXT)}).Return(nil).Once()
			s := newSyncer(p)
			s.DeletePolicy = dns.DeleteDelete

			var results []dns.NameResult
			var err error
			if tt.target == "" {
				results, err = s.Cleanup(context.Background(), resource, []string{gone})
			} else {
				req := baseReq()
				req.Previous = []string{gone}
				results, _, err = s.Sync(context.Background(), req)
			}

			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, tt.wantAction, results[0].Action, results[0].Detail)
			p.AssertExpectations(t)
			p.AssertNumberOfCalls(t, "DeleteRecords", map[bool]int{true: 2, false: 1}[tt.wantRecord])
		})
	}
}

func TestRemoveKeepsNameTrackedWhenTXTDeleteFails(t *testing.T) {
	gone := "gone.example.com"
	p := &dns.MockProvider{}
	expectGet(p, gone, []dns.Record{cname(gone, target)}, txtFor(gone, ownTXT))
	p.On("DeleteRecords", ctx, zone, []dns.Record{cname(gone, target)}).Return(nil).Once()
	p.On("DeleteRecords", ctx, zone, []dns.Record{txtFor(gone, ownTXT)}).Return(dns.ErrRetryable).Once()
	s := newSyncer(p)
	s.DeletePolicy = dns.DeleteDelete
	req := baseReq()
	req.Previous = []string{gone}

	_, owned, err := s.Sync(context.Background(), req)

	require.ErrorIs(t, err, dns.ErrRetryable)
	assert.Equal(t, []string{gone}, owned)
}
