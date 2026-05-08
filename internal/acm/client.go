package acmclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/acm"
	"github.com/aws/aws-sdk-go-v2/service/acm/types"
	smithy "github.com/aws/smithy-go"
)

// ACMAPI is the subset of acm.Client methods the controller uses.
// Defining an interface allows the ACM client to be mocked in tests.
type ACMAPI interface {
	ImportCertificate(ctx context.Context, params *acm.ImportCertificateInput, optFns ...func(*acm.Options)) (*acm.ImportCertificateOutput, error)
	DescribeCertificate(ctx context.Context, params *acm.DescribeCertificateInput, optFns ...func(*acm.Options)) (*acm.DescribeCertificateOutput, error)
}

// Pool maintains a lazily-initialised, thread-safe map of region → ACMAPI.
// All clients share the same base AWS config (IRSA credentials) but each
// uses a different region.
type Pool struct {
	baseCfg aws.Config
	mu      sync.RWMutex
	clients map[string]ACMAPI
}

// NewPool creates a Pool from an AWS config loaded at startup (e.g. via IRSA).
func NewPool(cfg aws.Config) *Pool {
	return &Pool{
		baseCfg: cfg,
		clients: make(map[string]ACMAPI),
	}
}

// ClientForRegion returns (or creates) a client for the given region.
// It uses double-checked locking so only one client is created per region.
func (p *Pool) ClientForRegion(region string) ACMAPI {
	p.mu.RLock()
	c, ok := p.clients[region]
	p.mu.RUnlock()
	if ok {
		return c
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok = p.clients[region]; ok {
		return c
	}
	regionalCfg := p.baseCfg.Copy()
	regionalCfg.Region = region
	c = acm.NewFromConfig(regionalCfg)
	p.clients[region] = c
	return c
}

// ImportWithRetry calls acm:ImportCertificate with exponential backoff on
// throttle errors. Non-throttle errors are returned immediately.
func ImportWithRetry(ctx context.Context, client ACMAPI, input *acm.ImportCertificateInput) (*acm.ImportCertificateOutput, error) {
	const maxAttempts = 5
	base := time.Second

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		out, err := client.ImportCertificate(ctx, input)
		if err == nil {
			return out, nil
		}
		if !isThrottleError(err) {
			return nil, err
		}
		lastErr = err
		if attempt < maxAttempts-1 {
			wait := base << uint(attempt)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("ACM import failed after %d attempts: %w", maxAttempts, lastErr)
}

// IsNotFound returns true when the ACM error indicates the certificate ARN
// does not exist (was deleted externally).
func IsNotFound(err error) bool {
	var nfe *types.ResourceNotFoundException
	return errors.As(err, &nfe)
}

func isThrottleError(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		c := ae.ErrorCode()
		return c == "Throttling" || c == "ThrottlingException" ||
			c == "TooManyRequestsException" || c == "RequestLimitExceeded"
	}
	return false
}
