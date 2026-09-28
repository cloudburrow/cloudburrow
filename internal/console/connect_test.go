package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/version"
)

func connectGet(t *testing.T, s *Server, path string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9090"+path, nil)
	req.Host = "127.0.0.1:9090"
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// TestAboutIsTheBuildsVersion (#802): About is `cloudburrow version`, field
// by field, from version.Get().
func TestAboutIsTheBuildsVersion(t *testing.T) {
	w := connectGet(t, New("127.0.0.1:0", nil), "/api/about", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/about = %d: %s", w.Code, w.Body.String())
	}
	var got About
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	i := version.Get()
	want := About{Version: i.Version, Commit: i.Commit, BuildDate: i.BuildDate, GoVersion: i.GoVersion,
		Platform: i.Platform, Line: i.String()}
	if got != want {
		t.Errorf("About = %+v, want version.Get() %+v", got, want)
	}
}

// TestConnectSnippetsAreTheDocumentedOnes: every client-library snippet the
// page offers is a block of the page it cites, character for character, so
// the page never gives advice the documented, suite-run examples do not.
func TestConnectSnippetsAreTheDocumentedOnes(t *testing.T) {
	if len(snippets) == 0 {
		t.Fatal("no snippets")
	}
	langs := map[string]bool{}
	for _, s := range snippets {
		langs[s.Language] = true
		doc, err := os.ReadFile("../../" + s.Source)
		if err != nil {
			t.Fatalf("%s %s cites %s: %v", s.Language, s.Title, s.Source, err)
		}
		if !strings.Contains(string(doc), s.Code) {
			t.Errorf("the %s %s snippet is not a block of %s:\n%s", s.Language, s.Title, s.Source, s.Code)
		}
		// A variable the client reads by itself is named in the note.
		for _, n := range s.Needs {
			if !strings.Contains(s.Code, n) && !strings.Contains(s.Note, n) {
				t.Errorf("the %s %s snippet needs %s but neither reads nor names it", s.Language, s.Title, n)
			}
		}
	}
	for _, l := range []string{"Go", "Python", "Node.js"} {
		if !langs[l] {
			t.Errorf("no %s snippet", l)
		}
	}
}

// A snippet is offered only when every variable it reads is exported, so
// no example names a service the instance does not serve.
func TestSnippetsForOffersOnlyWhatIsExported(t *testing.T) {
	got := SnippetsFor([]EnvVar{{Name: "STORAGE_EMULATOR_HOST"}, {Name: "GOOGLE_CLOUD_PROJECT"}})
	if len(got) == 0 {
		t.Fatal("no snippet for an instance exporting Cloud Storage")
	}
	for _, s := range got {
		if strings.Contains(s.Code, "CLOUDBURROW_TASKS_ENDPOINT") || strings.Contains(s.Code, "CLOUDBURROW_RUN_ENDPOINT") {
			t.Errorf("offered %s %s, whose variable is not exported", s.Language, s.Title)
		}
	}
}

type fakeConnect struct {
	refuse *AdminRefusal
	auth   string
}

func (f *fakeConnect) Connect(context.Context) (Connect, error) {
	return Connect{Instance: "i", Variables: []EnvVar{{Name: "A", Value: "b"}}}, nil
}

func (f *fakeConnect) Diagnose(_ context.Context, authorization string) (string, []byte, error) {
	f.auth = authorization
	if f.refuse != nil {
		return "", nil, f.refuse
	}
	return "bundle.tar.gz", []byte("\x1f\x8bbundle"), nil
}

// The download relays the admin API's refusal with its status and message,
// names the token file on a 401, and passes the page's Authorization header
// and nothing else; accepted, it is an attachment with the bundle's name.
func TestDiagnoseDownloadRelaysTheAdminAPI(t *testing.T) {
	src := &fakeConnect{refuse: &AdminRefusal{Status: http.StatusUnauthorized, Message: "the admin API needs the instance's admin token", TokenFile: "/state/i/admin-token"}}
	s := New("127.0.0.1:0", nil)
	s.SetConnect(src)
	w := connectGet(t, s, "/api/diagnose", map[string]string{"Authorization": "Bearer wrong"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("refused download = %d, want 401", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "the admin API needs the instance's admin token" || body["token_required"] != true || body["token_file"] != "/state/i/admin-token" {
		t.Errorf("refusal body = %s", w.Body.String())
	}
	if src.auth != "Bearer wrong" {
		t.Errorf("the source was given %q, want the page's header", src.auth)
	}

	src.refuse = nil
	w = connectGet(t, s, "/api/diagnose", map[string]string{"Authorization": "Bearer right"})
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/gzip" ||
		w.Header().Get("Content-Disposition") != `attachment; filename="bundle.tar.gz"` || w.Body.String() != "\x1f\x8bbundle" {
		t.Errorf("download = %d %v %q", w.Code, w.Header(), w.Body.String())
	}
}

// A console with no instance behind it says so on both endpoints, rather
// than offering an empty environment.
func TestConnectWithoutAnInstanceSaysSo(t *testing.T) {
	s := New("127.0.0.1:0", nil)
	for _, path := range []string{"/api/connect", "/api/diagnose"} {
		w := connectGet(t, s, path, nil)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "not attached") {
			t.Errorf("GET %s with no source = %d %s", path, w.Code, w.Body.String())
		}
	}
}

// The page never asks the console for a token or shows one: the download
// sends the token the developer pasted, kept in session storage for the tab.
func TestConnectPageHoldsNoTokenOfItsOwn(t *testing.T) {
	js := consoleAsset(t, "console.js")
	start := strings.Index(js, "// --- Connect and About (#802)")
	if start < 0 {
		t.Fatal("no Connect section in console.js")
	}
	page := js[start:]
	for _, want := range []string{`sessionStorage.setItem(CONNECT_TOKEN_KEY`, `Authorization: ` + "`Bearer ${token}`", `type: "password"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the Connect page is missing %s", want)
		}
	}
	if strings.Contains(page, "localStorage") {
		t.Error("the Connect page keeps something in localStorage, which outlives the tab")
	}
}
