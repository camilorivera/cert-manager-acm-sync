package dnsimple_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
	"github.com/camilorivera/cert-manager-acm-sync/internal/dnsimple"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *dnsimple.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return dnsimple.NewClient(srv.URL, "1234", "tok", srv.Client())
}

func TestClient_ListRecords_HeadersAndFilter(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		assert.Equal(t, "cert-manager-acm-sync", r.Header.Get("User-Agent"))
		assert.Equal(t, "/v2/1234/zones/example.com/records", r.URL.Path)
		assert.Equal(t, "www", r.URL.Query().Get("name"))
		_, _ = w.Write([]byte(`{"data":[
			{"id":1,"name":"www","type":"CNAME","content":"a.example.net","ttl":300},
			{"id":2,"name":"www2","type":"CNAME","content":"b.example.net","ttl":300}
		],"pagination":{"current_page":1,"total_pages":1}}`))
	})

	recs, err := c.ListRecords(context.Background(), "example.com", "www")
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, int64(1), recs[0].ID)
}

func TestClient_ListRecords_ApexOmitsNameFilter(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.False(t, r.URL.Query().Has("name"))
		_, _ = w.Write([]byte(`{"data":[
			{"id":1,"name":"","type":"ALIAS","content":"x.cloudfront.net","ttl":300},
			{"id":2,"name":"www","type":"CNAME","content":"x","ttl":300}
		],"pagination":{"current_page":1,"total_pages":1}}`))
	})

	recs, err := c.ListRecords(context.Background(), "example.com", "")
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, "ALIAS", recs[0].Type)
}

func TestClient_ListRecords_Pagination(t *testing.T) {
	var pages []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		switch page {
		case "1":
			_, _ = w.Write([]byte(`{"data":[{"id":1,"name":"www","type":"A","content":"1.1.1.1"}],"pagination":{"current_page":1,"total_pages":2}}`))
		default:
			_, _ = w.Write([]byte(`{"data":[{"id":2,"name":"www","type":"TXT","content":"x"}],"pagination":{"current_page":2,"total_pages":2}}`))
		}
	})

	recs, err := c.ListRecords(context.Background(), "example.com", "www")
	require.NoError(t, err)
	assert.Equal(t, []string{"1", "2"}, pages)
	assert.Len(t, recs, 2)
}

func TestClient_CreateUpdateDelete(t *testing.T) {
	var got []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var in dnsimple.RecordInput
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "www", in.Name)
		assert.Equal(t, "a.example.net", in.Content)
		assert.Equal(t, int64(300), in.TTL)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"id":7,"name":"www","type":"CNAME","content":"a.example.net","ttl":300}}`))
	})
	ctx := context.Background()
	in := dnsimple.RecordInput{Name: "www", Type: "CNAME", Content: "a.example.net", TTL: 300}

	rec, err := c.CreateRecord(ctx, "example.com", in)
	require.NoError(t, err)
	assert.Equal(t, int64(7), rec.ID)
	_, err = c.UpdateRecord(ctx, "example.com", 7, in)
	require.NoError(t, err)
	require.NoError(t, c.DeleteRecord(ctx, "example.com", 7))

	assert.Equal(t, []string{
		"POST /v2/1234/zones/example.com/records",
		"PATCH /v2/1234/zones/example.com/records/7",
		"DELETE /v2/1234/zones/example.com/records/7",
	}, got)
}

func TestClient_ErrorMapping(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		retryable bool
		contains  string
	}{
		{"rate limited", http.StatusTooManyRequests, `{"message":"slow down"}`, true, "slow down"},
		{"server error", http.StatusBadGateway, `bad gateway`, true, "bad gateway"},
		{"validation", http.StatusBadRequest, `{"message":"Validation failed","errors":{"content":["is invalid"]}}`, false, "content is invalid"},
		{"unauthorized", http.StatusUnauthorized, `{"message":"Authentication failed"}`, false, "Authentication failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			_, err := c.CreateRecord(context.Background(), "example.com", dnsimple.RecordInput{Name: "www"})
			require.Error(t, err)
			assert.Equal(t, tt.retryable, errors.Is(err, dns.ErrRetryable))
			assert.Contains(t, err.Error(), tt.contains)
		})
	}
}

func TestClient_NotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Record not found"}`))
	})
	err := c.DeleteRecord(context.Background(), "example.com", 1)
	require.Error(t, err)
	assert.True(t, dnsimple.IsNotFound(err))
}
