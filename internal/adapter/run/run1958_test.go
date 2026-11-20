package run

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// TestMergeInstanceMask (#1958): UpdateInstance's mask takes the named
// fields from the request and keeps the rest; an output-only path is
// INVALID_ARGUMENT.
func TestMergeInstanceMask(t *testing.T) {
	cur := &runpb.Instance{Name: "projects/p/locations/l/instances/i", Description: "old", Labels: map[string]string{"a": "1"},
		Containers: []*runpb.Container{{Image: "img:1"}}}
	in := &runpb.Instance{Description: "new", Containers: []*runpb.Container{{Image: "img:2"}}}
	out, err := mergeInstanceMask(cur, in, []string{"containers"})
	if err != nil {
		t.Fatal(err)
	}
	if out.GetDescription() != "old" || out.GetContainers()[0].GetImage() != "img:2" || out.GetLabels()["a"] != "1" || out.GetName() != cur.GetName() {
		t.Errorf("merged = %v", out)
	}
	if _, err := mergeInstanceMask(cur, in, []string{"uid"}); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("an output-only path = %v; want INVALID_ARGUMENT", err)
	}
	all, err := mergeInstanceMask(cur, in, []string{"*"})
	if err != nil || all.GetDescription() != "new" || all.GetLabels() != nil || all.GetName() != cur.GetName() {
		t.Errorf("* = %v, %v", all, err)
	}
}

// TestFunctionVersionsAndPaging (#1958): a 1st-gen function's earlier
// versions are kept and read by v1 versionId and v2 revision, its download
// URL names that version's source, and ListFunctions pages and orders.
func TestFunctionVersionsAndPaging(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	f := s.Functions(s.Builds(fakeFetcher{}, nil), func() string { return "http://run" })
	v := f.V1()
	const loc = "projects/proj-1958/locations/us-central1"
	name := loc + "/functions/versions"
	gen2 := loc + "/functions/gen2"
	fn := &functionspb.Function{Name: name, Environment: functionspb.Environment_GEN_1, State: functionspb.Function_ACTIVE,
		CreateTime: timestamppb.Now(), UpdateTime: timestamppb.Now(),
		BuildConfig: &functionspb.BuildConfig{Runtime: "go123", EntryPoint: "Hello",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: &functionspb.StorageSource{Bucket: "b", Object: "v1.zip"}}}},
		ServiceConfig: &functionspb.ServiceConfig{TimeoutSeconds: 60}}
	functions.mu.Lock()
	functions.fns[name] = &storedFunction{fn: fn, version: 1, v1: &fv1.CloudFunction{Name: name, Description: "first", Runtime: "go123",
		EntryPoint: "Hello", SourceCode: &fv1.CloudFunction_SourceArchiveUrl{SourceArchiveUrl: "gs://b/v1.zip"},
		Trigger: &fv1.CloudFunction_HttpsTrigger{HttpsTrigger: &fv1.HttpsTrigger{}}}}
	functions.fns[gen2] = &storedFunction{fn: &functionspb.Function{Name: gen2, Environment: functionspb.Environment_GEN_2,
		CreateTime: timestamppb.New(time.Unix(1, 0))}}
	functions.mu.Unlock()
	t.Cleanup(func() {
		functions.mu.Lock()
		delete(functions.fns, name)
		delete(functions.fns, gen2)
		functions.mu.Unlock()
	})
	ctx := context.Background()
	op, err := v.UpdateFunction(ctx, &fv1.UpdateFunctionRequest{Function: &fv1.CloudFunction{Name: name, Description: "second"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil {
		t.Fatalf("UpdateFunction: %v", err)
	}
	waitOp(t, s, op.GetName())
	cur, err := v.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name})
	if err != nil || cur.GetVersionId() != 2 || cur.GetDescription() != "second" {
		t.Fatalf("GetFunction = %v, %v; want version 2", cur, err)
	}
	old, err := v.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name, VersionId: 1})
	if err != nil || old.GetVersionId() != 1 || old.GetDescription() != "first" {
		t.Errorf("GetFunction(versionId 1) = %v, %v; want the first version", old, err)
	}
	if _, err := v.GetFunction(ctx, &fv1.GetFunctionRequest{Name: name, VersionId: 9}); apierror.From(err).Code != codes.NotFound {
		t.Errorf("GetFunction(versionId 9) = %v; want NOT_FOUND", err)
	}
	dl, err := v.GenerateDownloadUrl(ctx, &fv1.GenerateDownloadUrlRequest{Name: name, VersionId: 1})
	if err != nil || dl.GetDownloadUrl() != "http://run"+downloadPrefix+"b/v1.zip" {
		t.Errorf("GenerateDownloadUrl(versionId 1) = %v, %v", dl, err)
	}
	if r, err := f.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name, Revision: "1"}); err != nil || r.GetName() != name {
		t.Errorf("v2 GetFunction(revision 1) = %v, %v", r, err)
	}
	if _, err := f.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: gen2, Revision: "1"}); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("v2 GetFunction(revision) of a 2nd gen function = %v; want INVALID_ARGUMENT", err)
	}
	if _, err := f.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name, Revision: "x"}); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("v2 GetFunction(revision x) = %v; want INVALID_ARGUMENT", err)
	}

	page, err := f.ListFunctions(ctx, &functionspb.ListFunctionsRequest{Parent: loc, PageSize: 1})
	if err != nil || len(page.GetFunctions()) != 1 || page.GetFunctions()[0].GetName() != gen2 || page.GetNextPageToken() == "" {
		t.Fatalf("page 1 = %v, %v", page, err)
	}
	page2, err := f.ListFunctions(ctx, &functionspb.ListFunctionsRequest{Parent: loc, PageSize: 1, PageToken: page.GetNextPageToken()})
	if err != nil || len(page2.GetFunctions()) != 1 || page2.GetFunctions()[0].GetName() != name || page2.GetNextPageToken() != "" {
		t.Errorf("page 2 = %v, %v", page2, err)
	}
	desc, err := f.ListFunctions(ctx, &functionspb.ListFunctionsRequest{Parent: loc, OrderBy: "name desc"})
	if err != nil || len(desc.GetFunctions()) != 2 || desc.GetFunctions()[0].GetName() != name {
		t.Errorf("order_by name desc = %v, %v", desc, err)
	}
	if _, err := f.ListFunctions(ctx, &functionspb.ListFunctionsRequest{Parent: loc, OrderBy: "runtime"}); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("order_by runtime = %v; want INVALID_ARGUMENT", err)
	}
}

