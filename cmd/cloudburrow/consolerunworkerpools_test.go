package main

import (
	"context"
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
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
