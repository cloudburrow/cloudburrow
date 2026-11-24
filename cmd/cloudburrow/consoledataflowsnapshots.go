package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// dataflowSnapshotsProvider is Dataflow's Snapshots page (#2215): a
// project's snapshots of streaming jobs, each with Delete, as Google's
// page lists them (guides/using-snapshots: "Use the Snapshots page").
type dataflowSnapshotsProvider struct{ svc *dataflowService }

func (dataflowSnapshotsProvider) ID() string    { return "dataflow-snapshots" }
func (dataflowSnapshotsProvider) Title() string { return "Snapshots" }

// dataflowSnapshotState is Google's word for a snapshot state: "Ready".
func dataflowSnapshotState(st dataflowpb.SnapshotState) string {
	s := strings.TrimPrefix(st.String(), "SNAPSHOT_STATE_")
	if s == "" || s == "UNKNOWN" {
		return "Unknown"
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func (p dataflowSnapshotsProvider) List(_ context.Context, project string) (console.Listing, error) {
	s, err := dataflowProvider{svc: p.svc}.api()
	if err != nil {
		return console.Listing{}, err
	}
	l := console.Listing{Columns: []string{"Description", "Source job", "Status", "Region", "Creation time", "Expiration time", "Size"},
		NameColumn: "Snapshot ID", Noun: "snapshots"}
	if project == "" {
		l.Prompt = "Dataflow lists snapshots per project. Choose one in the toolbar."
		return l, nil
	}
	jobs := map[string]string{}
	for _, j := range s.Jobs(project) {
		jobs[j.GetId()] = j.GetName()
	}
	for _, sn := range s.Snapshots(project) {
		st := dataflowSnapshotState(sn.GetState())
		src := sn.GetSourceJobId()
		if n := jobs[src]; n != "" {
			src = n + " (" + src + ")"
		}
		expires := sn.GetCreationTime().AsTime().Add(sn.GetTtl().AsDuration())
		l.Items = append(l.Items, console.Resource{Name: sn.GetRegion() + "/" + sn.GetId(), Status: st,
			Opens: []string{"/dataflow/jobs/" + sn.GetRegion() + "/" + sn.GetSourceJobId()},
			Fields: map[string]string{"Description": kmsOrDash(sn.GetDescription()), "Source job": src, "Status": st, "Region": sn.GetRegion(),
				"Creation time": kmsTime(sn.GetCreationTime()), "Expiration time": kmsTime(timestamppb.New(expires)),
				"Size": strconv.FormatInt(sn.GetDiskSizeBytes(), 10) + " bytes"}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// Actions are a snapshot row's: Delete.
func (dataflowSnapshotsProvider) Actions(console.Resource) []console.Action {
	return []console.Action{{ID: "delete", Label: "Delete", Destructive: true, Confirm: "The snapshot is deleted. Jobs can no longer be created from it."}}
}

// Act deletes a snapshot, named REGION/ID.
func (p dataflowSnapshotsProvider) Act(ctx context.Context, project, name, action string) error {
	s, err := dataflowProvider{svc: p.svc}.api()
	if err != nil {
		return err
	}
	region, id, ok := strings.Cut(name, "/")
	if action != "delete" || !ok {
		return fmt.Errorf("unknown action %q on %q", action, name)
	}
	_, err = s.DeleteSnapshot(ctx, &dataflowpb.DeleteSnapshotRequest{ProjectId: project, Location: region, SnapshotId: id})
	return err
}