// TestCallEventFunction (#1958): v1 call of a Pub/Sub function delivers
// data as the message of a messagePublished CloudEvent, and of a Cloud
// Storage function as the object of a finalized one.
func TestCallEventFunction(t *testing.T) {
	var mu sync.Mutex
	var got *http.Request
	var gotBody string
	fnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, gotBody = r, string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer fnSrv.Close()
	f := (&FunctionsServer{}).WithEvents(Events{Transport: &fakeTransport{}, Tokens: &fakeTokens{},
		Ingress: strings.TrimPrefix(fnSrv.URL, "http://")})
	v := f.V1()
	fn := &functionspb.Function{Name: "projects/p/locations/us-central1/functions/ps",
		ServiceConfig: &functionspb.ServiceConfig{Uri: "http://ps.ns.localhost", TimeoutSeconds: 10},
		EventTrigger:  &functionspb.EventTrigger{EventType: pubsubPublished, PubsubTopic: "projects/p/topics/t"}}
	resp, err := v.callEvent(context.Background(), fn.GetName(), fn, `{"data":"`+base64.StdEncoding.EncodeToString([]byte("hi"))+`"}`)
	if err != nil || resp.GetError() != "" || resp.GetExecutionId() == "" {
		t.Fatalf("callEvent = %v, %v", resp, err)
	}
	var mpd struct {
		Message map[string]any `json:"message"`
	}
	_ = json.Unmarshal([]byte(gotBody), &mpd)
	if got.Header.Get("ce-type") != pubsubPublished || got.Header.Get("ce-source") != "//pubsub.googleapis.com/projects/p/topics/t" ||
		mpd.Message["data"] != base64.StdEncoding.EncodeToString([]byte("hi")) || mpd.Message["messageId"] == "" {
		t.Errorf("the function got %v %s", got.Header, gotBody)
	}
	fn.EventTrigger = &functionspb.EventTrigger{EventType: storageEventPrefix + "finalized",
		EventFilters: []*functionspb.EventFilter{{Attribute: "bucket", Value: "bkt"}}}
	if _, err := v.callEvent(context.Background(), fn.GetName(), fn, `{"name":"o.txt"}`); err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("ce-subject") != "objects/o.txt" || got.Header.Get("ce-bucket") != "bkt" || !strings.Contains(gotBody, `"bucket":"bkt"`) {
		t.Errorf("the function got %v %s", got.Header, gotBody)
	}
	if _, err := v.callEvent(context.Background(), fn.GetName(), fn, `not json`); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("callEvent of data that is not JSON = %v; want INVALID_ARGUMENT", err)
	}
}

type putRecorder struct {
	fakeFetcher
	puts map[string][]byte
}

func (p *putRecorder) Put(_ context.Context, _, bucket, object string, data []byte, _ bool) error {
	p.puts[bucket+"/"+object] = data
	return nil
}

// TestUploadSourceMedia (#1958): a multipart media upload is written to
// run-sources-{project}-{region} under services/{name}/, and a request with
// no media is INVALID_ARGUMENT.
func TestUploadSourceMedia(t *testing.T) {
	s := NewServer(&Knative{}, "t", time.Minute)
	rec := &putRecorder{puts: map[string][]byte{}}
	u := s.Builds(rec, nil).SourceUpload()
	h := u.Handler(http.NotFoundHandler())
	body := "--b\r\nContent-Type: application/json\r\n\r\n{\"service\":\"svc\"}\r\n--b\r\nContent-Type: application/gzip\r\n\r\n\x1f\x8bsource\r\n--b--\r\n"
	req := httptest.NewRequest(http.MethodPost, "/upload/v2/projects/googleproj/locations/us-central1:uploadSource?uploadType=multipart", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/related; boundary=b")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var resp struct {
		CloudStorageSource struct{ Bucket, Object string } `json:"cloudStorageSource"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusOK {
		t.Fatalf("upload = %d %s", w.Code, w.Body)
	}
	cs := resp.CloudStorageSource
	if cs.Bucket != "run-sources-elgoogproj-us-central1" || !strings.HasPrefix(cs.Object, "services/svc/") || !strings.HasSuffix(cs.Object, ".tar.gz") ||
		!bytes.Equal(rec.puts[cs.Bucket+"/"+cs.Object], []byte("\x1f\x8bsource")) {
		t.Errorf("stored %v at %+v", rec.puts, cs)
	}
	if _, err := u.UploadSource(context.Background(), &runpb.UploadSourceRequest{Parent: "projects/p/locations/us-central1"}); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("UploadSource with no media = %v; want INVALID_ARGUMENT", err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/projects/p/locations/l/services", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("another path = %d; want it passed on", w.Code)
	}
}
