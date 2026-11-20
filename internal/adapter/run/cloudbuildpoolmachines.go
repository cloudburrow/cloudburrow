package run

// A private pool's machine types by region (#2073), as Google's "Private
// pool config file schema" lists them under machineType: the e2, n2d and
// c3 machine types a pool can use ("specify one of the following machine
// types"), and the table of "the availability of machine types by region",
// a column of regions per family (c3-standard-*, c3-highcpu-*,
// c3-highmem-*, e2-*, n2d-*). With no machine type, "Cloud Build uses the
// default value of e2-medium". CreateWorkerPool and UpdateWorkerPool refuse
// a machine type the list does not have, or that the pool's region does not
// offer; the console's Series and Machine type drop-downs offer the
// region's.

import (
	"slices"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// PoolDefaultMachineType is the machine type of a pool that names none.
const PoolDefaultMachineType = "e2-medium"

// PoolSeries are the machine series in Google's order.
var PoolSeries = []string{"e2", "n2d", "c3"}

// poolMachineTypes are each series' machine types, as the schema's table
// lists them.
var poolMachineTypes = map[string][]string{
	"e2": {"e2-medium", "e2-standard-2", "e2-standard-4", "e2-standard-8", "e2-standard-16", "e2-standard-32",
		"e2-highmem-2", "e2-highmem-4", "e2-highmem-8", "e2-highmem-16",
		"e2-highcpu-2", "e2-highcpu-4", "e2-highcpu-8", "e2-highcpu-16", "e2-highcpu-32"},
	"n2d": {"n2d-standard-2", "n2d-standard-4", "n2d-standard-8", "n2d-standard-16", "n2d-standard-32", "n2d-standard-48",
		"n2d-standard-64", "n2d-standard-80", "n2d-standard-96", "n2d-standard-128", "n2d-standard-224",
		"n2d-highmem-2", "n2d-highmem-4", "n2d-highmem-8", "n2d-highmem-16", "n2d-highmem-32", "n2d-highmem-48",
		"n2d-highmem-64", "n2d-highmem-80", "n2d-highmem-96",
		"n2d-highcpu-2", "n2d-highcpu-4", "n2d-highcpu-8", "n2d-highcpu-16", "n2d-highcpu-32", "n2d-highcpu-48",
		"n2d-highcpu-64", "n2d-highcpu-80", "n2d-highcpu-96", "n2d-highcpu-128", "n2d-highcpu-224"},
	"c3": {"c3-standard-4", "c3-standard-8", "c3-standard-22", "c3-standard-44", "c3-standard-88", "c3-standard-176",
		"c3-highcpu-4", "c3-highcpu-8", "c3-highcpu-22", "c3-highcpu-44", "c3-highcpu-88", "c3-highcpu-176",
		"c3-highmem-4", "c3-highmem-8", "c3-highmem-22", "c3-highmem-44", "c3-highmem-88", "c3-highmem-176"},
}

// poolFamilyRegions is the availability table: each family's regions.
var poolFamilyRegions = map[string][]string{
	"e2": {"africa-south1", "asia-east1", "asia-east2", "asia-northeast1", "asia-northeast2", "asia-northeast3", "asia-south1",
		"asia-south2", "asia-southeast1", "asia-southeast2", "australia-southeast1", "australia-southeast2", "europe-central2",
		"europe-north1", "europe-north2", "europe-southwest1", "europe-west1", "europe-west2", "europe-west3", "europe-west4",
		"europe-west6", "europe-west8", "europe-west9", "europe-west10", "europe-west12", "me-central1", "me-central2", "me-west1",
		"northamerica-northeast1", "northamerica-northeast2", "northamerica-south1", "southamerica-east1", "southamerica-west1",
		"us-central1", "us-east1", "us-east4", "us-east5", "us-south1", "us-west1", "us-west2", "us-west3", "us-west4"},
	"n2d": {"africa-south1", "asia-east1", "asia-east2", "asia-northeast1", "asia-northeast2", "asia-northeast3", "asia-south1",
		"asia-south2", "asia-southeast1", "asia-southeast2", "australia-southeast1", "australia-southeast2", "europe-central2",
		"europe-north1", "europe-southwest1", "europe-west1", "europe-west2", "europe-west3", "europe-west4", "europe-west6",
		"europe-west8", "europe-west9", "europe-west10", "europe-west12", "me-central1", "me-central2", "me-west1",
		"northamerica-northeast1", "northamerica-northeast2", "southamerica-east1", "southamerica-west1",
		"us-central1", "us-east1", "us-east4", "us-east5", "us-south1", "us-west1", "us-west2", "us-west3", "us-west4"},
	"c3-standard": {"asia-northeast1", "asia-southeast1", "australia-southeast1", "europe-west1", "europe-west2", "europe-west3",
		"europe-west4", "europe-west9", "me-central2", "me-west1", "northamerica-northeast1", "southamerica-east1",
		"us-central1", "us-east1", "us-east4", "us-east5", "us-west1", "us-west2", "us-west3", "us-west4"},
	"c3-highcpu": {"asia-southeast1", "europe-west1", "europe-west3", "europe-west4", "me-central2",
		"us-central1", "us-east1", "us-east4", "us-east5", "us-west2", "us-west3"},
	"c3-highmem": {"asia-southeast1", "europe-west1", "europe-west2", "europe-west3", "europe-west4",
		"us-central1", "us-east1", "us-east4", "us-east5", "us-west1", "us-west2", "us-west4"},
}

// poolFamily is a machine type's column of the availability table: its
// series, or for c3 its series and kind.
func poolFamily(machineType string) string {
	parts := strings.SplitN(machineType, "-", 3)
	if parts[0] == "c3" && len(parts) > 1 {
		return "c3-" + parts[1]
	}
	return parts[0]
}

// PoolRegions are the regions a private pool can be created in: those of
// the availability table, which every one offers e2 in.
func PoolRegions() []string {
	return slices.Clone(poolFamilyRegions["e2"])
}

// PoolMachineTypes are the machine types of a series a region offers.
func PoolMachineTypes(region, series string) []string {
	var out []string
	for _, mt := range poolMachineTypes[series] {
		if slices.Contains(poolFamilyRegions[poolFamily(mt)], region) {
			out = append(out, mt)
		}
	}
	return out
}

// PoolRegionSeries are the series a region offers.
func PoolRegionSeries(region string) []string {
	var out []string
	for _, s := range PoolSeries {
		if len(PoolMachineTypes(region, s)) > 0 {
			out = append(out, s)
		}
	}
	return out
}

// checkPoolMachineType refuses a machine type the schema does not list,
// or the pool's region does not offer.
func checkPoolMachineType(region, machineType string) error {
	if machineType == "" {
		return nil
	}
	series, _, _ := strings.Cut(machineType, "-")
	if !slices.Contains(poolMachineTypes[series], machineType) {
		return apierror.InvalidArgument("invalid value for privatePoolV1Config.workerConfig.machineType: %q is not a private pool machine type; "+
			"specify one of the e2, n2d or c3 machine types the private pool config file schema lists", machineType)
	}
	if !slices.Contains(poolFamilyRegions[poolFamily(machineType)], region) {
		return apierror.InvalidArgument("invalid value for privatePoolV1Config.workerConfig.machineType: %s machine types are not available in region %s",
			poolFamily(machineType)+"-*", region)
	}
	return nil
}
