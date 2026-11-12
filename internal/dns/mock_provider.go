package dns

import (
	"context"

	"github.com/stretchr/testify/mock"
)

// MockProvider is a testify mock satisfying Provider.
type MockProvider struct {
	mock.Mock
}

func (m *MockProvider) Name() string {
	return m.Called().String(0)
}

func (m *MockProvider) GetRecords(ctx context.Context, zone Zone, fqdn string) ([]Record, error) {
	args := m.Called(ctx, zone, fqdn)
	if out := args.Get(0); out != nil {
		return out.([]Record), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockProvider) CreateRecords(ctx context.Context, zone Zone, recs []Record) error {
	return m.Called(ctx, zone, recs).Error(0)
}

func (m *MockProvider) UpdateRecord(ctx context.Context, zone Zone, old, updated Record) error {
	return m.Called(ctx, zone, old, updated).Error(0)
}

func (m *MockProvider) DeleteRecords(ctx context.Context, zone Zone, recs []Record) error {
	return m.Called(ctx, zone, recs).Error(0)
}
