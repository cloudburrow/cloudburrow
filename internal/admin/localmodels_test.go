package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// fakeModels records what the admin API asked for.
type fakeModels struct{ calls []string }

func (f *fakeModels) States() []localai.ModelState {
	return []localai.ModelState{{ID: "m", Pinned: true, State: "downloaded", Loadable: true, Loaded: true}}
}
func (f *fakeModels) Download(id string, n int64) error {
	f.calls = append(f.calls, fmt.Sprintf("download %s %d", id, n))
	return nil
}
func (f *fakeModels) Cancel(id string) error { f.calls = append(f.calls, "cancel "+id); return nil }
func (f *fakeModels) Load(id string) error   { f.calls = append(f.calls, "load "+id); return nil }
func (f *fakeModels) Unload(id string) error { f.calls = append(f.calls, "unload "+id); return nil }
func (f *fakeModels) Delete(id string) error {
	f.calls = append(f.calls, "delete "+id)
	return fmt.Errorf("%w: m is loaded", localai.ErrBusy)
}

// TestAdminLocalModelsNeedTheTokenAndRunEachAction (#1396): the local
// models' admin routes require the admin token like every other admin
// route, list the manager's states, run each action through the same
// localai.Act as the console, and refuse a download without a confirmed size
// before the manager is asked.
func TestAdminLocalModelsNeedTheTokenAndRunEachAction(t *testing.T) {
	a := NewAPI(NewRecorder(10, nil))
	a.RequireToken("s3cret")
	srv := serve(a)
	defer srv.Close()
	do := func(method, path, body, auth string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := do("GET", "/admin/local-models", "", "s3cret"); code != http.StatusNotFound {
		t.Fatalf("with no manager = %d %s, want 404", code, body)
	}
	f := &fakeModels{}
	a.SetLocalModels(f)
	for _, c := range []struct{ method, path string }{
		{"GET", "/admin/local-models"}, {"POST", "/admin/local-models/m/unload"},
	} {
		if code, _ := do(c.method, c.path, "", ""); code != http.StatusUnauthorized {
			t.Errorf("%s %s without the token = %d, want 401", c.method, c.path, code)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("a refused request reached the manager: %v", f.calls)
	}

	code, body := do("GET", "/admin/local-models", "", "s3cret")
	var list struct {
		Models  []localai.ModelState `json:"models"`
		Actions []string             `json:"actions"`
	}
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil ||
		len(list.Models) != 1 || !list.Models[0].Loaded || len(list.Actions) != 5 {
		t.Fatalf("list = %d %s", code, body)
	}

	if code, body := do("POST", "/admin/local-models/m/download", `{}`, "s3cret"); code != http.StatusBadRequest {
		t.Errorf("download without confirmBytes = %d %s, want 400", code, body)
	}
	for _, c := range []struct {
		action, body string
		want         int
	}{
		{"download", `{"confirmBytes":9}`, http.StatusOK},
		{"cancel", "", http.StatusOK},
		{"unload", "", http.StatusOK},
		{"load", "", http.StatusOK},
		{"delete", "", http.StatusConflict},
		{"explode", "", http.StatusNotFound},
	} {
		if code, body := do("POST", "/admin/local-models/m/"+c.action, c.body, "s3cret"); code != c.want {
			t.Errorf("%s = %d %s, want %d", c.action, code, body, c.want)
		}
	}
	want := "download m 9,cancel m,unload m,load m,delete m"
	if got := strings.Join(f.calls, ","); got != want {
		t.Errorf("manager calls = %s, want %s", got, want)
	}
}
