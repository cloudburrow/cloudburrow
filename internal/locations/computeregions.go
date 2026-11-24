package locations

import "sort"

// AutoSubnetRanges are the auto mode subnet ranges, by region, as the
// "Auto mode IPv4 subnet ranges" table of
// https://docs.cloud.google.com/vpc/docs/subnets lists them (re-read
// 2026-11-19). They are also the regions Compute Engine serves, which the
// networking APIs beside it (Certificate Manager, IAP, Network Security,
// Network Services, Serverless VPC Access) serve too (#2153, #2160).
var AutoSubnetRanges = map[string]string{
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

// ComputeRegions are the regions Compute Engine serves, sorted.
func ComputeRegions() []string {
	out := make([]string, 0, len(AutoSubnetRanges))
	for r := range AutoSubnetRanges {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// IsComputeRegion reports whether r is a region Compute Engine serves.
func IsComputeRegion(r string) bool { _, ok := AutoSubnetRanges[r]; return ok }
