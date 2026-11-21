package run

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestPoolMachineTypesByRegion (#2073): the schema's 15 e2, 31 n2d and 18
// c3 machine types; its table's 42 regions, 40 with n2d, and c3's kinds
// where the table has them.
func TestPoolMachineTypesByRegion(t *testing.T) {
	if n := []int{len(poolMachineTypes["e2"]), len(poolMachineTypes["n2d"]), len(poolMachineTypes["c3"])}; !slices.Equal(n, []int{15, 31, 18}) {
		t.Errorf("machine types per series = %v", n)
	}
	if n := []int{len(PoolRegions()), len(poolFamilyRegions["n2d"]), len(poolFamilyRegions["c3-standard"]),
		len(poolFamilyRegions["c3-highcpu"]), len(poolFamilyRegions["c3-highmem"])}; !slices.Equal(n, []int{42, 40, 20, 11, 12}) {
		t.Errorf("regions per family = %v", n)
	}
	for region, want := range map[string][]string{"us-central1": {"e2", "n2d", "c3"}, "asia-east1": {"e2", "n2d"},
		"europe-north2": {"e2"}, "northamerica-south1": {"e2"}} {
		if got := PoolRegionSeries(region); !slices.Equal(got, want) {
			t.Errorf("PoolRegionSeries(%s) = %v, want %v", region, got, want)
		}
	}
	if got := PoolMachineTypes("europe-west2", "c3"); len(got) != 12 || slices.Contains(got, "c3-highcpu-4") || !slices.Contains(got, "c3-highmem-4") {
		t.Errorf("europe-west2's c3 = %v; it has c3-standard and c3-highmem", got)
	}
}

// TestWorkerPoolMachineTypeIsChecked (#2073): CreateWorkerPool and
// UpdateWorkerPool refuse a machine type the schema does not list, or the
// pool's region does not offer.
func TestWorkerPoolMachineTypeIsChecked(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	cb := s.Builds(fakeFetcher{}, &fakeBuilder{got: make(chan BuildSpec, 1)}).CloudBuild()
	ctx := context.Background()
	pool := func(mt string) *cbpb.WorkerPool {
		return &cbpb.WorkerPool{Config: &cbpb.WorkerPool_PrivatePoolV1Config{PrivatePoolV1Config: &cbpb.PrivatePoolV1Config{
			WorkerConfig: &cbpb.PrivatePoolV1Config_WorkerConfig{MachineType: mt}}}}
	}
	for i, tc := range []struct {
		region, mt string
		code       codes.Code
	}{
		{"us-central1", "c3-highcpu-8", codes.OK},
		{"us-central1", "", codes.OK},
		{"europe-west2", "c3-highcpu-8", codes.InvalidArgument},
		{"europe-north2", "n2d-standard-2", codes.InvalidArgument},
		{"us-central1", "n1-standard-1", codes.InvalidArgument},
		{"us-central1", "e2-standard-3", codes.InvalidArgument},
	} {
		_, err := cb.CreateWorkerPool(ctx, &cbpb.CreateWorkerPoolRequest{Parent: "projects/proj-pool/locations/" + tc.region,
			WorkerPoolId: "pool-" + string(rune('a'+i)), WorkerPool: pool(tc.mt)})
		if status.Code(err) != tc.code {
			t.Errorf("CreateWorkerPool %s in %s = %v; want %v", tc.mt, tc.region, err, tc.code)
		}
	}
	_, err := cb.UpdateWorkerPool(ctx, &cbpb.UpdateWorkerPoolRequest{WorkerPool: func() *cbpb.WorkerPool {
		p := pool("c3-highmem-4")
		p.Name = "projects/proj-pool/locations/us-central1/workerPools/pool-a"
		return p
	}(), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"private_pool_v1_config.worker_config.machine_type"}}})
	if err != nil {
		t.Errorf("UpdateWorkerPool to c3-highmem-4 in us-central1 = %v", err)
	}
	_, err = cb.CreateWorkerPool(ctx, &cbpb.CreateWorkerPoolRequest{Parent: "projects/proj-pool/locations/europe-west2", WorkerPoolId: "west2", WorkerPool: pool("e2-medium")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cb.UpdateWorkerPool(ctx, &cbpb.UpdateWorkerPoolRequest{WorkerPool: func() *cbpb.WorkerPool {
		p := pool("c3-highcpu-4")
		p.Name = "projects/proj-pool/locations/europe-west2/workerPools/west2"
		return p
	}(), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"private_pool_v1_config.worker_config.machine_type"}}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "c3-highcpu-* machine types are not available in region europe-west2") {
		t.Errorf("UpdateWorkerPool to c3-highcpu-4 in europe-west2 = %v", err)
	}
}
