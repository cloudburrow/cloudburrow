package main

import (
	"context"
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeRunPools serves worker pools and instances read-only, for the screens'
// structural tests: the sparsest each API returns.
type fakeRunPools struct {
	runpb.UnimplementedWorkerPoolsServer
	pools map[string]*runpb.WorkerPool
}

func (f *fakeRunPools) GetWorkerPool(_ context.Context, req *runpb.GetWorkerPoolRequest) (*runpb.WorkerPool, error) {
	if wp, ok := f.pools[req.GetName()]; ok {
		return wp, nil
	}
	return nil, status.Errorf(codes.NotFound, "worker pool %s not found", req.GetName())
}

func (f *fakeRunPools) ListWorkerPools(_ context.Context, req *runpb.ListWorkerPoolsRequest) (*runpb.ListWorkerPoolsResponse, error) {
	out := &runpb.ListWorkerPoolsResponse{}
	for name, wp := range f.pools {
		if strings.HasPrefix(name, req.GetParent()+"/") {
			out.WorkerPools = append(out.WorkerPools, wp)
		}
	}
	return out, nil
}

type fakeRunInstances struct {
	runpb.UnimplementedInstancesServer
	instances map[string]*runpb.Instance
}

func (f *fakeRunInstances) GetInstance(_ context.Context, req *runpb.GetInstanceRequest) (*runpb.Instance, error) {
	if in, ok := f.instances[req.GetName()]; ok {
		return in, nil
	}
	return nil, status.Errorf(codes.NotFound, "instance %s not found", req.GetName())
}

func (f *fakeRunInstances) ListInstances(_ context.Context, req *runpb.ListInstancesRequest) (*runpb.ListInstancesResponse, error) {
	out := &runpb.ListInstancesResponse{}
	for name, in := range f.instances {
		if strings.HasPrefix(name, req.GetParent()+"/") {
			out.Instances = append(out.Instances, in)
		}
	}
	return out, nil
}

// TestWorkerPoolEditKeepsWhatTheFormDoesNotShow: the edit form is prefilled
// from the pool and the form's container replaces the pool's, the instance
// count read from it.
func TestWorkerPoolEditFormRoundTrips(t *testing.T) {
	two := int32(2)
	wp := &runpb.WorkerPool{Name: "projects/p/locations/l/workerPools/pool",
		Template: &runpb.WorkerPoolRevisionTemplate{Containers: []*runpb.Container{{Image: "img:1",
			Command: []string{"sh", "-c"}, Args: []string{"sleep 1"},
			Env: []*runpb.EnvVar{{Name: "A", Values: &runpb.EnvVar_Value{Value: "b"}}}}}},
		Scaling: &runpb.WorkerPoolScaling{ManualInstanceCount: &two}}
	form := workerPoolEditForm(wp)
	values := map[string]string{}
	for _, f := range form.Fields {
		values[f.Name] = f.Default
	}
	c, count, err := workerPoolForm(values)
	if err != nil {
		t.Fatalf("workerPoolForm: %v", err)
	}
	if c.GetImage() != "img:1" || strings.Join(c.GetArgs(), "|") != "sleep 1" || c.GetEnv()[0].GetValue() != "b" || count == nil || *count != 2 {
		t.Errorf("the prefilled form reads back as %v, %v", c, count)
	}
}

// TestConsoleRunWorkerPoolSplit (#1387): Split instances prefills the
// pool's splits, LATEST for the latest revision, and parses its lines back
// into instanceSplits; the revisions list gives each revision its share of
// the instances.
func TestConsoleRunWorkerPoolSplit(t *testing.T) {
	four := int32(4)
	name := "projects/p/locations/us-central1/workerPools/wp"
	wp := &runpb.WorkerPool{Name: name, LatestCreatedRevision: name + "/revisions/wp-00002-def",
		Scaling: &runpb.WorkerPoolScaling{ManualInstanceCount: &four},
		InstanceSplits: []*runpb.InstanceSplit{
			{Type: runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST, Percent: 75},
			{Type: runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_REVISION, Revision: "wp-00001-abc", Percent: 25}}}
	a := splitAction(wp)
	if a.ID != actSplit || len(a.Fields) != 1 || a.Fields[0].Default != `{"LATEST":"75","wp-00001-abc":"25"}` {
		t.Errorf("split action = %+v", a)
	}
	splits, err := instanceSplits(`{"latest":"60","wp-00001-abc":"40%"}`)
	if err != nil || len(splits) != 2 {
		t.Fatalf("instanceSplits = %v, %v", splits, err)
	}
	byRev := map[string]*runpb.InstanceSplit{}
	for _, sp := range splits {
		byRev[sp.GetRevision()] = sp
	}
	if l := byRev[""]; l.GetType() != runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST || l.GetPercent() != 60 {
		t.Errorf("latest split = %v", l)
	}
	if r := byRev["wp-00001-abc"]; r.GetType() != runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_REVISION || r.GetPercent() != 40 {
		t.Errorf("revision split = %v", r)
	}
	if _, err := instanceSplits(`{"LATEST":"half"}`); err == nil {
		t.Error("a split that is not a percentage was accepted")
	}
	counts := runadapter.RevisionInstances(wp)
	if counts[name+"/revisions/wp-00002-def"] != 3 || counts[name+"/revisions/wp-00001-abc"] != 1 {
		t.Errorf("revision instances = %v", counts)
	}
}
