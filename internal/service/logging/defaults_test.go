package logging

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// bornHierarchy is organizations/1 > folders/10 > projects/in-folder, and
// organizations/1 > projects/top-proj.
type bornHierarchy struct{}

func (bornHierarchy) Exists(name string) error {
	if name == "organizations/1" || name == "folders/10" {
		return nil
	}
	return apierror.NotFound("%s not found", name)
}

func (bornHierarchy) Ancestors(name string) ([]string, error) {
	switch name {
	case "projects/in-folder":
		return []string{"folders/10", "organizations/1"}, nil
	case "folders/10", "projects/top-proj", "projects/old-proj":
		return []string{"organizations/1"}, nil
	}
	return nil, errors.New("no ancestry")
}

func writeText(t *testing.T, srv *Server, project, log, text string) {
	t.Helper()
	if _, err := srv.WriteLogEntries(context.Background(), &loggingpb.WriteLogEntriesRequest{Resource: &monitoredres.MonitoredResource{Type: "global"},
		Entries: []*loggingpb.LogEntry{{LogName: "projects/" + project + "/logs/" + log, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: text}}}}); err != nil {
		t.Fatal(err)
	}
}

// TestDefaultSink (#2108): the _Default sink is listed and read with
// Google's filter, can be changed and disabled but not deleted, and routes
// what it says from then on.
func TestDefaultSink(t *testing.T) {
	ctx := context.Background()
	srv := NewServer(NewStore(0))
	a := NewAdmin(store.NewMemory())
	srv.UseAdmin(a)
	cs := a.ConfigServer()
	const p, g = "projects/proj-one", "projects/proj-one/locations/global/buckets/"
	sk, err := cs.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: p + "/sinks/_Default"})
	if err != nil || sk.GetFilter() != DefaultSinkFilter || sk.GetDestination() != "logging.googleapis.com/"+g+"_Default" || sk.GetDisabled() {
		t.Fatalf("GetSink _Default = %v, %v", sk, err)
	}
	if !strings.HasPrefix(DefaultSinkFilter, `NOT LOG_ID("cloudaudit.googleapis.com/activity") AND NOT LOG_ID("externalaudit.googleapis.com/activity") AND `) {
		t.Errorf("filter %q", DefaultSinkFilter)
	}
	if _, err := cs.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: p + "/sinks/_Default"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteSink _Default = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: p, Sink: &loggingpb.LogSink{Name: "_Default", Destination: "logging.googleapis.com/" + g + "_Default"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateSink _Default = %v; want INVALID_ARGUMENT", err)
	}
	writeText(t, srv, "proj-one", "app", "before")
	// Disabled: what is written meanwhile is in no bucket; what was
	// routed before stays.
	if sk, err = cs.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: p + "/sinks/_Default", Sink: &loggingpb.LogSink{Disabled: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}}); err != nil || !sk.GetDisabled() || sk.GetFilter() != DefaultSinkFilter {
		t.Fatalf("UpdateSink disabled = %v, %v", sk, err)
	}
	writeText(t, srv, "proj-one", "app", "while-disabled")
	if got := readTexts(t, srv, "", p); strings.Join(got, ",") != "before" {
		t.Errorf("p1 reads %v; want only what came before the sink was disabled", got)
	}
	// Re-enabled with a filter and an exclusion of its own; the project's
	// exclusions still apply.
	if _, err := cs.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: p, Exclusion: &loggingpb.LogExclusion{Name: "noisy", Filter: `textPayload = "noisy"`}}); err != nil {
		t.Fatal(err)
	}
	if sk, err = cs.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: p + "/sinks/_Default", Sink: &loggingpb.LogSink{
		Filter: `logName = "projects/proj-one/logs/app"`, Exclusions: []*loggingpb.LogExclusion{{Name: "skip", Filter: `textPayload = "skip"`}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled", "filter", "exclusions"}}}); err != nil || sk.GetDisabled() || len(sk.GetExclusions()) != 1 {
		t.Fatalf("UpdateSink = %v, %v", sk, err)
	}
	for _, text := range []string{"kept", "skip", "noisy"} {
		writeText(t, srv, "proj-one", "app", text)
	}
	writeText(t, srv, "proj-one", "other", "other-log")
	writeText(t, srv, "proj-one", "cloudaudit.googleapis.com%2Factivity", "audit")
	if got := readTexts(t, srv, "", p); strings.Join(got, ",") != "before,kept,audit" {
		t.Errorf("p1 reads %v; want before, kept and the audit entry (in _Required)", got)
	}
	// To another bucket: the _Default bucket takes nothing new.
	if _, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/proj-one/locations/global", BucketId: "mine", Bucket: &loggingpb.LogBucket{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: p + "/sinks/_Default", Sink: &loggingpb.LogSink{Destination: "logging.googleapis.com/" + g + "mine", Filter: ""},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"destination", "filter", "exclusions"}}}); err != nil {
		t.Fatal(err)
	}
	writeText(t, srv, "proj-one", "app", "moved")
	if got := readTexts(t, srv, "", g+"mine/views/_AllLogs"); strings.Join(got, ",") != "moved" {
		t.Errorf("mine holds %v; want moved", got)
	}
	if got := readTexts(t, srv, "", g+"_Default/views/_AllLogs"); strings.Join(got, ",") != "before,kept" {
		t.Errorf("_Default holds %v", got)
	}
	ls, err := cs.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: p})
	var names []string
	for _, s := range ls.GetSinks() {
		names = append(names, s.GetName())
	}
	if err != nil || !slices.Equal(names, []string{"_Default", "_Required"}) || ls.GetSinks()[0].GetDestination() != "logging.googleapis.com/"+g+"mine" {
		t.Errorf("ListSinks = %v, %v", names, err)
	}
	if _, err := cs.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: p + "/sinks/_Default", Sink: &loggingpb.LogSink{IncludeChildren: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"include_children"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateSink include_children = %v; want INVALID_ARGUMENT", err)
	}
}

