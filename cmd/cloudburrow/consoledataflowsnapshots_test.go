package main

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataflow"
)

// snapDataflowEngine is fakeDataflowEngine that takes snapshots.
type snapDataflowEngine struct{ fakeDataflowEngine }

func (snapDataflowEngine) Snapshot(context.Context, string, string) (int64, error) { return 2048, nil }
func (snapDataflowEngine) DeleteSnapshot(string) error                             { return nil }

// TestConsoleDataflowSnapshots (#2215): a running streaming job's page
// offers Create snapshot, which snapshots it; the Snapshots page lists the
// snapshot (its description, source job, status, region and size) and
// Delete deletes it; a batch job's page offers no Create snapshot.
func TestConsoleDataflowSnapshots(t *testing.T) {
	ctx := context.Background()
	const project = "df-snaps"
	api, err := dataflow.NewServer(dataflow.Options{Engine: snapDataflowEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	svc := &dataflowService{api: api}
	p, sp := dataflowProvider{svc: svc}, dataflowSnapshotsProvider{svc: svc}
	mk := func(name string, typ dataflowpb.JobType) *dataflowpb.Job {
		j, err := api.CreateJob(ctx, &dataflowpb.CreateJobRequest{ProjectId: project, Job: &dataflowpb.Job{Name: name, Type: typ,
			Environment: &dataflowpb.Environment{SdkPipelineOptions: &structpb.Struct{Fields: map[string]*structpb.Value{"options": structpb.NewStructValue(&structpb.Struct{
				Fields: map[string]*structpb.Value{"pipelineUrl": structpb.NewStringValue("gs://b/p.pb")}})}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	stream := mk("stream", dataflowpb.JobType_JOB_TYPE_STREAMING)
	path := []string{stream.GetLocation() + "/" + stream.GetId()}
	var acts []string
	for deadline := time.Now().Add(10 * time.Second); ; {
		acts = acts[:0]
		for _, a := range p.DetailActions(ctx, project, path) {
			acts = append(acts, a.ID)
		}
		if len(acts) == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(acts) != 2 || acts[1] != "snapshot" {
		t.Fatalf("the running streaming job's actions are %v; want stop and snapshot", acts)
	}
	if err := p.ActAt(ctx, project, path, "snapshot", map[string]string{"description": "nightly", "sources": "without"}); err != nil {
		t.Fatal(err)
	}
	var l console.Listing
	for deadline := time.Now().Add(10 * time.Second); ; {
		if l, err = sp.List(ctx, project); err != nil {
			t.Fatal(err)
		}
		if len(l.Items) == 1 && l.Items[0].Status == "Ready" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(l.Items) != 1 || l.Items[0].Status != "Ready" || l.Items[0].Fields["Description"] != "nightly" ||
		l.Items[0].Fields["Source job"] != "stream ("+stream.GetId()+")" || l.Items[0].Fields["Size"] != "2048 bytes" || l.Items[0].Fields["Region"] != "us-central1" {
		t.Fatalf("Snapshots page = %+v", l.Items)
	}
	if err := sp.Act(ctx, project, l.Items[0].Name, "delete"); err != nil {
		t.Fatal(err)
	}
	if l, _ = sp.List(ctx, project); len(l.Items) != 0 {
		t.Errorf("after Delete the page lists %+v", l.Items)
	}
	batch := mk("batch", dataflowpb.JobType_JOB_TYPE_BATCH)
	for _, a := range p.DetailActions(ctx, project, []string{batch.GetLocation() + "/" + batch.GetId()}) {
		if a.ID == "snapshot" {
			t.Error("a batch job's page offers Create snapshot")
		}
	}
}
