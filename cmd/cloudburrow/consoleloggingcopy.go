package main

// Copy log entries on a log bucket's page (#1394): CopyLogEntries to a Cloud
// Storage bucket on the instance, and the bucket's copy operations, each
// with its state, progress and count, and Cancel on one still running.
// Google documents copying through gcloud alone, so the action's label is
// CloudBurrow's own and no console parity is claimed for it; it is offered
// so the feature is reachable from the console.

import (
	"context"
	"fmt"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	copyEntriesAction = "copy-entries"
	cancelCopyAction  = "cancel-copy"
	copyNote          = "Copies the bucket's entries a filter matches, stored when the copy starts, to a Cloud Storage bucket " +
		"on this instance, as LOG_ID/YYYY/MM/DD/HH:00:00_HH:59:59_S0.json objects of LogEntry JSON, one per line (#1394). " +
		"_Default holds the project's own entries; any bucket also holds what sinks route to it."
)

// copyActions offers Copy log entries on an active bucket when the instance
// serves Cloud Storage, and Cancel copy while one of its copies is
// unfinished.
func (p loggingBucketsProvider) copyActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	if a, err := p.admin(); err != nil || !a.CanCopy() {
		return nil
	}
	b, err := p.get(ctx, project, path[0])
	if err != nil || b.GetLifecycleState() != loggingpb.LifecycleState_ACTIVE {
		return nil
	}
	out := []console.Action{{ID: copyEntriesAction, Label: "Copy log entries", Fields: []console.Field{
		{Name: "destination", Label: "Cloud Storage bucket", Type: "text", Required: true,
			Help: "A bucket on this instance; the copy goes to storage.googleapis.com/BUCKET."},
		{Name: "filter", Label: "Filter", Type: "text", Help: "Empty copies every entry."},
	}}}
	var running []string
	for _, op := range p.copyOps(ctx, path[0]) {
		if !op.GetDone() {
			running = append(running, op.GetName()[strings.LastIndex(op.GetName(), "/")+1:])
		}
	}
	if len(running) > 0 {
		out = append(out, console.Action{ID: cancelCopyAction, Label: "Cancel copy", Destructive: true,
			Fields: []console.Field{{Name: "operation", Label: "Operation", Type: "select", Required: true, Options: running, Default: running[0]}}})
	}
	return out
}

// copyOps are the copy operations of bucket's location whose source is it.
func (p loggingBucketsProvider) copyOps(ctx context.Context, bucket string) []*longrunningpb.Operation {
	a, err := p.admin()
	if err != nil {
		return nil
	}
	loc := bucket[:strings.Index(bucket, "/buckets/")]
	resp, err := a.OperationsServer().ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: loc, PageSize: 100})
	if err != nil {
		return nil
	}
	var out []*longrunningpb.Operation
	for _, op := range resp.GetOperations() {
		md := &loggingpb.CopyLogEntriesMetadata{}
		if op.GetMetadata().UnmarshalTo(md) == nil && md.GetRequest().GetName() == bucket {
			out = append(out, op)
		}
	}
	return out
}

// copies lists the bucket's copy operations for its page.
func (p loggingBucketsProvider) copies(ctx context.Context, bucket string) console.Listing {
	l := console.Listing{Columns: []string{"Destination", "Filter", "State", "Progress", "Entries copied", "Started"},
		NameColumn: "Operation", Noun: "copy operations", Note: copyNote}
	for _, op := range p.copyOps(ctx, bucket) {
		md := &loggingpb.CopyLogEntriesMetadata{}
		_ = op.GetMetadata().UnmarshalTo(md)
		copied := "-"
		resp := &loggingpb.CopyLogEntriesResponse{}
		if r := op.GetResponse(); r != nil && r.UnmarshalTo(resp) == nil {
			copied = fmt.Sprint(resp.GetLogEntriesCopiedCount())
		}
		state := strings.TrimPrefix(md.GetState().String(), "OPERATION_STATE_")
		if e := op.GetError(); e != nil {
			state += ": " + e.GetMessage()
		}
		l.Items = append(l.Items, console.Resource{Name: op.GetName()[strings.LastIndex(op.GetName(), "/")+1:], Fields: map[string]string{
			"Destination": md.GetRequest().GetDestination(), "Filter": orDash(md.GetRequest().GetFilter()), "State": state,
			"Progress": fmt.Sprintf("%d%%", md.GetProgress()), "Entries copied": copied, "Started": loggingTime(md.GetStartTime())}})
	}
	l.Total = len(l.Items)
	return l
}

// actOnCopy starts a copy through CopyLogEntries, or cancels one through
// CancelOperation.
func (p loggingBucketsProvider) actOnCopy(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	if _, err := p.get(ctx, project, path[0]); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	switch action {
	case copyEntriesAction:
		if err := p.fault(ctx, loggingpb.ConfigServiceV2_CopyLogEntries_FullMethodName, path[0]); err != nil {
			return err
		}
		dest := strings.TrimPrefix(strings.TrimSpace(values["destination"]), "gs://")
		if !strings.HasPrefix(dest, "storage.googleapis.com/") {
			dest = "storage.googleapis.com/" + dest
		}
		_, err = a.ConfigServer().CopyLogEntries(ctx, &loggingpb.CopyLogEntriesRequest{Name: path[0],
			Filter: strings.TrimSpace(values["filter"]), Destination: dest})
	case cancelCopyAction:
		loc := path[0][:strings.Index(path[0], "/buckets/")]
		_, err = a.OperationsServer().CancelOperation(ctx, &longrunningpb.CancelOperationRequest{
			Name: loc + "/operations/" + strings.TrimSpace(values["operation"])})
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return err
}
