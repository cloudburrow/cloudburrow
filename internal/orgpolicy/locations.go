package orgpolicy

import (
	"context"
	"regexp"
	"sort"
	"strings"
)

// The value groups of constraints/gcp.resourceLocations, as "Restricting
// Resource Locations" lists them (Value groups, read 2026-11-19:
// https://docs.cloud.google.com/resource-manager/docs/organization-policy/defining-locations).
// Each group holds regions, multi-regions and other groups; a region's
// own group (in:{region}-locations) also holds the region's zones, which
// regionGroup derives rather than lists.
var valueGroups = map[string][]string{
	"in:africa-south1-locations":           {"africa-south1"},
	"in:asia-locations":                    {"in:asia-east2-locations", "in:id-locations", "in:il-locations", "in:in-locations", "in:jp-locations", "in:kr-locations", "in:me-central1-locations", "in:sa-locations", "in:sg-locations", "in:tw-locations", "asia", "asia1", "aws-ap-northeast-2"},
	"in:id-locations":                      {"in:asia-southeast2-locations", "id"},
	"in:il-locations":                      {"in:me-west1-locations", "il"},
	"in:in-locations":                      {"in:asia-south1-locations", "in:asia-south2-locations", "in"},
	"in:jp-locations":                      {"in:asia-northeast1-locations", "in:asia-northeast2-locations", "jp"},
	"in:kr-locations":                      {"in:asia-northeast3-locations", "kr"},
	"in:sa-locations":                      {"in:me-central2-locations", "sa"},
	"in:sg-locations":                      {"in:asia-southeast1-locations", "sg"},
	"in:tw-locations":                      {"in:asia-east1-locations", "tw"},
	"in:th-locations":                      {"in:asia-southeast3-locations", "th"},
	"in:australia-locations":               {"in:australia-southeast1-locations", "in:australia-southeast2-locations", "au"},
	"in:aws-locations":                     {"aws-ap-northeast-2", "aws-ap-southeast-2", "aws-eu-central-1", "aws-us-east-1"},
	"in:azure-locations":                   {"azure-eastus2", "azure-westus2"},
	"in:eu-locations":                      {"in:de-locations", "in:europe-central2-locations", "in:europe-north1-locations", "in:europe-north2-locations", "in:europe-southwest1-locations", "in:europe-west1-locations", "in:europe-west4-locations", "in:europe-west9-locations", "in:it-locations", "EU", "eu", "eur3", "eur4", "eur8", "europe-west"},
	"in:de-locations":                      {"in:europe-west10-locations", "in:europe-west3-locations", "de"},
	"in:it-locations":                      {"in:europe-west12-locations", "in:europe-west8-locations", "it"},
	"in:europe-locations":                  {"in:ch-locations", "in:de-locations", "in:europe-central2-locations", "in:europe-north1-locations", "in:europe-north2-locations", "in:europe-southwest1-locations", "in:europe-west1-locations", "in:europe-west4-locations", "in:europe-west9-locations", "in:gb-locations", "in:it-locations", "EU", "eu", "eur3", "eur4", "eur5", "eur7", "eur8", "europe", "europe-west"},
	"in:ch-locations":                      {"in:europe-west6-locations", "ch"},
	"in:gb-locations":                      {"in:europe-west2-locations", "gb"},
	"in:low-carbon-locations":              {"in:canada-low-carbon-locations", "in:eu-low-carbon-locations", "in:europe-low-carbon-locations", "in:northamerica-low-carbon-locations", "in:southamerica-low-carbon-locations", "in:us-low-carbon-locations"},
	"in:canada-low-carbon-locations":       {"in:northamerica-northeast1-locations", "in:northamerica-northeast2-locations"},
	"in:eu-low-carbon-locations":           {"in:europe-north1-locations", "in:europe-southwest1-locations", "in:europe-west1-locations", "in:europe-west9-locations"},
	"in:europe-low-carbon-locations":       {"in:europe-north1-locations", "in:europe-southwest1-locations", "in:europe-west1-locations", "in:europe-west6-locations", "in:europe-west9-locations"},
	"in:northamerica-low-carbon-locations": {"in:northamerica-northeast1-locations", "in:northamerica-northeast2-locations", "in:us-central1-locations", "in:us-west1-locations"},
	"in:southamerica-low-carbon-locations": {"in:southamerica-east1-locations"},
	"in:us-low-carbon-locations":           {"in:us-central1-locations", "in:us-west1-locations"},
	"in:northamerica-locations":            {"in:canada-locations", "in:northamerica-south1-locations", "in:us-locations", "nam14"},
	"in:canada-locations":                  {"in:northamerica-northeast1-locations", "in:northamerica-northeast2-locations", "ca"},
	"in:us-locations":                      {"in:us-central1-locations", "in:us-central2-locations", "in:us-east1-locations", "in:us-east4-locations", "in:us-east5-locations", "in:us-south1-locations", "in:us-west1-locations", "in:us-west2-locations", "in:us-west3-locations", "in:us-west4-locations", "US", "aws-us-east-1", "azure-eastus2", "nam10", "nam11", "nam12", "nam13", "nam15", "nam3", "nam4", "nam5", "nam6", "nam7", "nam8", "nam9", "us", "us-central"},
	"in:southamerica-locations":            {"in:br-locations", "in:cl-locations"},
	"in:br-locations":                      {"in:southamerica-east1-locations", "br"},
	"in:cl-locations":                      {"in:southamerica-west1-locations", "cl"},
}

