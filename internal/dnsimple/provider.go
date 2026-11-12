package dnsimple

import (
	"context"
	"fmt"
	"strings"

	"github.com/camilorivera/cert-manager-acm-sync/internal/dns"
)

const (
	typeA    = "A"
	typeAAAA = "AAAA"

	aliasCompanionPrefix = "ALIAS for "
)

// Provider implements dns.Provider on top of the DNSimple records API.
type Provider struct {
	api API
}

func NewProvider(api API) *Provider {
	return &Provider{api: api}
}

func (p *Provider) Name() string { return "dnsimple" }

func zoneRef(zone dns.Zone) string {
	if zone.ID != "" {
		return zone.ID
	}
	return zone.Name
}

func relativeName(zone dns.Zone, fqdn string) (string, error) {
	fqdn = strings.TrimSuffix(strings.ToLower(fqdn), ".")
	zoneName := strings.TrimSuffix(strings.ToLower(zone.Name), ".")
	if fqdn == zoneName {
		return "", nil
	}
	rel, ok := strings.CutSuffix(fqdn, "."+zoneName)
	if !ok {
		return "", fmt.Errorf("dnsimple: %s is not inside zone %s", fqdn, zone.Name)
	}
	return rel, nil
}

func trimTarget(t dns.RecordType, v string) string {
	if t == dns.TypeTXT {
		return strings.Trim(v, `"`)
	}
	return strings.TrimSuffix(strings.ToLower(v), ".")
}

func relevant(r Record) bool {
	if r.SystemRecord {
		return false
	}
	switch r.Type {
	case string(dns.TypeCNAME), string(dns.TypeALIAS), typeA, typeAAAA:
		return true
	case string(dns.TypeTXT):
		return r.ParentID == 0 && !strings.HasPrefix(r.Content, aliasCompanionPrefix)
	}
	return false
}

func toRecord(zone dns.Zone, r Record) dns.Record {
	t := dns.RecordType(r.Type)
	name := zone.Name
	if r.Name != "" {
		name = r.Name + "." + zone.Name
	}
	return dns.Record{Name: strings.ToLower(name), Type: t, Target: trimTarget(t, r.Content), TTL: r.TTL}
}

func toInput(rel string, rec dns.Record) RecordInput {
	return RecordInput{Name: rel, Type: string(rec.Type), Content: rec.Target, TTL: rec.TTL}
}

func (p *Provider) list(ctx context.Context, zone dns.Zone, fqdn string) (string, []Record, error) {
	rel, err := relativeName(zone, fqdn)
	if err != nil {
		return "", nil, err
	}
	all, err := p.api.ListRecords(ctx, zoneRef(zone), rel)
	if err != nil {
		return "", nil, err
	}
	var out []Record
	for _, r := range all {
		if relevant(r) {
			out = append(out, r)
		}
	}
	return rel, out, nil
}

func (p *Provider) GetRecords(ctx context.Context, zone dns.Zone, fqdn string) ([]dns.Record, error) {
	_, recs, err := p.list(ctx, zone, fqdn)
	if err != nil {
		return nil, err
	}
	out := make([]dns.Record, 0, len(recs))
	for _, r := range recs {
		out = append(out, toRecord(zone, r))
	}
	return out, nil
}

func (p *Provider) CreateRecords(ctx context.Context, zone dns.Zone, recs []dns.Record) error {
	for _, rec := range recs {
		_, existing, err := p.list(ctx, zone, rec.Name)
		if err != nil {
			return err
		}
		for _, e := range existing {
			if rec.Type != dns.TypeTXT || e.Type == string(dns.TypeTXT) {
				return fmt.Errorf("%w: %s %s", dns.ErrAlreadyExists, e.Type, rec.Name)
			}
		}
	}
	for _, rec := range recs {
		rel, err := relativeName(zone, rec.Name)
		if err != nil {
			return err
		}
		if _, err := p.api.CreateRecord(ctx, zoneRef(zone), toInput(rel, rec)); err != nil {
			return fmt.Errorf("create %s %s: %w", rec.Type, rec.Name, err)
		}
	}
	return nil
}

func (p *Provider) find(ctx context.Context, zone dns.Zone, rec dns.Record) (string, *Record, error) {
	rel, existing, err := p.list(ctx, zone, rec.Name)
	if err != nil {
		return "", nil, err
	}
	for i, e := range existing {
		if e.Type == string(rec.Type) && trimTarget(rec.Type, e.Content) == trimTarget(rec.Type, rec.Target) {
			return rel, &existing[i], nil
		}
	}
	return rel, nil, nil
}

func (p *Provider) UpdateRecord(ctx context.Context, zone dns.Zone, old, updated dns.Record) error {
	rel, found, err := p.find(ctx, zone, old)
	if err != nil {
		return err
	}
	if found == nil {
		return fmt.Errorf("dnsimple: record %s %s %s not found for update", old.Type, old.Name, old.Target)
	}
	if old.Type != updated.Type {
		if err := p.api.DeleteRecord(ctx, zoneRef(zone), found.ID); err != nil && !IsNotFound(err) {
			return fmt.Errorf("delete %s %s: %w", old.Type, old.Name, err)
		}
		if _, err := p.api.CreateRecord(ctx, zoneRef(zone), toInput(rel, updated)); err != nil {
			return fmt.Errorf("create %s %s: %w", updated.Type, updated.Name, err)
		}
		return nil
	}
	in := toInput(rel, updated)
	in.Type = ""
	if _, err := p.api.UpdateRecord(ctx, zoneRef(zone), found.ID, in); err != nil {
		return fmt.Errorf("update %s %s: %w", updated.Type, updated.Name, err)
	}
	return nil
}

func (p *Provider) DeleteRecords(ctx context.Context, zone dns.Zone, recs []dns.Record) error {
	for _, rec := range recs {
		_, found, err := p.find(ctx, zone, rec)
		if err != nil {
			return err
		}
		if found == nil {
			continue
		}
		if err := p.api.DeleteRecord(ctx, zoneRef(zone), found.ID); err != nil && !IsNotFound(err) {
			return fmt.Errorf("delete %s %s: %w", rec.Type, rec.Name, err)
		}
	}
	return nil
}
