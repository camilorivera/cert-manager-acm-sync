package route53client

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/stretchr/testify/mock"
)

// MockRoute53API is a testify mock satisfying Route53API.
type MockRoute53API struct {
	mock.Mock
}

func (m *MockRoute53API) ListResourceRecordSets(ctx context.Context, params *route53.ListResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error) {
	args := m.Called(ctx, params)
	if out := args.Get(0); out != nil {
		return out.(*route53.ListResourceRecordSetsOutput), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockRoute53API) ChangeResourceRecordSets(ctx context.Context, params *route53.ChangeResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error) {
	args := m.Called(ctx, params)
	if out := args.Get(0); out != nil {
		return out.(*route53.ChangeResourceRecordSetsOutput), args.Error(1)
	}
	return nil, args.Error(1)
}
