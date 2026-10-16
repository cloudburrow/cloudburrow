package logging

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// fakeHierarchy is organizations/1 > folders/10 > projects/child-proj.
type fakeHierarchy struct{}

func (fakeHierarchy) Exists(name string) error {
	if name == "organizations/1" || name == "folders/10" {
		return nil
	}
	return apierror.NotFound("%s not found", name)
}

func (fakeHierarchy) Ancestors(name string) ([]string, error) {
	switch name {
	case "projects/child-proj":
		return []string{"folders/10", "organizations/1"}, nil
	case "folders/10":
		return []string{"organizations/1"}, nil
	}
	return nil, errors.New("no ancestry")
}

func parentsServer() (*Server, loggingpb.ConfigServiceV2Server) {
	a := NewAdmin(store.NewMemory())
	a.UseHierarchy(fakeHierarchy{})
	s := NewServer(NewStore(0))
	s.UseAdmin(a)
	return s, a.ConfigServer()
}

func writeTo(t *testing.T, s *Server, logName string, msgs ...string) {
	t.Helper()
	var es []*loggingpb.LogEntry
	for _, m := range msgs {
		e := textEntry(m)
		e.Labels = map[string]string{"route": strings.Split(m, "-")[0], "x": m[strings.LastIndex(m, "-")+1:]}
		es = append(es, e)
	}
	if _, err := s.WriteLogEntries(context.Background(), &loggingpb.WriteLogEntriesRequest{
		LogName: logName, Resource: &mrpb.MonitoredResource{Type: "global"}, Entries: es}); err != nil {
		t.Fatalf("write %s: %v", logName, err)
	}
}

func read(t *testing.T, s *Server, names ...string) []string {
	t.Helper()
	resp, err := s.ListLogEntries(context.Background(), &loggingpb.ListLogEntriesRequest{ResourceNames: names})
	if err != nil {
		t.Fatalf("list %v: %v", names, err)
	}
	var out []string
	for _, e := range resp.GetEntries() {
		out = append(out, e.GetTextPayload())
	}
	return out
}

