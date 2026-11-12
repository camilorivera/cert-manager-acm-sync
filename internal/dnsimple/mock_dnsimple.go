package dnsimple

import (
	"context"

	"github.com/stretchr/testify/mock"
)

// MockAPI is a testify mock satisfying API.
type MockAPI struct {
	mock.Mock
}

func (m *MockAPI) ListRecords(ctx context.Context, zone, name string) ([]Record, error) {
	args := m.Called(ctx, zone, name)
	if out := args.Get(0); out != nil {
		return out.([]Record), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockAPI) CreateRecord(ctx context.Context, zone string, in RecordInput) (Record, error) {
	args := m.Called(ctx, zone, in)
	return args.Get(0).(Record), args.Error(1)
}

func (m *MockAPI) UpdateRecord(ctx context.Context, zone string, id int64, in RecordInput) (Record, error) {
	args := m.Called(ctx, zone, id, in)
	return args.Get(0).(Record), args.Error(1)
}

func (m *MockAPI) DeleteRecord(ctx context.Context, zone string, id int64) error {
	return m.Called(ctx, zone, id).Error(0)
}
