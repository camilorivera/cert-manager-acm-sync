package cloudfrontclient_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	cloudfrontclient "github.com/camilorivera/cert-manager-acm-sync/internal/cloudfront"
)

func TestIsInvalidViewerCertificate(t *testing.T) {
	raw := &smithy.GenericAPIError{Code: "InvalidViewerCertificate", Message: "cert doesn't cover domain"}
	wrapped := fmt.Errorf("cloudfront UpdateDistribution ABC: %w", raw)

	assert.True(t, cloudfrontclient.IsInvalidViewerCertificate(raw), "raw error")
	assert.True(t, cloudfrontclient.IsInvalidViewerCertificate(wrapped), "wrapped error")
	assert.False(t, cloudfrontclient.IsInvalidViewerCertificate(errors.New("some other error")))
	assert.False(t, cloudfrontclient.IsInvalidViewerCertificate(&smithy.GenericAPIError{Code: "AccessDenied"}))
}

func TestDistributionIDFromARN(t *testing.T) {
	tests := []struct {
		name    string
		arn     string
		want    string
		wantErr bool
	}{
		{
			name: "valid ARN",
			arn:  "arn:aws:cloudfront::123456789012:distribution/EDFDVBD6EXAMPLE",
			want: "EDFDVBD6EXAMPLE",
		},
		{
			name:    "no slash",
			arn:     "arn:aws:cloudfront::123456789012:distribution",
			wantErr: true,
		},
		{
			name:    "trailing slash",
			arn:     "arn:aws:cloudfront::123456789012:distribution/",
			wantErr: true,
		},
		{
			name:    "empty string",
			arn:     "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cloudfrontclient.DistributionIDFromARN(tt.arn)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func minimalDistributionConfig() *cftypes.DistributionConfig {
	return &cftypes.DistributionConfig{
		CallerReference: aws.String("ref"),
		Comment:         aws.String(""),
		DefaultCacheBehavior: &cftypes.DefaultCacheBehavior{
			ViewerProtocolPolicy: cftypes.ViewerProtocolPolicyRedirectToHttps,
			TargetOriginId:       aws.String("origin"),
			AllowedMethods: &cftypes.AllowedMethods{
				Quantity: aws.Int32(2),
				Items:    []cftypes.Method{cftypes.MethodGet, cftypes.MethodHead},
			},
		},
		Enabled: aws.Bool(true),
		Origins: &cftypes.Origins{
			Quantity: aws.Int32(1),
			Items: []cftypes.Origin{
				{Id: aws.String("origin"), DomainName: aws.String("example.s3.amazonaws.com")},
			},
		},
		ViewerCertificate: &cftypes.ViewerCertificate{
			SSLSupportMethod:             cftypes.SSLSupportMethodSniOnly,
			MinimumProtocolVersion:       cftypes.MinimumProtocolVersionTLSv122021,
			CloudFrontDefaultCertificate: aws.Bool(false),
		},
	}
}

func TestSyncDistribution_HappyPath(t *testing.T) {
	const (
		distARN = "arn:aws:cloudfront::123456789012:distribution/EDFDVBD6EXAMPLE"
		acmARN  = "arn:aws:acm:us-east-1:123456789012:certificate/abc"
		etag    = "E2QWRUHEXAMPLE"
	)
	sans := []string{"example.com", "www.example.com"}

	m := &cloudfrontclient.MockCloudFrontAPI{}
	m.On("GetDistributionConfig", mock.Anything, &cloudfront.GetDistributionConfigInput{
		Id: aws.String("EDFDVBD6EXAMPLE"),
	}).Return(&cloudfront.GetDistributionConfigOutput{
		DistributionConfig: minimalDistributionConfig(),
		ETag:               aws.String(etag),
	}, nil)

	m.On("UpdateDistribution", mock.Anything, mock.MatchedBy(func(in *cloudfront.UpdateDistributionInput) bool {
		return aws.ToString(in.Id) == "EDFDVBD6EXAMPLE" &&
			aws.ToString(in.IfMatch) == etag &&
			aws.ToString(in.DistributionConfig.ViewerCertificate.ACMCertificateArn) == acmARN &&
			len(in.DistributionConfig.Aliases.Items) == 2
	})).Return(&cloudfront.UpdateDistributionOutput{}, nil)

	err := cloudfrontclient.SyncDistribution(context.Background(), m, distARN, acmARN, sans)
	require.NoError(t, err)
	m.AssertExpectations(t)
}

func TestSyncDistribution_GetConfigError(t *testing.T) {
	m := &cloudfrontclient.MockCloudFrontAPI{}
	m.On("GetDistributionConfig", mock.Anything, mock.Anything).
		Return(nil, errors.New("access denied"))

	err := cloudfrontclient.SyncDistribution(context.Background(), m, "arn:aws:cloudfront::123:distribution/ABC", "arn:acm", []string{"a.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GetDistributionConfig")
	m.AssertNotCalled(t, "UpdateDistribution")
}

func TestSyncDistribution_UpdateError(t *testing.T) {
	m := &cloudfrontclient.MockCloudFrontAPI{}
	m.On("GetDistributionConfig", mock.Anything, mock.Anything).
		Return(&cloudfront.GetDistributionConfigOutput{
			DistributionConfig: minimalDistributionConfig(),
			ETag:               aws.String("etag"),
		}, nil)
	m.On("UpdateDistribution", mock.Anything, mock.Anything).
		Return(nil, errors.New("precondition failed"))

	err := cloudfrontclient.SyncDistribution(context.Background(), m, "arn:aws:cloudfront::123:distribution/ABC", "arn:acm", []string{"a.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UpdateDistribution")
}

func TestSyncDistribution_NilConfig(t *testing.T) {
	m := &cloudfrontclient.MockCloudFrontAPI{}
	m.On("GetDistributionConfig", mock.Anything, mock.Anything).
		Return(&cloudfront.GetDistributionConfigOutput{
			DistributionConfig: nil,
			ETag:               aws.String("etag"),
		}, nil)

	err := cloudfrontclient.SyncDistribution(context.Background(), m, "arn:aws:cloudfront::123:distribution/ABC", "arn:acm", []string{"a.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil config")
}

func TestSyncDistribution_BadARN(t *testing.T) {
	m := &cloudfrontclient.MockCloudFrontAPI{}
	err := cloudfrontclient.SyncDistribution(context.Background(), m, "not-an-arn", "arn:acm", []string{"a.com"})
	require.Error(t, err)
	m.AssertNotCalled(t, "GetDistributionConfig")
}
