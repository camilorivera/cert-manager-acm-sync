package acmclient

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/acm"
	"github.com/stretchr/testify/mock"
)

// MockACMAPI is a testify mock that satisfies the ACMAPI interface.
type MockACMAPI struct {
	mock.Mock
}

func (m *MockACMAPI) ImportCertificate(ctx context.Context, params *acm.ImportCertificateInput, _ ...func(*acm.Options)) (*acm.ImportCertificateOutput, error) {
	args := m.Called(ctx, params)
	if out := args.Get(0); out != nil {
		return out.(*acm.ImportCertificateOutput), args.Error(1)
	}
	return nil, args.Error(1)
}

func (m *MockACMAPI) DescribeCertificate(ctx context.Context, params *acm.DescribeCertificateInput, _ ...func(*acm.Options)) (*acm.DescribeCertificateOutput, error) {
	args := m.Called(ctx, params)
	if out := args.Get(0); out != nil {
		return out.(*acm.DescribeCertificateOutput), args.Error(1)
	}
	return nil, args.Error(1)
}

// MockPool wraps MockACMAPI so it satisfies the pool contract the controller expects.
type MockPool struct {
	Client ACMAPI
}

func (mp *MockPool) ClientForRegion(_ string) ACMAPI {
	return mp.Client
}
