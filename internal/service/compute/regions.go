package compute

import (
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// autoRanges are the auto mode subnet ranges, by region, as the
// "Auto mode IPv4 subnet ranges" table of
// https://docs.cloud.google.com/vpc/docs/subnets lists them (re-read
// 2026-11-19). They are also the regions served.
var autoRanges = map[string]string{
	"africa-south1": "10.218.0.0/20", "asia-east1": "10.140.0.0/20", "asia-east2": "10.170.0.0/20",
	"asia-northeast1": "10.146.0.0/20", "asia-northeast2": "10.174.0.0/20", "asia-northeast3": "10.178.0.0/20",
	"asia-south1": "10.160.0.0/20", "asia-south2": "10.190.0.0/20", "asia-southeast1": "10.148.0.0/20",
	"asia-southeast2": "10.184.0.0/20", "asia-southeast3": "10.232.0.0/20", "australia-southeast1": "10.152.0.0/20",
	"australia-southeast2": "10.192.0.0/20", "europe-central2": "10.186.0.0/20", "europe-north1": "10.166.0.0/20",
	"europe-north2": "10.226.0.0/20", "europe-west1": "10.132.0.0/20", "europe-west2": "10.154.0.0/20",
	"europe-west3": "10.156.0.0/20", "europe-west4": "10.164.0.0/20", "europe-west6": "10.172.0.0/20",
	"europe-west8": "10.198.0.0/20", "europe-west9": "10.200.0.0/20", "europe-west10": "10.214.0.0/20",
	"europe-west12": "10.210.0.0/20", "europe-southwest1": "10.204.0.0/20", "me-central1": "10.212.0.0/20",
	"me-central2": "10.216.0.0/20", "me-west1": "10.208.0.0/20", "northamerica-northeast1": "10.162.0.0/20",
	"northamerica-northeast2": "10.188.0.0/20", "northamerica-south1": "10.224.0.0/20", "southamerica-east1": "10.158.0.0/20",
	"southamerica-west1": "10.194.0.0/20", "us-central1": "10.128.0.0/20", "us-east1": "10.142.0.0/20",
	"us-east4": "10.150.0.0/20", "us-east5": "10.202.0.0/20", "us-south1": "10.206.0.0/20",
	"us-west1": "10.138.0.0/20", "us-west2": "10.168.0.0/20", "us-west3": "10.180.0.0/20", "us-west4": "10.182.0.0/20",
}

// Regions are the regions served, sorted.
func Regions() []string {
	out := make([]string, 0, len(autoRanges))
	for r := range autoRanges {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Zones are a region's zones.
func Zones(region string) []string { return locations.Zones(region) }

// IsRegion reports whether r is a region served.
func IsRegion(r string) bool { _, ok := autoRanges[r]; return ok }

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
