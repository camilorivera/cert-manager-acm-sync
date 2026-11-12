package dns

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	WildcardSkip   = "skip"
	WildcardCreate = "create"
	DeleteRetain   = "retain"
	DeleteDelete   = "delete"
)

const (
	ActionCreated         = "created"
	ActionUpdated         = "updated"
	ActionUnchanged       = "unchanged"
	ActionAdopted         = "adopted"
	ActionUnmanagedOK     = "unmanaged_ok"
	ActionConflict        = "conflict"
	ActionSkippedWildcard = "skipped_wildcard"
	ActionSkippedTarget   = "skipped_target"
	ActionNoZone          = "no_zone"
	ActionDeleted         = "deleted"
	ActionError           = "error"
)

type Syncer struct {
	Provider       Provider
	Zones          ZoneSet
	OwnerID        string
	WildcardPolicy string
	DeletePolicy   string
}

type Request struct {
	Resource string
	Names    []string
	Target   string
	Type     RecordType
	TTL      int64
	Previous []string
	Adopt    bool
}

type NameResult struct {
	Name, Action, Detail string
}

type ownership int

const (
	ownerAbsent ownership = iota
	ownerOurs
	ownerForeign
)

type state struct {
	zone    Zone
	records []Record
	nameTXT int
	txt     ownership
	ours    []Record
}

