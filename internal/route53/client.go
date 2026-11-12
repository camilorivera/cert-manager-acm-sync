package route53client

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	smithy "github.com/aws/smithy-go"

	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
)

const (
	cloudFrontHostedZoneID = "Z2FDTNDATAQYW2"
	cloudFrontSuffix       = ".cloudfront.net"

	typeA    dns.RecordType = "A"
	typeAAAA dns.RecordType = "AAAA"
)

// Route53API is the subset of route53.Client methods the provider uses.
type Route53API interface {
	ListResourceRecordSets(ctx context.Context, params *route53.ListResourceRecordSetsInput, optFns ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error)
	ChangeResourceRecordSets(ctx context.Context, params *route53.ChangeResourceRecordSetsInput, optFns ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error)
}

// NewClient returns a Route53 client built from the base AWS config.
func NewClient(cfg aws.Config) Route53API {
	return route53.NewFromConfig(cfg)
}

// Provider implements dns.Provider on top of Route53.
type Provider struct {
	client Route53API
}

func NewProvider(client Route53API) *Provider {
	return &Provider{client: client}
}

func (p *Provider) Name() string { return "route53" }

func normalizeName(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), `\052`, "*")
	return strings.TrimSuffix(s, ".")
}

func apiName(s string) string {
	return normalizeName(s) + "."
}

