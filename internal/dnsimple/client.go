package dnsimple

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
)

const (
	userAgent = "cert-manager-acm-sync"
	perPage   = 100
	maxBody   = 1 << 20
)

// API is the subset of the DNSimple records API the provider uses. The zone
// argument is the zone name or numeric ID; name is zone-relative ("" for apex).
type API interface {
	ListRecords(ctx context.Context, zone, name string) ([]Record, error)
	CreateRecord(ctx context.Context, zone string, in RecordInput) (Record, error)
	UpdateRecord(ctx context.Context, zone string, id int64, in RecordInput) (Record, error)
	DeleteRecord(ctx context.Context, zone string, id int64) error
}

type Record struct {
	ID           int64  `json:"id"`
	ParentID     int64  `json:"parent_id"`
	Name         string `json:"name"`
	Content      string `json:"content"`
	TTL          int64  `json:"ttl"`
	Type         string `json:"type"`
	SystemRecord bool   `json:"system_record"`
}

type RecordInput struct {
	Name    string `json:"name"`
	Type    string `json:"type,omitempty"`
	Content string `json:"content"`
	TTL     int64  `json:"ttl"`
}

// APIError is a non-2xx DNSimple response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("dnsimple: status %d: %s", e.Status, e.Message)
}

func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

type Client struct {
	baseURL    string
	accountID  string
	token      string
	httpClient *http.Client
}

func NewClient(baseURL, accountID, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: baseURL, accountID: accountID, token: token, httpClient: httpClient}
}

func (c *Client) recordsPath(zone string) string {
	return "/v2/" + url.PathEscape(c.accountID) + "/zones/" + url.PathEscape(zone) + "/records"
}

type listResponse struct {
	Data       []Record `json:"data"`
	Pagination struct {
		CurrentPage int `json:"current_page"`
		TotalPages  int `json:"total_pages"`
	} `json:"pagination"`
}

type recordResponse struct {
	Data Record `json:"data"`
}

// ListRecords returns the records whose name matches exactly, following pagination.
func (c *Client) ListRecords(ctx context.Context, zone, name string) ([]Record, error) {
	var out []Record
	for page := 1; ; page++ {
		q := url.Values{}
		if name != "" {
			q.Set("name", name)
		}
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))
		var resp listResponse
		if err := c.do(ctx, http.MethodGet, c.recordsPath(zone)+"?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Data {
			if r.Name == name {
				out = append(out, r)
			}
		}
		if page >= resp.Pagination.TotalPages {
			return out, nil
		}
	}
}

func (c *Client) CreateRecord(ctx context.Context, zone string, in RecordInput) (Record, error) {
	var resp recordResponse
	err := c.do(ctx, http.MethodPost, c.recordsPath(zone), in, &resp)
	return resp.Data, err
}

func (c *Client) UpdateRecord(ctx context.Context, zone string, id int64, in RecordInput) (Record, error) {
	var resp recordResponse
	err := c.do(ctx, http.MethodPatch, c.recordsPath(zone)+"/"+strconv.FormatInt(id, 10), in, &resp)
	return resp.Data, err
}

func (c *Client) DeleteRecord(ctx context.Context, zone string, id int64) error {
	return c.do(ctx, http.MethodDelete, c.recordsPath(zone)+"/"+strconv.FormatInt(id, 10), nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("dnsimple: encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("dnsimple: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("dnsimple %s %s: %w", method, path, err)
		}
		return fmt.Errorf("dnsimple %s %s: %w: %w", method, path, dns.ErrRetryable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("dnsimple %s %s: read response: %w: %w", method, path, dns.ErrRetryable, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode, Message: errorMessage(raw)}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return fmt.Errorf("dnsimple %s %s: %w: %w", method, path, dns.ErrRetryable, apiErr)
		}
		return fmt.Errorf("dnsimple %s %s: %w", method, path, apiErr)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("dnsimple %s %s: decode response: %w", method, path, err)
	}
	return nil
}

func errorMessage(raw []byte) string {
	var e struct {
		Message string              `json:"message"`
		Errors  map[string][]string `json:"errors"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		msg := e.Message
		for field, msgs := range e.Errors {
			for _, m := range msgs {
				msg += "; " + field + " " + m
			}
		}
		return msg
	}
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return string(bytes.TrimSpace(raw))
}
