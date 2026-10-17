package logging

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// An intercepting folder sink takes what it matches from its children's
// _Default buckets and sinks, while the sinks above the folder still see it.
func TestInterceptingSinkRouting(t *testing.T) {
	ctx := context.Background()
	s, cs := parentsServer()
	for _, b := range []string{"proj-agg", "org-agg"} {
		if _, err := cs.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/sink-proj/locations/global", BucketId: b}); err != nil {
			t.Fatal(err)
		}
	}
	sink := func(parent string, sk *loggingpb.LogSink, intercept bool) {
		t.Helper()
		markIntercept(sk, intercept)
		got, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: sk})
		if err != nil {
			t.Fatalf("CreateSink %s/%s: %v", parent, sk.GetName(), err)
		}
		if v, set := requestIntercept(ctx, got); v != intercept || (intercept && !set) {
			t.Errorf("CreateSink %s answered intercept_children %v, want %v", sk.GetName(), v, intercept)
		}
	}
	sink("projects/child-proj", &loggingpb.LogSink{Name: "own",
		Destination: "logging.googleapis.com/projects/sink-proj/locations/global/buckets/proj-agg"}, false)
	sink("organizations/1", &loggingpb.LogSink{Name: "above", IncludeChildren: true,
		Destination: "logging.googleapis.com/projects/sink-proj/locations/global/buckets/org-agg"}, false)
	sink("folders/10", &loggingpb.LogSink{Name: "grab", IncludeChildren: true, Filter: `labels.route = "grab"`,
		Destination: "logging.googleapis.com/projects/sink-proj"}, true)

	writeTo(t, s, "projects/child-proj/logs/app", "grab-1", "stay-1")

	for _, tc := range []struct {
		name string
		want []string
	}{
		{"projects/child-proj", []string{"stay-1"}},
		{"projects/sink-proj/locations/global/buckets/proj-agg/views/_AllLogs", []string{"stay-1"}},
		{"projects/sink-proj/locations/global/buckets/org-agg/views/_AllLogs", []string{"grab-1", "stay-1"}},
		{"projects/sink-proj/locations/global/buckets/_Default/views/_AllLogs", []string{"grab-1"}},
	} {
		got := read(t, s, tc.name)
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}

	// GetSink and ListSinks answer it; an update naming it clears it.
	got, err := cs.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: "folders/10/sinks/grab"})
	if v, _ := requestIntercept(ctx, got); err != nil || !v {
		t.Errorf("GetSink = %v, intercept %v", err, v)
	}
	if _, err := cs.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: "folders/10/sinks/grab", Sink: &loggingpb.LogSink{},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"intercept_children"}}}); err != nil {
		t.Fatal(err)
	}
	ls, err := cs.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: "folders/10"})
	if err != nil || len(ls.GetSinks()) != 1 {
		t.Fatalf("ListSinks = %v, %v", ls, err)
	}
	if v, _ := requestIntercept(ctx, ls.GetSinks()[0]); v {
		t.Error("intercept_children still set after an update clearing it")
	}
	writeTo(t, s, "projects/child-proj/logs/app", "grab-2")
	if got := read(t, s, "projects/child-proj"); !slices.Contains(got, "grab-2") {
		t.Errorf("after clearing intercept_children, child-proj = %v, want grab-2 in it", got)
	}
}

func TestInterceptChildrenRestrictions(t *testing.T) {
	ctx := context.Background()
	_, cs := parentsServer()
	for _, tc := range []struct {
		what, parent string
		sk           *loggingpb.LogSink
	}{
		{"a project's sink", "projects/child-proj", &loggingpb.LogSink{Name: "x", IncludeChildren: false, Destination: "logging.googleapis.com/projects/p"}},
		{"without include_children", "folders/10", &loggingpb.LogSink{Name: "x", Destination: "logging.googleapis.com/projects/p"}},
		{"to a bucket", "folders/10", &loggingpb.LogSink{Name: "x", IncludeChildren: true,
			Destination: "logging.googleapis.com/projects/p/locations/global/buckets/b"}},
		{"to Cloud Storage", "organizations/1", &loggingpb.LogSink{Name: "x", IncludeChildren: true, Destination: "storage.googleapis.com/b"}},
	} {
		markIntercept(tc.sk, true)
		_, err := cs.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: tc.parent, Sink: tc.sk})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "intercept_children") {
			t.Errorf("%s: %v, want INVALID_ARGUMENT naming intercept_children", tc.what, err)
		}
	}
}

// Over JSON, interceptChildren is taken from the body and answered.
func TestInterceptChildrenOverREST(t *testing.T) {
	a := NewAdmin(store.NewMemory())
	a.UseHierarchy(fakeHierarchy{})
	srv := httptest.NewServer(NewRESTHandler(func(g grpc.ServiceRegistrar) { a.Register(g) }))
	t.Cleanup(srv.Close)
	call := func(method, path, body string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, raw)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		return out
	}
	sk := call(http.MethodPost, "/v2/folders/10/sinks",
		`{"name":"grab","destination":"logging.googleapis.com/projects/p","includeChildren":true,"interceptChildren":true}`)
	if sk["interceptChildren"] != true {
		t.Errorf("create answered %v", sk)
	}
	if got := call(http.MethodGet, "/v2/folders/10/sinks/grab", ""); got["interceptChildren"] != true {
		t.Errorf("get answered %v", got)
	}
	list := call(http.MethodGet, "/v2/folders/10/sinks", "")
	if ss, _ := list["sinks"].([]any); len(ss) != 1 || ss[0].(map[string]any)["interceptChildren"] != true {
		t.Errorf("list answered %v", list)
	}
	if got := call(http.MethodPatch, "/v2/folders/10/sinks/grab?updateMask=intercept_children", `{"interceptChildren":false}`); got["interceptChildren"] != nil {
		t.Errorf("update answered %v", got)
	}
	if got := call(http.MethodGet, "/v2/folders/10/sinks/grab", ""); got["interceptChildren"] != nil {
		t.Errorf("get after update answered %v", got)
	}
}
