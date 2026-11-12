package dns

import (
	"strings"
)

const (
	ownerPrefix = "_acm-sync."
	heritage    = "cert-manager-acm-sync"
)

// OwnerName returns the FQDN of the owner TXT record guarding fqdn.
func OwnerName(fqdn string) string {
	fqdn = normalize(fqdn)
	if parent, ok := strings.CutPrefix(fqdn, "*."); ok {
		return ownerPrefix + "_wildcard." + parent
	}
	return ownerPrefix + fqdn
}

func EncodeOwner(ownerID, resource string) string {
	return "heritage=" + heritage + ",owner=" + ownerID + ",resource=" + resource
}

// ParseOwner decodes an owner TXT value, tolerating surrounding quotes.
func ParseOwner(value string) (ownerID, resource string, ok bool) {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	var herit bool
	for _, kv := range strings.Split(value, ",") {
		k, v, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		switch k {
		case "heritage":
			herit = v == heritage
		case "owner":
			ownerID = v
		case "resource":
			resource = v
		}
	}
	if !herit || ownerID == "" || resource == "" {
		return "", "", false
	}
	return ownerID, resource, true
}
