package dns

import (
	"context"
	"errors"
)

type RecordType string

const (
	TypeCNAME RecordType = "CNAME"
	TypeALIAS RecordType = "ALIAS"
	TypeTXT   RecordType = "TXT"
)

type Zone struct {
	Name string
	ID   string
}

type Record struct {
	Name   string
	Type   RecordType
	Target string
	TTL    int64
}

var (
	ErrAlreadyExists = errors.New("dns: record already exists")
	ErrRetryable     = errors.New("dns: retryable")
)

type Provider interface {
	Name() string
	GetRecords(ctx context.Context, zone Zone, fqdn string) ([]Record, error)
	// CreateRecords never overwrites and returns ErrAlreadyExists on conflict.
	CreateRecords(ctx context.Context, zone Zone, recs []Record) error
	UpdateRecord(ctx context.Context, zone Zone, old, updated Record) error
	DeleteRecords(ctx context.Context, zone Zone, recs []Record) error
}
