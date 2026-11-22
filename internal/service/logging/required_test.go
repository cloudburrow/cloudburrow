package logging

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func readTexts(t *testing.T, srv *Server, filter string, names ...string) []string {
	t.Helper()
	resp, err := srv.ListLogEntries(context.Background(), &loggingpb.ListLogEntriesRequest{ResourceNames: names, Filter: filter})
	if err != nil {
		t.Fatalf("ListLogEntries %v %q: %v", names, filter, err)
	}
	var out []string
	for _, e := range resp.GetEntries() {
		if p := e.GetTextPayload(); p != "" {
			out = append(out, p)
		} else {
			out = append(out, e.GetJsonPayload().GetFields()["name"].GetStringValue())
		}
	}
	return out
}

// TestRequiredBucketAndSink (#2108): the _Required sink is listed and
// can't be changed; audit entries go to _Required, not _Default, whatever an
// exclusion says; a parent read takes both buckets; and _Required's
// retention can't be changed.
func TestRequiredBucketAndSink(t *testing.T) {
	ctx := context.Background()
	srv := NewServer(NewStore(0))
	a := NewAdmin(store.NewMemory())
	srv.UseAdmin(a)
	cs := a.ConfigServer()
	if _, err := cs.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: "projects/proj-1",
		Exclusion: &loggingpb.LogExclusion{Name: "all", Filter: `severity >= DEFAULT`}}); err != nil {
		t.Fatal(err)
	}
	for _, log := range []string{"app", "cloudaudit.googleapis.com%2Factivity", "cloudaudit.googleapis.com%2Fsystem_event", "cloudaudit.googleapis.com%2Fdata_access"} {
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: &monitoredres.MonitoredResource{Type: "global"},
			Entries: []*loggingpb.LogEntry{{LogName: "projects/proj-1/logs/" + log, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: log}}}}); err != nil {
			t.Fatal(err)
		}
	}
	const g = "projects/proj-1/locations/global/buckets/"
	if got := readTexts(t, srv, "", g+"_Required/views/_AllLogs"); strings.Join(got, ",") != "cloudaudit.googleapis.com%2Factivity,cloudaudit.googleapis.com%2Fsystem_event" {
		t.Errorf("_Required holds %v; want the two audit entries the exclusion does not reach", got)
	}
	if got := readTexts(t, srv, "", g+"_Default/views/_AllLogs"); len(got) != 0 {
		t.Errorf("_Default holds %v; the exclusion drops the rest", got)
	}
	if got := readTexts(t, srv, "", "projects/proj-1"); len(got) != 2 {
		t.Errorf("projects/proj-1 reads %v; want both _Required entries", got)
	}
	sk, err := cs.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: "projects/proj-1/sinks/_Required"})
	if err != nil || sk.GetDestination() != "logging.googleapis.com/"+g+"_Required" || !strings.HasPrefix(sk.GetFilter(), `LOG_ID("cloudaudit.googleapis.com/activity") OR `) {
		t.Fatalf("GetSink _Required = %v, %v", sk, err)
	}
	ls, err := cs.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: "projects/proj-1"})
	if err != nil || len(ls.GetSinks()) != 2 || ls.GetSinks()[0].GetName() != "_Default" || ls.GetSinks()[1].GetName() != "_Required" {
		t.Errorf("ListSinks = %v, %v", ls, err)
	}
	if _, err := cs.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: "projects/proj-1/sinks/_Required", Sink: &loggingpb.LogSink{Disabled: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("disabling _Required = %v", err)
	}
	if _, err := cs.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: "projects/proj-1/sinks/_Required"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deleting _Required = %v", err)
	}
	_, err = cs.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: g + "_Required", Bucket: &loggingpb.LogBucket{RetentionDays: 30},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention_days"}}})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "400 days and can't be changed") {
		t.Errorf("_Required retention change = %v", err)
	}
}

