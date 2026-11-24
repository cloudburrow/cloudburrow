package compute

import (
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// autoRanges are the auto mode subnet ranges, by region
// (locations.AutoSubnetRanges): also the regions served.
var autoRanges = locations.AutoSubnetRanges

// Regions are the regions served, sorted.
func Regions() []string { return locations.ComputeRegions() }

// Zones are a region's zones.
func Zones(region string) []string { return locations.Zones(region) }

// IsRegion reports whether r is a region served.
func IsRegion(r string) bool { return locations.IsComputeRegion(r) }

// IsZone reports whether z is a zone of a region served.
func IsZone(z string) bool {
	i := strings.LastIndex(z, "-")
	if i < 0 {
		return false
	}
	for _, x := range Zones(z[:i]) {
		if x == z {
			return true
		}
	}
	return false
}

func regionObj(project, region string) obj {
	zones := []any{}
	for _, z := range Zones(region) {
		zones = append(zones, "projects/"+project+"/zones/"+z)
	}
	return obj{"kind": "compute#region", "id": stableID(region), "creationTimestamp": "1969-12-31T16:00:00.000-08:00",
		"name": region, "description": region, "status": "UP", "zones": zones,
		"selfLink": "projects/" + project + "/regions/" + region, "supportsPzs": false,
		"quotas": []any{
			obj{"metric": "SUBNETWORKS", "limit": 175.0, "usage": 0.0},
			obj{"metric": "STATIC_ADDRESSES", "limit": 8.0, "usage": 0.0},
			obj{"metric": "IN_USE_ADDRESSES", "limit": 8.0, "usage": 0.0},
		}}
}

func zoneObj(project, zone string) obj {
	region := zone[:strings.LastIndex(zone, "-")]
	return obj{"kind": "compute#zone", "id": stableID(zone), "creationTimestamp": "1969-12-31T16:00:00.000-08:00",
		"name": zone, "description": zone, "status": "UP", "region": "projects/" + project + "/regions/" + region,
		"selfLink": "projects/" + project + "/zones/" + zone, "supportsPzs": false,
		"availableCpuPlatforms": []any{"Intel Cascade Lake", "AMD Milan"}}
}

// stableID is a fixed numeric ID for a name.
func stableID(name string) string {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= 1099511628211
	}
	return strconvU(h >> 1)
}