func unquoteTXT(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		v = v[1 : len(v)-1]
	}
	return strings.ReplaceAll(strings.ReplaceAll(v, `\"`, `"`), `\\`, `\`)
}

func quoteTXT(v string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
}

func aliasHostedZone(zone dns.Zone, target string) (string, error) {
	t := normalizeName(target)
	z := normalizeName(zone.Name)
	switch {
	case strings.HasSuffix(t, cloudFrontSuffix):
		return cloudFrontHostedZoneID, nil
	case t == z || strings.HasSuffix(t, "."+z):
		return zone.ID, nil
	}
	return "", fmt.Errorf("route53: unsupported alias target %q", target)
}

func mapError(op string, err error) error {
	var invalid *r53types.InvalidChangeBatch
	if errors.As(err, &invalid) {
		msgs := append([]string{aws.ToString(invalid.Message)}, invalid.Messages...)
		for _, m := range msgs {
			if strings.Contains(strings.ToLower(m), "already exists") {
				return fmt.Errorf("route53 %s: %w: %w", op, dns.ErrAlreadyExists, err)
			}
		}
	}
	var pending *r53types.PriorRequestNotComplete
	if errors.As(err, &pending) {
		return fmt.Errorf("route53 %s: %w: %w", op, dns.ErrRetryable, err)
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "Throttling", "ThrottlingException", "PriorRequestNotComplete", "RequestLimitExceeded":
			return fmt.Errorf("route53 %s: %w: %w", op, dns.ErrRetryable, err)
		}
	}
	return fmt.Errorf("route53 %s: %w", op, err)
}

func (p *Provider) listSets(ctx context.Context, zone dns.Zone, fqdn string) ([]r53types.ResourceRecordSet, error) {
	name := normalizeName(fqdn)
	in := &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(zone.ID),
		StartRecordName: aws.String(strings.ReplaceAll(apiName(name), "*", `\052`)),
	}
	var out []r53types.ResourceRecordSet
	for {
		resp, err := p.client.ListResourceRecordSets(ctx, in)
		if err != nil {
			return nil, mapError("ListResourceRecordSets", err)
		}
		for _, rs := range resp.ResourceRecordSets {
			if normalizeName(aws.ToString(rs.Name)) != name {
				return out, nil
			}
			out = append(out, rs)
		}
		if !resp.IsTruncated {
			return out, nil
		}
		in.StartRecordName = resp.NextRecordName
		in.StartRecordType = resp.NextRecordType
		in.StartRecordIdentifier = resp.NextRecordIdentifier
	}
}

func (p *Provider) GetRecords(ctx context.Context, zone dns.Zone, fqdn string) ([]dns.Record, error) {
	sets, err := p.listSets(ctx, zone, fqdn)
	if err != nil {
		return nil, err
	}
	name := normalizeName(fqdn)
	var out []dns.Record
	seenAlias := map[string]bool{}
	apex := name == normalizeName(zone.Name)
	for _, rs := range sets {
		ttl := aws.ToInt64(rs.TTL)
		if apex && (rs.Type == r53types.RRTypeSoa || rs.Type == r53types.RRTypeNs) {
			continue
		}
		if rs.AliasTarget != nil {
			target := normalizeName(aws.ToString(rs.AliasTarget.DNSName))
			if rs.Type != r53types.RRTypeA && rs.Type != r53types.RRTypeAaaa {
				out = append(out, dns.Record{Name: name, Type: dns.RecordType(rs.Type), Target: target})
				continue
			}
			if !seenAlias[target] {
				seenAlias[target] = true
				out = append(out, dns.Record{Name: name, Type: dns.TypeALIAS, Target: target})
			}
			continue
		}
		for _, rr := range rs.ResourceRecords {
			v := aws.ToString(rr.Value)
			switch rs.Type {
			case r53types.RRTypeCname:
				out = append(out, dns.Record{Name: name, Type: dns.TypeCNAME, Target: normalizeName(v), TTL: ttl})
			case r53types.RRTypeTxt:
				out = append(out, dns.Record{Name: name, Type: dns.TypeTXT, Target: unquoteTXT(v), TTL: ttl})
			case r53types.RRTypeA:
				out = append(out, dns.Record{Name: name, Type: typeA, Target: v, TTL: ttl})
			case r53types.RRTypeAaaa:
				out = append(out, dns.Record{Name: name, Type: typeAAAA, Target: v, TTL: ttl})
			default:
				out = append(out, dns.Record{Name: name, Type: dns.RecordType(rs.Type), Target: v, TTL: ttl})
			}
		}
	}
	return out, nil
}

func recordSets(zone dns.Zone, rec dns.Record) ([]r53types.ResourceRecordSet, error) {
	name := aws.String(apiName(rec.Name))
	switch rec.Type {
	case dns.TypeCNAME:
		return []r53types.ResourceRecordSet{{
			Name:            name,
			Type:            r53types.RRTypeCname,
			TTL:             aws.Int64(rec.TTL),
			ResourceRecords: []r53types.ResourceRecord{{Value: aws.String(normalizeName(rec.Target))}},
		}}, nil
	case dns.TypeTXT:
		return []r53types.ResourceRecordSet{{
			Name:            name,
			Type:            r53types.RRTypeTxt,
			TTL:             aws.Int64(rec.TTL),
			ResourceRecords: []r53types.ResourceRecord{{Value: aws.String(quoteTXT(rec.Target))}},
		}}, nil
	case dns.TypeALIAS:
		hz, err := aliasHostedZone(zone, rec.Target)
		if err != nil {
			return nil, err
		}
		var out []r53types.ResourceRecordSet
		for _, t := range []r53types.RRType{r53types.RRTypeA, r53types.RRTypeAaaa} {
			out = append(out, r53types.ResourceRecordSet{
				Name: name,
				Type: t,
				AliasTarget: &r53types.AliasTarget{
					DNSName:              aws.String(normalizeName(rec.Target)),
					HostedZoneId:         aws.String(hz),
					EvaluateTargetHealth: false,
				},
			})
		}
		return out, nil
	}
	return nil, fmt.Errorf("route53: unsupported record type %q", rec.Type)
}

func changes(action r53types.ChangeAction, sets []r53types.ResourceRecordSet) []r53types.Change {
	out := make([]r53types.Change, 0, len(sets))
	for _, rs := range sets {
		out = append(out, r53types.Change{Action: action, ResourceRecordSet: &rs})
	}
	return out
}

func (p *Provider) apply(ctx context.Context, zone dns.Zone, cs []r53types.Change, op string) error {
	if len(cs) == 0 {
		return nil
	}
	_, err := p.client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zone.ID),
		ChangeBatch:  &r53types.ChangeBatch{Changes: cs},
	})
	if err != nil {
		return mapError(op, err)
	}
	return nil
}

func (p *Provider) CreateRecords(ctx context.Context, zone dns.Zone, recs []dns.Record) error {
	var cs []r53types.Change
	for _, rec := range recs {
		sets, err := recordSets(zone, rec)
		if err != nil {
			return err
		}
		cs = append(cs, changes(r53types.ChangeActionCreate, sets)...)
	}
	return p.apply(ctx, zone, cs, "ChangeResourceRecordSets CREATE")
}

// deleteChanges builds exact-value deletions for rec from the live record sets.
// A TXT set holding other values is rewritten without rec's value instead of deleted.
func (p *Provider) deleteChanges(ctx context.Context, zone dns.Zone, rec dns.Record) ([]r53types.Change, error) {
	sets, err := p.listSets(ctx, zone, rec.Name)
	if err != nil {
		return nil, err
	}
	var out []r53types.Change
	for _, rs := range sets {
		switch {
		case rec.Type == dns.TypeALIAS:
			if rs.AliasTarget != nil && normalizeName(aws.ToString(rs.AliasTarget.DNSName)) == normalizeName(rec.Target) {
				out = append(out, r53types.Change{Action: r53types.ChangeActionDelete, ResourceRecordSet: &rs})
			}
		case rs.AliasTarget == nil && string(rs.Type) == string(rec.Type):
			var rest []r53types.ResourceRecord
			matched := false
			for _, rr := range rs.ResourceRecords {
				if recordValueMatches(rec, aws.ToString(rr.Value)) {
					matched = true
					continue
				}
				rest = append(rest, rr)
			}
			if !matched {
				continue
			}
			if len(rest) == 0 {
				out = append(out, r53types.Change{Action: r53types.ChangeActionDelete, ResourceRecordSet: &rs})
				continue
			}
			kept := rs
			kept.ResourceRecords = rest
			out = append(out, r53types.Change{Action: r53types.ChangeActionUpsert, ResourceRecordSet: &kept})
		}
	}
	return out, nil
}

func recordValueMatches(rec dns.Record, v string) bool {
	if rec.Type == dns.TypeTXT {
		return unquoteTXT(v) == unquoteTXT(rec.Target)
	}
	return normalizeName(v) == normalizeName(rec.Target)
}

func (p *Provider) UpdateRecord(ctx context.Context, zone dns.Zone, old, updated dns.Record) error {
	var cs []r53types.Change
	if old.Type != updated.Type {
		del, err := p.deleteChanges(ctx, zone, old)
		if err != nil {
			return err
		}
		cs = append(cs, del...)
	}
	sets, err := recordSets(zone, updated)
	if err != nil {
		return err
	}
	cs = append(cs, changes(r53types.ChangeActionUpsert, sets)...)
	return p.apply(ctx, zone, cs, "ChangeResourceRecordSets UPSERT")
}

func (p *Provider) DeleteRecords(ctx context.Context, zone dns.Zone, recs []dns.Record) error {
	var cs []r53types.Change
	for _, rec := range recs {
		del, err := p.deleteChanges(ctx, zone, rec)
		if err != nil {
			return err
		}
		cs = append(cs, del...)
	}
	return p.apply(ctx, zone, cs, "ChangeResourceRecordSets DELETE")
}
