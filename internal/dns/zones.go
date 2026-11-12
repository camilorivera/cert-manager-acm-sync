package dns

import (
	"fmt"
	"strings"
)

type ZoneSet []Zone

func normalize(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// ParseZones parses entries of the form name[=id].
func ParseZones(entries []string) (ZoneSet, error) {
	seen := map[string]bool{}
	var zs ZoneSet
	for _, e := range entries {
		name, id, _ := strings.Cut(e, "=")
		name = normalize(name)
		if name == "" {
			return nil, fmt.Errorf("dns: empty zone name in %q", e)
		}
		if seen[name] {
			return nil, fmt.Errorf("dns: duplicate zone %q", name)
		}
		seen[name] = true
		zs = append(zs, Zone{Name: name, ID: strings.TrimSpace(id)})
	}
	return zs, nil
}

// Match returns the zone with the longest name that is fqdn or one of its parent domains.
func (zs ZoneSet) Match(fqdn string) (Zone, bool) {
	fqdn = normalize(fqdn)
	var best Zone
	found := false
	for _, z := range zs {
		if fqdn != z.Name && !strings.HasSuffix(fqdn, "."+z.Name) {
			continue
		}
		if !found || len(z.Name) > len(best.Name) {
			best, found = z, true
		}
	}
	return best, found
}
