package main

import (
	"slices"
	"strings"
	"testing"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// optionsFor is what a select offers with the form's region set.
func optionsFor(f console.Field, region string) []string {
	for _, o := range f.OptionsWhen {
		if slices.Contains(strings.Split(strings.TrimPrefix(o.When, "region="), "|"), region) {
			return o.Options
		}
	}
	return f.Options
}

// TestCreatePrivatePoolOffersTheRegionsMachineTypes (#2073): Create
// private pool's Region lists the 42 regions of Google's availability
// table; Series and Machine type offer what the chosen region has; Edit
// private pool offers its fixed region's.
func TestCreatePrivatePoolOffersTheRegionsMachineTypes(t *testing.T) {
	_, fields := buildWorkerPoolsProvider{}.CreateForm()
	by := map[string]console.Field{}
	for _, f := range fields {
		by[f.Name] = f
	}
	if n := len(by["region"].Options); n != 42 {
		t.Errorf("Region offers %d regions", n)
	}
	for region, want := range map[string]string{"us-central1": "e2,n2d,c3", "asia-east1": "e2,n2d", "europe-north2": "e2"} {
		if got := strings.Join(optionsFor(by["series"], region), ","); got != want {
			t.Errorf("Series in %s = %s, want %s", region, got, want)
		}
	}
	if got := optionsFor(by["machineType-c3"], "europe-west2"); slices.Contains(got, "c3-highcpu-4") || !slices.Contains(got, "c3-highmem-4") {
		t.Errorf("Machine type for C3 in europe-west2 = %v", got)
	}
	if by["machineType-e2"].Default != "e2-medium" || by["machineType-n1"].Name != "" {
		t.Errorf("e2 defaults to %q; n1 field %+v", by["machineType-e2"].Default, by["machineType-n1"])
	}
	w := &cbpb.WorkerPool{Name: "projects/p/locations/europe-north2/workerPools/w", Config: &cbpb.WorkerPool_PrivatePoolV1Config{
		PrivatePoolV1Config: &cbpb.PrivatePoolV1Config{WorkerConfig: &cbpb.PrivatePoolV1Config_WorkerConfig{MachineType: "e2-standard-4"}}}}
	edit := map[string]console.Field{}
	for _, f := range editPoolForm(w).Fields {
		edit[f.Name] = f
	}
	if s := edit["series"]; strings.Join(s.Options, ",") != "e2" || len(s.OptionsWhen) != 0 {
		t.Errorf("Edit private pool's Series in europe-north2 = %v", s.Options)
	}
	if _, ok := edit["machineType-c3"]; ok || edit["machineType-e2"].Default != "e2-standard-4" {
		t.Errorf("Edit private pool in europe-north2 = %+v", edit)
	}
}
