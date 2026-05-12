package annotations

const (
	Enabled                   = "acm.sync/enabled"
	ARN                       = "acm.sync/arn"
	Fingerprint               = "acm.sync/fingerprint"
	Region                    = "acm.sync/region"
	LastSync                  = "acm.sync/last-sync"
	CloudFrontDistributionARN = "acm.sync/cloudfront-distribution-arn"

	enabledValue = "true"
)

// controllerWrittenAnnotations is the closed set of keys the controller writes
// back onto Secrets/Certificates. StripACMAnnotations removes only these so
// that user-set keys (region, cloudfront-distribution-arn) are treated as real
// annotation changes by the event predicates.
var controllerWrittenAnnotations = map[string]bool{
	ARN:         true,
	Fingerprint: true,
	LastSync:    true,
}

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

func GetCloudFrontDistributionARN(ann map[string]string) string {
	return ann[CloudFrontDistributionARN]
}

// StripACMAnnotations returns a copy of the map with only the controller-written
// acm.sync/* keys removed. User-set keys (acm.sync/region,
// acm.sync/cloudfront-distribution-arn, etc.) are preserved so that changes to
// them are detected by the event predicates and trigger a reconcile.
func StripACMAnnotations(ann map[string]string) map[string]string {
	if ann == nil {
		return nil
	}
	out := make(map[string]string, len(ann))
	for k, v := range ann {
		if controllerWrittenAnnotations[k] {
			continue
		}
		out[k] = v
	}
	return out
}
