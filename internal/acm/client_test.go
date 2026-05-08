package acmclient_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/acm"
	"github.com/aws/aws-sdk-go-v2/service/acm/types"
	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	acmclient "github.com/camilorivera/cert-manager-acm-sync/internal/acm"
)

func TestImportWithRetry_Success(t *testing.T) {
	m := &acmclient.MockACMAPI{}
	m.On("ImportCertificate", mock.Anything, mock.Anything).
		Return(&acm.ImportCertificateOutput{CertificateArn: aws.String("arn:aws:acm:us-east-1:123:certificate/abc")}, nil)

	out, err := acmclient.ImportWithRetry(context.Background(), m, &acm.ImportCertificateInput{})
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:acm:us-east-1:123:certificate/abc", aws.ToString(out.CertificateArn))
	m.AssertNumberOfCalls(t, "ImportCertificate", 1)
}

func TestImportWithRetry_NonThrottleErrorReturnedImmediately(t *testing.T) {
	m := &acmclient.MockACMAPI{}
	m.On("ImportCertificate", mock.Anything, mock.Anything).
		Return(nil, errors.New("some other error"))

	_, err := acmclient.ImportWithRetry(context.Background(), m, &acm.ImportCertificateInput{})
	assert.Error(t, err)
	m.AssertNumberOfCalls(t, "ImportCertificate", 1)
}

func TestImportWithRetry_ThrottleRetriesAndSucceeds(t *testing.T) {
	throttleErr := &smithy.GenericAPIError{Code: "Throttling", Message: "rate exceeded"}
	successOut := &acm.ImportCertificateOutput{CertificateArn: aws.String("arn:aws:acm:us-east-1:123:certificate/xyz")}

	m := &acmclient.MockACMAPI{}
	m.On("ImportCertificate", mock.Anything, mock.Anything).
		Return(nil, throttleErr).Once()
	m.On("ImportCertificate", mock.Anything, mock.Anything).
		Return(nil, throttleErr).Once()
	m.On("ImportCertificate", mock.Anything, mock.Anything).
		Return(successOut, nil).Once()

	out, err := acmclient.ImportWithRetry(context.Background(), m, &acm.ImportCertificateInput{})
	require.NoError(t, err)
	assert.NotNil(t, out)
	m.AssertNumberOfCalls(t, "ImportCertificate", 3)
}

func TestImportWithRetry_ReimportUsesExistingARN(t *testing.T) {
	existingARN := "arn:aws:acm:us-east-1:123:certificate/existing"
	m := &acmclient.MockACMAPI{}
	m.On("ImportCertificate", mock.Anything, mock.MatchedBy(func(in *acm.ImportCertificateInput) bool {
		return aws.ToString(in.CertificateArn) == existingARN
	})).Return(&acm.ImportCertificateOutput{CertificateArn: aws.String(existingARN)}, nil)

	input := &acm.ImportCertificateInput{
		CertificateArn: aws.String(existingARN),
	}
	out, err := acmclient.ImportWithRetry(context.Background(), m, input)
	require.NoError(t, err)
	assert.Equal(t, existingARN, aws.ToString(out.CertificateArn))
}

func TestIsNotFound(t *testing.T) {
	rnfe := &types.ResourceNotFoundException{Message: aws.String("not found")}
	assert.True(t, acmclient.IsNotFound(rnfe))
	assert.False(t, acmclient.IsNotFound(errors.New("other")))
}
