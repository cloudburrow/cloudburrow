package logging

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestCustomWriterIdentity (#1879): over JSON, sinks.create's
// customWriterIdentity becomes the sink's writer identity, kept by an
// update that names none; it is refused for a sink that does not route to
// a log bucket in another project, or that names no service account.
func TestCustomWriterIdentity(t *testing.T) {
	a := NewAdmin(store.NewMemory())
	srv := httptest.NewServer(NewRESTHandler(func(g grpc.ServiceRegistrar) { a.Register(g) }))
	t.Cleanup(srv.Close)
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	const writer = "serviceAccount:writer@proj-b.iam.gserviceaccount.com"
	code, sk := call(http.MethodPost, "/v2/projects/proj-a/sinks?customWriterIdentity="+writer,
		`{"name":"s","destination":"logging.googleapis.com/projects/proj-b/locations/global/buckets/b"}`)
	if code != http.StatusOK || sk["writerIdentity"] != writer {
		t.Fatalf("create = %d %v", code, sk)
	}
	code, sk = call(http.MethodPatch, "/v2/projects/proj-a/sinks/s?updateMask=filter", `{"filter":"severity>=ERROR"}`)
	if code != http.StatusOK || sk["writerIdentity"] != writer {
		t.Fatalf("update = %d %v", code, sk)
	}
	for _, tc := range []struct{ what, query, dest string }{
		{"to the sink's own project", writer, "logging.googleapis.com/projects/proj-a/locations/global/buckets/b"},
		{"to Cloud Storage", writer, "storage.googleapis.com/b"},
		{"not a service account", "user:x@example.com", "logging.googleapis.com/projects/proj-b/locations/global/buckets/b"},
	} {
		if code, _ := call(http.MethodPost, "/v2/projects/proj-a/sinks?customWriterIdentity="+tc.query,
			`{"name":"t","destination":"`+tc.dest+`"}`); code != http.StatusBadRequest {
			t.Errorf("%s: create = %d, want 400", tc.what, code)
		}
	}
}
