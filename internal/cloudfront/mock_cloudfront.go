package cloudfrontclient

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/stretchr/testify/mock"
)

// MockCloudFrontAPI is a testify mock satisfying CloudFrontAPI.
type MockCloudFrontAPI struct {
	mock.Mock
}

func (m *MockCloudFrontAPI) GetDistributionConfig(ctx context.Context, params *cloudfront.GetDistributionConfigInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error) {
	args := m.Called(ctx, params)
	if out := args.Get(0); out != nil {
		return out.(*cloudfront.GetDistributionConfigOutput), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockCloudFrontAPI) UpdateDistribution(ctx context.Context, params *cloudfront.UpdateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error) {
	args := m.Called(ctx, params)
	if out := args.Get(0); out != nil {
		return out.(*cloudfront.UpdateDistributionOutput), args.Error(1)
	}
	return nil, args.Error(1)
}