// TestCustomIndexes (#2108): index_configs are validated, stamped and read
// back, and a query of the bucket compares an indexed field as the index's
// type once the index has applied, as the custom-index page's example does.
func TestCustomIndexes(t *testing.T) {
	ctx := context.Background()
	srv := NewServer(NewStore(0))
	a := NewAdmin(store.NewMemory())
	srv.UseAdmin(a)
	cs := a.ConfigServer()
	const parent = "projects/proj-1/locations/global"
	for _, ics := range [][]*loggingpb.IndexConfig{
		{{FieldPath: "jsonPayload.value"}},
		{{FieldPath: "nope.value", Type: loggingpb.IndexType_INDEX_TYPE_STRING}},
		{{Type: loggingpb.IndexType_INDEX_TYPE_STRING}},
		{{FieldPath: "jsonPayload.a", Type: loggingpb.IndexType_INDEX_TYPE_STRING}, {FieldPath: "jsonPayload.a", Type: loggingpb.IndexType_INDEX_TYPE_INTEGER}},
		func() (out []*loggingpb.IndexConfig) {
			for i := 0; i < 21; i++ {
				out = append(out, &loggingpb.IndexConfig{FieldPath: "jsonPayload.f" + string(rune('a'+i)), Type: loggingpb.IndexType_INDEX_TYPE_STRING})
			}
			return
		}(),
	} {
		if _, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: parent, BucketId: "bad", Bucket: &loggingpb.LogBucket{IndexConfigs: ics}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateBucket %v = %v, want INVALID_ARGUMENT", ics, err)
		}
	}
	for _, id := range []string{"ints", "strs"} {
		typ := loggingpb.IndexType_INDEX_TYPE_INTEGER
		if id == "strs" {
			typ = loggingpb.IndexType_INDEX_TYPE_STRING
		}
		b, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: parent, BucketId: id,
			Bucket: &loggingpb.LogBucket{IndexConfigs: []*loggingpb.IndexConfig{{FieldPath: "jsonPayload.value", Type: typ}}}})
		if err != nil || b.GetIndexConfigs()[0].GetCreateTime() == nil {
			t.Fatalf("CreateBucket %s = %v, %v", id, b, err)
		}
		if _, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: "projects/proj-1",
			Sink: &loggingpb.LogSink{Name: "to-" + id, Destination: "logging.googleapis.com/" + parent + "/buckets/" + id}}); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name string, v any) {
		p, _ := structpb.NewStruct(map[string]any{"name": name, "value": v})
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: &monitoredres.MonitoredResource{Type: "global"},
			Entries: []*loggingpb.LogEntry{{LogName: "projects/proj-1/logs/app", Timestamp: timestamppb.Now(), Payload: &loggingpb.LogEntry_JsonPayload{JsonPayload: p}}}}); err != nil {
			t.Fatal(err)
		}
	}
	write("A", 12345)
	write("B", "3")
	ints, strs := parent+"/buckets/ints/views/_AllLogs", parent+"/buckets/strs/views/_AllLogs"
	// Before the hour has passed, the field's own type: both match.
	if got := readTexts(t, srv, "jsonPayload.value > 20", ints); len(got) != 2 {
		t.Errorf("before the index applies: %v", got)
	}
	srv.Advance(ctx, time.Hour+time.Minute)
	if got := strings.Join(readTexts(t, srv, "jsonPayload.value > 20", ints), ","); got != "A" {
		t.Errorf("integer index: %q matched; want A", got)
	}
	if got := strings.Join(readTexts(t, srv, "jsonPayload.value > 20", strs), ","); got != "B" {
		t.Errorf("string index: %q matched; want B", got)
	}
	if got := strings.Join(readTexts(t, srv, "jsonPayload.value > 50", strs), ","); got != "" {
		t.Errorf("string index > 50: %q matched; want none", got)
	}
	// A query value that is no integer ignores the integer index.
	if got := strings.Join(readTexts(t, srv, `jsonPayload.value = "3"`, ints), ","); got != "B" {
		t.Errorf(`integer index = "3": %q`, got)
	}
	// An update that keeps the index keeps its create_time.
	b, _ := cs.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: parent + "/buckets/ints"})
	made := b.GetIndexConfigs()[0].GetCreateTime().AsTime()
	b2, err := cs.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: parent + "/buckets/ints", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"index_configs"}},
		Bucket: &loggingpb.LogBucket{IndexConfigs: []*loggingpb.IndexConfig{{FieldPath: "jsonPayload.value", Type: loggingpb.IndexType_INDEX_TYPE_INTEGER},
			{FieldPath: "jsonPayload.req_status", Type: loggingpb.IndexType_INDEX_TYPE_STRING}}}})
	if err != nil || len(b2.GetIndexConfigs()) != 2 || !b2.GetIndexConfigs()[0].GetCreateTime().AsTime().Equal(made) {
		t.Errorf("UpdateBucket index_configs = %v, %v", b2, err)
	}
}