func TestFolderAndOrganizationParents(t *testing.T) {
	ctx := context.Background()
	s, cs := parentsServer()
	bucket := "projects/sink-proj/locations/global/buckets/agg"
	if _, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/sink-proj/locations/global", BucketId: "agg"}); err != nil {
		t.Fatal(err)
	}
	// An aggregated sink of the folder routes its children's entries.
	if _, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "folders/10", Sink: &loggingpb.LogSink{
		Name: "agg", Destination: "logging.googleapis.com/" + bucket, IncludeChildren: true, Filter: `labels.route = "keep"`}}); err != nil {
		t.Fatal(err)
	}
	// One that is not aggregated sees the folder's own entries only.
	if _, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "organizations/1", Sink: &loggingpb.LogSink{
		Name: "own", Destination: "logging.googleapis.com/projects/sink-proj/locations/global/buckets/_Default"}}); err != nil {
		t.Fatal(err)
	}
	// A project exclusion drops from the project's _Default bucket only.
	if _, err := cs.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: "projects/child-proj", Exclusion: &loggingpb.LogExclusion{
		Name: "drop", Filter: `labels.x = "excluded"`}}); err != nil {
		t.Fatal(err)
	}
	writeTo(t, s, "projects/child-proj/logs/app", "keep-1", "skip-1", "keep-excluded")
	writeTo(t, s, "folders/10/logs/audit", "keep-folder")
	writeTo(t, s, "organizations/1/logs/audit", "org-own")

	if got := read(t, s, "projects/child-proj"); len(got) != 2 {
		t.Errorf("project entries = %v, want keep-1 and skip-1 (the excluded one dropped)", got)
	}
	if got := read(t, s, "folders/10"); len(got) != 1 || got[0] != "keep-folder" {
		t.Errorf("folders/10 entries = %v, want its own entry only", got)
	}
	if got := read(t, s, bucket+"/views/_AllLogs"); len(got) != 3 {
		t.Errorf("aggregated bucket = %v, want keep-1, keep-excluded and keep-folder", got)
	}
	if got := read(t, s, "projects/sink-proj/locations/global/buckets/_Default/views/_AllLogs"); len(got) != 1 || got[0] != "org-own" {
		t.Errorf("sink-proj _Default = %v, want only the organization's own entry", got)
	}
	logs, err := s.ListLogs(ctx, &loggingpb.ListLogsRequest{Parent: "organizations/1"})
	if err != nil || len(logs.GetLogNames()) != 1 || logs.GetLogNames()[0] != "organizations/1/logs/audit" {
		t.Errorf("ListLogs(organizations/1) = %v, %v", logs, err)
	}
	if _, err := s.DeleteLog(ctx, &loggingpb.DeleteLogRequest{LogName: "folders/10/logs/audit"}); err != nil {
		t.Errorf("DeleteLog folder log: %v", err)
	}
	if got := read(t, s, "folders/10"); len(got) != 0 {
		t.Errorf("after DeleteLog, folders/10 = %v", got)
	}

	for _, tc := range []struct {
		what string
		err  error
		want codes.Code
	}{
		{"a folder that does not exist", func() error {
			_, err := cs.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: "folders/99"})
			return err
		}(), codes.NotFound},
		{"another organization", func() error {
			_, err := s.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"organizations/2"}})
			return err
		}(), codes.NotFound},
		{"include_children on a project sink", func() error {
			_, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/p-1", Sink: &loggingpb.LogSink{
				Name: "x", Destination: "storage.googleapis.com/b", IncludeChildren: true}})
			return err
		}(), codes.InvalidArgument},
		{"include_children on a billing account sink", func() error {
			_, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "billingAccounts/0A0B0C-0D0E0F-010203", Sink: &loggingpb.LogSink{
				Name: "x", Destination: "storage.googleapis.com/b", IncludeChildren: true}})
			return err
		}(), codes.InvalidArgument},
		{"a malformed billing account", func() error {
			_, err := cs.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: "billingAccounts/nope"})
			return err
		}(), codes.InvalidArgument},
		{"updating a folder's settings", func() error {
			_, err := cs.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: "folders/10/settings",
				Settings: &loggingpb.Settings{StorageLocation: "us-central1"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"storage_location"}}})
			return err
		}(), codes.FailedPrecondition},
	} {
		if status.Code(tc.err) != tc.want {
			t.Errorf("%s: %v, want %s", tc.what, tc.err, tc.want)
		}
	}

	// A billing account's sinks, exclusions and buckets are stored.
	ba := "billingAccounts/0A0B0C-0D0E0F-010203"
	if _, err := cs.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: ba, Exclusion: &loggingpb.LogExclusion{Name: "e", Filter: "severity<ERROR"}}); err != nil {
		t.Fatal(err)
	}
	if ex, err := cs.GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: ba + "/exclusions/e"}); err != nil || ex.GetFilter() != "severity<ERROR" {
		t.Errorf("billing account exclusion = %v, %v", ex, err)
	}
	if b, err := cs.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: "organizations/1/locations/global/buckets/_Default"}); err != nil || b.GetRetentionDays() != 30 {
		t.Errorf("organization _Default bucket = %v, %v", b, err)
	}
	st, err := cs.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: "organizations/1/settings",
		Settings: &loggingpb.Settings{StorageLocation: "europe-west1"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"storage_location"}}})
	if err != nil || st.GetName() != "organizations/1/settings" {
		t.Fatalf("UpdateSettings(org) = %v, %v", st, err)
	}
	if got, _ := cs.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: "organizations/1/settings"}); got.GetStorageLocation() != "europe-west1" {
		t.Errorf("org settings not stored: %v", got)
	}
	if c, err := cs.GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: "folders/10/cmekSettings"}); err != nil || c.GetName() != "folders/10/cmekSettings" {
		t.Errorf("folder cmekSettings = %v, %v", c, err)
	}
}
