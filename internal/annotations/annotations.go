package annotations

const (
	Enabled     = "acm.sync/enabled"
	ARN         = "acm.sync/arn"
	Fingerprint = "acm.sync/fingerprint"
	Region      = "acm.sync/region"
	LastSync    = "acm.sync/last-sync"

	enabledValue = "true"
)

func IsEnabled(ann map[string]string) bool {
	return ann[Enabled] == enabledValue
}

func GetARN(ann map[string]string) string {
	return ann[ARN]
}

func GetFingerprint(ann map[string]string) string {
	return ann[Fingerprint]
}

func GetRegion(ann map[string]string) string {
	return ann[Region]
}

// StripACMAnnotations returns a copy of the map with all acm.sync/* keys removed.
// Used by the predicate to detect whether an Update event was caused solely by
// the controller patching its own annotations back onto the Secret.
func StripACMAnnotations(ann map[string]string) map[string]string {
	if ann == nil {
		return nil
	}
	out := make(map[string]string, len(ann))
	for k, v := range ann {
		if len(k) >= 9 && k[:9] == "acm.sync/" {
			continue
		}
		out[k] = v
	}
	return out
}