// Sync reconciles the requested names and returns the names this resource owns afterwards.
func (s *Syncer) Sync(ctx context.Context, req Request) ([]NameResult, []string, error) {
	var (
		results []NameResult
		owned   []string
		errs    []error
	)
	current := map[string]bool{}
	prev := map[string]bool{}
	for _, n := range req.Previous {
		prev[normalize(n)] = true
	}

	for _, raw := range req.Names {
		name := normalize(raw)
		if current[name] {
			continue
		}
		current[name] = true
		res, keep, err := s.syncName(ctx, req, name, prev[name])
		results = append(results, res)
		if keep || (err != nil && prev[name]) {
			owned = append(owned, name)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}

	for _, raw := range req.Previous {
		name := normalize(raw)
		if current[name] {
			continue
		}
		current[name] = true
		if s.DeletePolicy != DeleteDelete {
			continue
		}
		res, removed, err := s.remove(ctx, req.Resource, name, req.Target)
		if res != nil {
			results = append(results, *res)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
		if !removed && err != nil {
			owned = append(owned, name)
		}
	}
	return results, owned, errors.Join(errs...)
}

// Cleanup deletes the records and owner TXTs that belong to resource.
func (s *Syncer) Cleanup(ctx context.Context, resource string, names []string) ([]NameResult, error) {
	var (
		results []NameResult
		errs    []error
	)
	for _, raw := range names {
		res, _, err := s.remove(ctx, resource, normalize(raw), "")
		if res != nil {
			results = append(results, *res)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", normalize(raw), err))
		}
	}
	return results, errors.Join(errs...)
}

func (s *Syncer) load(ctx context.Context, resource, name string) (*state, error) {
	zone, ok := s.Zones.Match(name)
	if !ok {
		return nil, nil
	}
	recs, err := s.Provider.GetRecords(ctx, zone, name)
	if err != nil {
		return nil, err
	}
	txts, err := s.Provider.GetRecords(ctx, zone, OwnerName(name))
	if err != nil {
		return nil, err
	}
	st := &state{zone: zone}
	for _, r := range recs {
		switch {
		case r.Type == TypeTXT:
			st.nameTXT++
		case name == zone.Name && CoexistsAtApex(r.Type):
		default:
			st.records = append(st.records, r)
		}
	}
	for _, t := range txts {
		if t.Type != TypeTXT {
			continue
		}
		if st.txt != ownerOurs {
			st.txt = ownerForeign
		}
		if id, res, ok := ParseOwner(t.Target); ok && id == s.OwnerID && res == resource {
			st.txt = ownerOurs
			st.ours = append(st.ours, t)
		}
	}
	return st, nil
}

func (s *Syncer) syncName(ctx context.Context, req Request, name string, tracked bool) (NameResult, bool, error) {
	res := func(action, detail string) NameResult { return NameResult{Name: name, Action: action, Detail: detail} }
	fail := func(err error) (NameResult, bool, error) { return res(ActionError, err.Error()), false, err }

	if name == normalize(req.Target) {
		return res(ActionSkippedTarget, "name equals the record target"), false, nil
	}
	if strings.HasPrefix(name, "*.") && s.WildcardPolicy != WildcardCreate {
		return res(ActionSkippedWildcard, "wildcard policy is skip"), false, nil
	}
	st, err := s.load(ctx, req.Resource, name)
	if err != nil {
		return fail(err)
	}
	if st == nil {
		return res(ActionNoZone, "no configured zone matches"), false, nil
	}

	want := Record{Name: name, Type: req.Type, Target: req.Target, TTL: req.TTL}
	if want.Type == "" {
		want.Type = TypeCNAME
	}
	if name == st.zone.Name {
		want.Type = TypeALIAS
	}
	txt := Record{Name: OwnerName(name), Type: TypeTXT, Target: EncodeOwner(s.OwnerID, req.Resource), TTL: req.TTL}

	raced := func(err error) bool { return errors.Is(err, ErrAlreadyExists) }

	if len(st.records) == 0 {
		if want.Type == TypeCNAME && st.nameTXT > 0 {
			return res(ActionConflict, "name has TXT records, which cannot coexist with a CNAME"), false, nil
		}
		var recs []Record
		switch st.txt {
		case ownerAbsent:
			recs = []Record{txt, want}
		case ownerOurs:
			recs = []Record{want}
		default:
			return res(ActionConflict, "owner TXT belongs to another owner"), false, nil
		}
		if err := s.Provider.CreateRecords(ctx, st.zone, recs); err != nil {
			if raced(err) {
				return res(ActionConflict, "record appeared concurrently"), false, nil
			}
			return fail(err)
		}
		return res(ActionCreated, string(want.Type)+" "+want.Target), true, nil
	}

	var existing *Record
	if len(st.records) == 1 && (st.records[0].Type == TypeCNAME || st.records[0].Type == TypeALIAS) {
		existing = &st.records[0]
	}
	if existing == nil {
		return res(ActionConflict, "name has records that are not a single CNAME or ALIAS"), false, nil
	}

	if st.txt == ownerOurs {
		if existing.Type == want.Type && sameTarget(existing.Target, want.Target) && (existing.TTL == want.TTL || existing.TTL == 0) {
			return res(ActionUnchanged, ""), true, nil
		}
		if !tracked {
			return res(ActionConflict, "existing "+string(existing.Type)+" points to "+existing.Target+" and was not created by this resource"), false, nil
		}
		if err := s.Provider.UpdateRecord(ctx, st.zone, *existing, want); err != nil {
			return fail(err)
		}
		return res(ActionUpdated, string(want.Type)+" "+want.Target), true, nil
	}

	if !sameTarget(existing.Target, want.Target) {
		return res(ActionConflict, "existing "+string(existing.Type)+" points to "+existing.Target), false, nil
	}
	if st.txt == ownerForeign {
		if req.Adopt {
			return res(ActionConflict, "owner TXT belongs to another owner"), false, nil
		}
		return res(ActionUnmanagedOK, "already points at target"), false, nil
	}
	if !req.Adopt {
		return res(ActionUnmanagedOK, "already points at target"), false, nil
	}
	if existing.Type != want.Type || (existing.TTL != want.TTL && existing.TTL != 0) {
		return res(ActionConflict, fmt.Sprintf("existing %s with TTL %d differs from requested %s with TTL %d; match them to adopt",
			existing.Type, existing.TTL, want.Type, want.TTL)), false, nil
	}
	if err := s.Provider.CreateRecords(ctx, st.zone, []Record{txt}); err != nil {
		if raced(err) {
			return res(ActionConflict, "owner TXT appeared concurrently"), false, nil
		}
		return fail(err)
	}
	return res(ActionAdopted, "already points at target"), true, nil
}

// remove deletes the owner TXT when it names this resource, and the record only
// when it is the single CNAME or ALIAS we manage (matching target when known).
func (s *Syncer) remove(ctx context.Context, resource, name, target string) (*NameResult, bool, error) {
	res := func(action, detail string) *NameResult {
		return &NameResult{Name: name, Action: action, Detail: detail}
	}
	st, err := s.load(ctx, resource, name)
	if err != nil {
		return res(ActionError, err.Error()), false, err
	}
	if st == nil {
		return nil, true, nil
	}
	if st.txt != ownerOurs {
		return res(ActionUnmanagedOK, "owner TXT is not ours, left in place"), true, nil
	}
	conflict := ""
	if len(st.records) > 0 {
		single := len(st.records) == 1 && (st.records[0].Type == TypeCNAME || st.records[0].Type == TypeALIAS)
		switch {
		case !single:
			conflict = "name has records that are not a single CNAME or ALIAS, left in place"
		case target != "" && !sameTarget(st.records[0].Target, target):
			conflict = "record points to " + st.records[0].Target + ", left in place"
		default:
			if err := s.Provider.DeleteRecords(ctx, st.zone, st.records); err != nil {
				return res(ActionError, err.Error()), false, err
			}
		}
	}
	if err := s.Provider.DeleteRecords(ctx, st.zone, st.ours); err != nil {
		return res(ActionError, err.Error()), false, err
	}
	if conflict != "" {
		return res(ActionConflict, conflict), true, nil
	}
	return res(ActionDeleted, ""), true, nil
}

func sameTarget(a, b string) bool {
	return normalize(a) == normalize(b)
}