var (
	regionRE      = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	zoneRE        = regexp.MustCompile(`^([a-z]+-[a-z]+[0-9]+)-[a-z]$`)
	regionGroupRE = regexp.MustCompile(`^in:([a-z]+-[a-z]+[0-9]+)-locations$`)
)

// KnownGroup reports whether g is a value group: one listed above or a
// region's own group.
func KnownGroup(g string) bool {
	if _, ok := valueGroups[g]; ok {
		return true
	}
	return regionGroupRE.MatchString(g)
}

// inGroup reports whether location is in group g, directly or through
// the groups it holds. A region's group holds the region and its zones.
func inGroup(g, location string, depth int) bool {
	if depth > 8 {
		return false
	}
	if m := regionGroupRE.FindStringSubmatch(g); m != nil {
		loc := strings.ToLower(location)
		if loc == m[1] {
			return true
		}
		if z := zoneRE.FindStringSubmatch(loc); z != nil && z[1] == m[1] {
			return true
		}
	}
	for _, v := range valueGroups[g] {
		if strings.HasPrefix(v, "in:") {
			if inGroup(v, location, depth+1) {
				return true
			}
			continue
		}
		if strings.EqualFold(v, location) {
			return true
		}
	}
	return false
}

// locationMatches reports whether a gcp.resourceLocations value (a
// location, or in:{group}) covers location. A plain value matches that
// location only, case-insensitively (Cloud Storage writes multi-regions
// in capitals: US, EU); "is:" is the explicit form of a plain value.
func locationMatches(value, location string) bool {
	value = strings.TrimPrefix(value, "is:")
	if strings.HasPrefix(value, "in:") {
		return inGroup(value, location, 0)
	}
	return strings.EqualFold(value, location)
}

// LocationKind is how Google's error names a location: ZONE, REGION or
// MULTI_REGION, as the documented message's LOCATION_IN_REQUEST does
// ("Location ZONE:us-east1-b violates constraint ...").
func LocationKind(location string) string {
	l := strings.ToLower(location)
	switch {
	case zoneRE.MatchString(l):
		return "ZONE"
	case regionRE.MatchString(l):
		return "REGION"
	}
	return "MULTI_REGION"
}

// googleRegion reports whether v is a Google Cloud region, not another
// cloud's location the groups list (aws-us-east-1, azure-eastus2).
func googleRegion(v string) bool {
	return regionRE.MatchString(v) && !strings.HasPrefix(v, "aws-") && !strings.HasPrefix(v, "azure-")
}

// knownRegions is every region the value groups name, as a region's own
// group (in:{region}-locations) or as itself.
func knownRegions() []string {
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimPrefix(v, "is:")
		if m := regionGroupRE.FindStringSubmatch(v); m != nil {
			seen[m[1]] = true
		} else if googleRegion(v) {
			seen[v] = true
		}
	}
	for g, vs := range valueGroups {
		add(g)
		for _, v := range vs {
			add(v)
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	return out
}

// AllowedRegions is the regions gcp.resourceLocations allows on resource,
// sorted, and whether a policy restricts them at all: Pub/Sub's
// `--recompute-message-storage-policy` pushes "the message storage policy
// defined in your organization's Resource Location Restriction policy" to
// a topic, and "when no organization policy is in effect, the message
// storage policy allows all regions" (Resource location restriction, read
// 2026-11-20: https://docs.cloud.google.com/pubsub/docs/resource-location-restriction).
// The candidates are the value groups' regions and the regions the
// policy's own values name; a policy allowing all of them restricts none.
func AllowedRegions(ctx context.Context, resource string) ([]string, bool) {
	e := effective(ctx, current(), resource, ResourceLocations)
	if e.Spec == nil {
		return nil, false
	}
	cands := map[string]bool{}
	for _, r := range knownRegions() {
		cands[r] = true
	}
	for _, rule := range e.Spec.GetRules() {
		for _, v := range rule.GetValues().GetAllowedValues() {
			v = strings.TrimPrefix(v, "is:")
			if m := regionGroupRE.FindStringSubmatch(v); m != nil {
				cands[m[1]] = true
			} else if googleRegion(strings.ToLower(v)) {
				cands[strings.ToLower(v)] = true
			}
		}
	}
	var out []string
	for r := range cands {
		if ListAllows(e.Spec, e.Tags, r, locationMatches) {
			out = append(out, r)
		}
	}
	if len(out) == len(cands) {
		return nil, false // a policy that allows every region restricts none
	}
	sort.Strings(out)
	return out, true
}
