package bigqueryfront

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// OBJ.GET_ACCESS_URL's URLs (#1980): a write URL is a signed PUT that
// Cloud Storage takes, a read URL of a 30-minute duration expires after
// 30 minutes, and a GET URL does not sign a PUT.
func TestObjectRefAccessURLs(t *testing.T) {
	const email = "bqcx-2-cd34" + agentDomain
	var agents agentKeys
	agents.keep(t.TempDir(), t.Logf)
	k, err := agents.get(email)
	if err != nil {
		t.Fatal(err)
	}
	certs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !agents.serveCerts(w, r) {
			http.NotFound(w, r)
		}
	}))
	defer certs.Close()
	srv, err := storage.NewServer(storage.Options{CertSources: map[string]string{strings.TrimPrefix(agentDomain, "@"): certs.URL}})
	if err != nil {
		t.Fatal(err)
	}
	gcs := httptest.NewServer(srv)
	defer gcs.Close()
	do := func(method, u, body string) (int, string) {
		req, _ := http.NewRequest(method, u, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := do("POST", gcs.URL+"/storage/v1/b?project=p", `{"name":"bkt"}`); code != 200 {
		t.Fatalf("create bucket = %d %s", code, body)
	}
	now := time.Now()
	w, _ := signURLFor(k, email, gcs.URL, http.MethodPut, "bkt", "new.txt", now, 30*time.Minute)
	r, _ := signURLFor(k, email, gcs.URL, http.MethodGet, "bkt", "new.txt", now, 30*time.Minute)
	if !strings.Contains(w, "X-Goog-Expires=1800") {
		t.Errorf("write url = %s", w)
	}
	if code, _ := do("PUT", r, "x"); code != 403 {
		t.Errorf("PUT through the read URL = %d, want 403", code)
	}
	if code, body := do("PUT", w, "written"); code != 200 {
		t.Fatalf("PUT through the write URL = %d %s", code, body)
	}
	if code, body := do("GET", r, ""); code != 200 || body != "written" {
		t.Errorf("GET through the read URL = %d %s", code, body)
	}
	old, _ := signURLFor(k, email, gcs.URL, http.MethodGet, "bkt", "new.txt", now.Add(-31*time.Minute), 30*time.Minute)
	if code, _ := do("GET", old, ""); code != 403 {
		t.Errorf("GET through an expired read URL = %d, want 403", code)
	}
}

// promptPieces reads ObjectRef and ObjectRefRuntime fields in order.
func TestPromptPiecesObjectRefs(t *testing.T) {
	ps, ok, code, msg := promptPieces(`{"_field_1":"Describe: ","r":{"uri":"gs://b/a.png","version":"1","authorizer":"p.us.c","details":null},` +
		`"rs":[{"uri":"gs://b/c.png","version":null,"authorizer":null,"details":{"gcs_metadata":{}}}],` +
		`"rt":{"obj_ref":{"uri":"gs://b/d.png","authorizer":"p.us.c"},"access_urls":{"read_url":"http://x"}}}`)
	if !ok || code != 0 {
		t.Fatalf("promptPieces: %d %s", code, msg)
	}
	if len(ps) != 4 || ps[0].text != "Describe: " || ps[1].ref.URI != "gs://b/a.png" || ps[1].ref.authorizer() != "p.us.c" ||
		ps[2].ref.URI != "gs://b/c.png" || ps[2].ref.Authorizer != nil || ps[3].ref.URI != "gs://b/d.png" {
		t.Fatalf("pieces = %+v", ps)
	}
	if _, ok, _, _ := promptPieces(`{"a":"x","b":null}`); ok {
		t.Error("a NULL field did not make the prompt NULL")
	}
	if _, _, code, _ := promptPieces(`{"a":{"x":1}}`); code != 400 {
		t.Errorf("a STRUCT field that is not an ObjectRef = %d, want 400", code)
	}
	if got := (objRef{URI: "gs://b/o", Authorizer: strp("p.us.c")}).sql(); got != "STRUCT('gs://b/o', CAST(NULL AS STRING), 'p.us.c', CAST(NULL AS JSON))" {
		t.Errorf("sql = %s", got)
	}
}

func TestNormalizeAuthorizer(t *testing.T) {
	f := front{base: "http://engine/bigquery/v2/projects/proj"}
	for in, want := range map[string]string{"us.c": "proj.us.c", "US.c": "proj.us.c", "o.eu.c": "o.eu.c", "projects/o/locations/us/connections/c": "o.us.c"} {
		if got, bad := f.normalizeAuthorizer(in); got != want || bad != "" {
			t.Errorf("normalizeAuthorizer(%q) = %q %q, want %q", in, got, bad, want)
		}
	}
	if _, bad := f.normalizeAuthorizer("c"); bad == "" {
		t.Error("a bare connection id was taken")
	}
}