func settingsCall(t *testing.T, h *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, h.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

// TestDefaultResourceSettings (#2108): a project or folder made after its
// ancestors' default resource settings gets its _Default and _Required
// buckets in their storage location, its _Default sink disabled or
// configured as they say; one made before keeps the built-ins.
func TestDefaultResourceSettings(t *testing.T) {
	ctx := context.Background()
	srv := NewServer(NewStore(0))
	a := NewAdmin(store.NewMemory())
	a.UseHierarchy(bornHierarchy{})
	srv.UseAdmin(a)
	cs := a.ConfigServer()
	h := httptest.NewServer(NewRESTHandler(func(g grpc.ServiceRegistrar) { srv.Register(g); a.Register(g) }))
	defer h.Close()

	a.Born("projects/old-proj")
	if _, err := cs.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: "organizations/1/settings",
		Settings: &loggingpb.Settings{StorageLocation: "europe-west1", DisableDefaultSink: true}}); err != nil {
		t.Fatal(err)
	}
	// defaultSinkConfig over JSON: checked, kept and answered.
	for _, bad := range []string{
		`{"defaultSinkConfig":{"filter":"severity>=ERROR"}}`,
		`{"defaultSinkConfig":{"mode":"OVERWRITE"}}`,
		`{"defaultSinkConfig":{"mode":"APPEND","exclusions":[{"filter":"x=1"}]}}`,
		`{"defaultSinkConfig":{"mode":"APPEND","filter":"severity >>> 1"}}`,
	} {
		if code, out := settingsCall(t, h, http.MethodPatch, "/v2/organizations/1/settings?updateMask=defaultSinkConfig", bad); code != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d %v; want 400", bad, code, out)
		}
	}
	code, out := settingsCall(t, h, http.MethodPatch, "/v2/organizations/1/settings?updateMask=defaultSinkConfig",
		`{"defaultSinkConfig":{"mode":"APPEND","filter":"severity >= WARNING","exclusions":[{"name":"no-debug","filter":"textPayload = \"debug\""}]}}`)
	if dsc, _ := out["defaultSinkConfig"].(map[string]any); code != http.StatusOK || dsc["mode"] != "APPEND" || out["storageLocation"] != "europe-west1" {
		t.Fatalf("PATCH defaultSinkConfig = %d %v", code, out)
	}
	if _, out := settingsCall(t, h, http.MethodGet, "/v2/organizations/1/settings", ""); out["defaultSinkConfig"] == nil || out["disableDefaultSink"] != true {
		t.Errorf("GET settings = %v", out)
	}
	// The folder sets its own location and turns the default sink back on.
	if _, err := cs.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: "folders/10/settings",
		Settings: &loggingpb.Settings{StorageLocation: "us-central1"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"storage_location", "disable_default_sink"}}}); err != nil {
		t.Fatal(err)
	}
	a.Born("projects/top-proj")
	a.Born("projects/in-folder")

	// Made before: global built-ins and the built-in sink.
	if b, err := cs.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: "projects/old-proj/locations/global/buckets/_Default"}); err != nil || b.GetName() == "" {
		t.Errorf("old's _Default = %v, %v", b, err)
	}
	if sk, _ := cs.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: "projects/old-proj/sinks/_Default"}); sk.GetDisabled() || sk.GetFilter() != DefaultSinkFilter {
		t.Errorf("old's _Default sink = %v", sk)
	}
	// top: the organization's settings.
	lb, err := cs.ListBuckets(ctx, &loggingpb.ListBucketsRequest{Parent: "projects/top-proj/locations/-"})
	var names []string
	for _, b := range lb.GetBuckets() {
		names = append(names, b.GetName())
	}
	if err != nil || !slices.Equal(names, []string{"projects/top-proj/locations/europe-west1/buckets/_Default", "projects/top-proj/locations/europe-west1/buckets/_Required"}) {
		t.Errorf("top's buckets = %v, %v", names, err)
	}
	if _, err := cs.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: "projects/top-proj/locations/global/buckets/_Default"}); status.Code(err) != codes.NotFound {
		t.Errorf("top's global _Default = %v; want NOT_FOUND", err)
	}
	if _, err := cs.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: "projects/top-proj/locations/europe-west1/buckets/_Default"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteBucket of top's _Default = %v", err)
	}
	if _, err := cs.UpdateBucket(ctx, &loggingpb.UpdateBucketRequest{Name: "projects/top-proj/locations/europe-west1/buckets/_Required", Bucket: &loggingpb.LogBucket{RetentionDays: 10},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"retention_days"}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("UpdateBucket of top's _Required = %v", err)
	}
	sk, err := cs.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: "projects/top-proj/sinks/_Default"})
	wantFilter := "(" + DefaultSinkFilter + ") AND (severity >= WARNING)"
	if err != nil || !sk.GetDisabled() || sk.GetFilter() != wantFilter || len(sk.GetExclusions()) != 1 ||
		sk.GetDestination() != "logging.googleapis.com/projects/top-proj/locations/europe-west1/buckets/_Default" {
		t.Errorf("top's _Default sink = %v, %v", sk, err)
	}
	if rs, _ := cs.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: "projects/top-proj/sinks/_Required"}); rs.GetDestination() != "logging.googleapis.com/projects/top-proj/locations/europe-west1/buckets/_Required" {
		t.Errorf("top's _Required sink = %v", rs)
	}
	writeText(t, srv, "top-proj", "app", "dropped")
	writeText(t, srv, "top-proj", "cloudaudit.googleapis.com%2Factivity", "audit")
	if got := readTexts(t, srv, "", "projects/top-proj"); strings.Join(got, ",") != "audit" {
		t.Errorf("top reads %v; want the audit entry alone (its _Default sink is disabled)", got)
	}
	if got := readTexts(t, srv, "", "projects/top-proj/locations/europe-west1/buckets/_Required/views/_AllLogs"); strings.Join(got, ",") != "audit" {
		t.Errorf("top's _Required holds %v", got)
	}

	// in-folder: the folder's location, its default sink on, and the
	// organization's defaultSinkConfig.
	sk, err = cs.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: "projects/in-folder/sinks/_Default"})
	if err != nil || sk.GetDisabled() || sk.GetFilter() != wantFilter ||
		sk.GetDestination() != "logging.googleapis.com/projects/in-folder/locations/us-central1/buckets/_Default" {
		t.Fatalf("in-folder's _Default sink = %v, %v", sk, err)
	}
	if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: &monitoredres.MonitoredResource{Type: "global"},
		Entries: []*loggingpb.LogEntry{
			{LogName: "projects/in-folder/logs/app", Severity: 200, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "info"}},
			{LogName: "projects/in-folder/logs/app", Severity: 500, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "error"}},
			{LogName: "projects/in-folder/logs/app", Severity: 500, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "debug"}},
		}}); err != nil {
		t.Fatal(err)
	}
	if got := readTexts(t, srv, "", "projects/in-folder/locations/us-central1/buckets/_Default/views/_AllLogs"); strings.Join(got, ",") != "error" {
		t.Errorf("in-folder's _Default holds %v; want error alone", got)
	}
	if got := readTexts(t, srv, "", "projects/in-folder"); strings.Join(got, ",") != "error" {
		t.Errorf("in-folder reads %v", got)
	}
	// A folder made now takes the organization's settings too.
	a.Born("folders/10")
	if b, err := cs.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: "folders/10/locations/europe-west1/buckets/_Required"}); err != nil || b.GetRetentionDays() != requiredRetention || !b.GetLocked() {
		t.Errorf("folder's _Required = %v, %v", b, err)
	}
	// Clearing it over JSON.
	if code, out := settingsCall(t, h, http.MethodPatch, "/v2/organizations/1/settings?updateMask=default_sink_config", `{}`); code != http.StatusOK || out["defaultSinkConfig"] != nil {
		t.Errorf("clearing defaultSinkConfig = %d %v", code, out)
	}
}
