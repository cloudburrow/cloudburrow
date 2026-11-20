package apienable

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

type fixed map[string]bool

func (f fixed) Disabled(_ context.Context, project, service string) (Denial, bool) {
	if f[project+"/"+service] {
		return Denial{Service: service, Title: "Cloud Pub/Sub API", Number: 123}, true
	}
	return Denial{}, false
}

func TestDenialIsGooglesError(t *testing.T) {
	d := Denial{Service: "pubsub.googleapis.com", Title: "Cloud Pub/Sub API", Number: 123}
	want := "Cloud Pub/Sub API has not been used in project 123 before or it is disabled. Enable it by visiting " +
		"https://console.developers.google.com/apis/api/pubsub.googleapis.com/overview?project=123 then retry."
	if !strings.HasPrefix(d.Message(), want) {
		t.Fatalf("message %q", d.Message())
	}
	st := d.Status()
	if st.Code() != codes.PermissionDenied {
		t.Fatalf("code %v", st.Code())
	}
	var info *errdetails.ErrorInfo
	for _, x := range st.Details() {
		if e, ok := x.(*errdetails.ErrorInfo); ok {
			info = e
		}
	}
	if info == nil || info.Reason != "SERVICE_DISABLED" || info.Domain != "googleapis.com" ||
		info.Metadata["consumer"] != "projects/123" || info.Metadata["service"] != "pubsub.googleapis.com" ||
		info.Metadata["containerInfo"] != "123" || !strings.Contains(info.Metadata["activationUrl"], "project=123") {
		t.Fatalf("ErrorInfo %v", info)
	}
}

func TestGateJSONAndGRPC(t *testing.T) {
	d := fixed{"p/pubsub.googleapis.com": true}
	served := 0
	h := Gate(d, "pubsub.googleapis.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served++ }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/projects/p/topics/t", nil))
	if rec.Code != 403 {
		t.Fatalf("REST: %d", rec.Code)
	}
	var body struct {
		Error struct {
			Status  string           `json:"status"`
			Details []map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Status != "PERMISSION_DENIED" || body.Error.Details[0]["reason"] != "SERVICE_DISABLED" {
		t.Fatalf("REST body %s", rec.Body)
	}

	r := httptest.NewRequest("POST", "/google.pubsub.v1.Publisher/Publish", nil)
	r.ProtoMajor = 2
	r.Header.Set("Content-Type", "application/grpc")
	r.Header.Set("X-Goog-Request-Params", "topic=projects%2Fp%2Ftopics%2Ft")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Header().Get("Grpc-Status") != "7" || rec.Header().Get("Grpc-Status-Details-Bin") == "" {
		t.Fatalf("gRPC headers %v", rec.Header())
	}

	for _, path := range []string{"/v1/projects/q/topics/t", "/v1/topics"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	}
	if served != 2 {
		t.Fatalf("served %d of the 2 allowed", served)
	}
}

func TestProjectOf(t *testing.T) {
	for name, want := range map[string]string{
		"projects/p/topics/t": "p", "projects/_/buckets/b": "", "/v1/projects/123/locations/l": "123",
		"organizations/o": "", "projects/p:batchGet": "p",
	} {
		if got := ProjectOfName(name); got != want {
			t.Errorf("ProjectOfName(%q) = %q, want %q", name, got, want)
		}
	}
	md := metadata.Pairs("x-goog-request-params", "parent=projects%2Fz")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	if got := projectOfContext(ctx); got != "z" {
		t.Errorf("metadata project %q", got)
	}
}

func TestRemoteCaches(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(Handler(sourceFunc(func() (Snapshot, error) {
		calls++
		return Snapshot{Projects: []Project{{ID: "p", Number: 9, Disabled: []string{"storage.googleapis.com"}}},
			Titles: map[string]string{"storage.googleapis.com": "Cloud Storage API"}}, nil
	})))
	defer srv.Close()
	hc := srv.Client()
	defer hc.CloseIdleConnections()
	c := &Remote{Base: srv.URL, Client: hc}
	if d, off := c.Disabled(context.Background(), "9", "storage.googleapis.com"); !off || d.Number != 9 || d.Title != "Cloud Storage API" {
		t.Fatalf("by number: %v %v", d, off)
	}
	if _, off := c.Disabled(context.Background(), "p", "pubsub.googleapis.com"); off {
		t.Fatal("pubsub is enabled")
	}
	if calls != 1 {
		t.Fatalf("%d fetches, want 1 within CacheFor", calls)
	}
}

type sourceFunc func() (Snapshot, error)

func (f sourceFunc) DisabledSnapshot() (Snapshot, error) { return f() }
