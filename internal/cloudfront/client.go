package cloudfrontclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	smithy "github.com/aws/smithy-go"
)

// CloudFrontAPI is the subset of cloudfront.Client methods the controller uses.
type CloudFrontAPI interface {
	GetDistributionConfig(ctx context.Context, params *cloudfront.GetDistributionConfigInput, optFns ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error)
	UpdateDistribution(ctx context.Context, params *cloudfront.UpdateDistributionInput, optFns ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error)
}

// NewClient returns a CloudFront client built from the base AWS config.
// CloudFront is a global service; no region override is needed.
func NewClient(cfg aws.Config) CloudFrontAPI {
	return cloudfront.NewFromConfig(cfg)
}

// DistributionIDFromARN extracts the distribution ID from a CloudFront ARN.
// ARN format: arn:aws:cloudfront::<account-id>:distribution/<distribution-id>
func DistributionIDFromARN(arn string) (string, error) {
	idx := strings.LastIndex(arn, "/")
	if idx < 0 || idx == len(arn)-1 {
		return "", fmt.Errorf("cloudfront: cannot extract distribution ID from ARN %q", arn)
	}
	return arn[idx+1:], nil
}

// IsInvalidViewerCertificate reports whether err is a CloudFront
// InvalidViewerCertificate error. This happens transiently right after
// ACM ImportCertificate: CloudFront's validation layer may not yet see the
// new certificate's SANs, so it rejects the UpdateDistribution call even
// though the cert was imported successfully.
func IsInvalidViewerCertificate(err error) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == "InvalidViewerCertificate"
}

// SyncDistribution updates a CloudFront distribution's ACM certificate ARN and
// Aliases to match the provided values. It uses optimistic locking: fetches the
// current DistributionConfig + ETag, mutates only the two fields, then calls
// UpdateDistribution. Existing SSLSupportMethod and MinimumProtocolVersion are
// preserved if already set; otherwise they default to sni-only / TLSv1.2_2021.
func SyncDistribution(ctx context.Context, client CloudFrontAPI, distributionARN, acmCertARN string, sans []string) error {
	distID, err := DistributionIDFromARN(distributionARN)
	if err != nil {
		return err
	}

	out, err := client.GetDistributionConfig(ctx, &cloudfront.GetDistributionConfigInput{
		Id: aws.String(distID),
	})
	if err != nil {
		return fmt.Errorf("cloudfront GetDistributionConfig %s: %w", distID, err)
	}

	cfg := out.DistributionConfig
	if cfg == nil {
		return fmt.Errorf("cloudfront: GetDistributionConfig returned nil config for %s", distID)
	}

	cfg.Aliases = &cftypes.Aliases{
		Quantity: aws.Int32(int32(len(sans))),
		Items:    sans,
	}

	if cfg.ViewerCertificate == nil {
		cfg.ViewerCertificate = &cftypes.ViewerCertificate{}
	}
	cfg.ViewerCertificate.ACMCertificateArn = aws.String(acmCertARN)
	cfg.ViewerCertificate.CloudFrontDefaultCertificate = aws.Bool(false)
	if cfg.ViewerCertificate.SSLSupportMethod == "" {
		cfg.ViewerCertificate.SSLSupportMethod = cftypes.SSLSupportMethodSniOnly
	}
	if cfg.ViewerCertificate.MinimumProtocolVersion == "" {
		cfg.ViewerCertificate.MinimumProtocolVersion = cftypes.MinimumProtocolVersionTLSv122021
	}

	_, err = client.UpdateDistribution(ctx, &cloudfront.UpdateDistributionInput{
		Id:                 aws.String(distID),
		DistributionConfig: cfg,
		IfMatch:            out.ETag,
	})
	if err != nil {
		return fmt.Errorf("cloudfront UpdateDistribution %s: %w", distID, err)
	}
	return nil
}
