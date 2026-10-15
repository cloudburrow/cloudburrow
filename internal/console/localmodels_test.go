package console

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// fakeModels records what the page asked for.
type fakeModels struct {
	calls  []string
	states []localai.ModelState
}

func (f *fakeModels) States() []localai.ModelState { return f.states }
func (f *fakeModels) Download(id string, n int64) error {
	f.calls = append(f.calls, fmt.Sprintf("download %s %d", id, n))
	if id == "unpinned" {
		return fmt.Errorf("%w: %s", localai.ErrNotPinned, id)
	}
	if n != 9 {
		return localai.ErrSizeNotConfirmed
	}
	return nil
}
func (f *fakeModels) Cancel(id string) error { f.calls = append(f.calls, "cancel "+id); return nil }
func (f *fakeModels) Load(id string) error {
	f.calls = append(f.calls, "load "+id)
	return fmt.Errorf("%w: no runtime", localai.ErrNotLoadable)
}
func (f *fakeModels) Unload(id string) error { f.calls = append(f.calls, "unload "+id); return nil }
func (f *fakeModels) Delete(id string) error {
	f.calls = append(f.calls, "delete "+id)
	return fmt.Errorf("%w: loaded", localai.ErrBusy)
}

// TestLocalModelsPageRoutesEachActionAndNeedsTheConfirmedSize (#1306): the
// page is offered when configured, lists the manager's states, sends a
// download only with a confirmed size, and maps each refusal to a status.
func TestLocalModelsPageRoutesEachActionAndNeedsTheConfirmedSize(t *testing.T) {
	f := &fakeModels{states: []localai.ModelState{{ID: "m", Pinned: true, Bytes: 9, State: "absent"}}}
	s := New("", func(context.Context) Status { return Status{} })
	s.SetLocalModels(f)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if code, body := getJSON(t, srv, "/api/services"); code != 200 || !strings.Contains(body, `"ai-local"`) {
		t.Fatalf("services: %d %s", code, body)
	}
	if code, body := getJSON(t, srv, "/api/ai/local-models"); code != 200 ||
		!strings.Contains(body, `"configured":true`) || !strings.Contains(body, `"state":"absent"`) {
		t.Fatalf("list: %d %s", code, body)
	}
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/api/ai/local-models/m/download", `{}`, http.StatusBadRequest},
		{"/api/ai/local-models/m/download", `{"confirmBytes":8}`, http.StatusBadRequest},
		{"/api/ai/local-models/m/download", `{"confirmBytes":9}`, http.StatusOK},
		{"/api/ai/local-models/unpinned/download", `{"confirmBytes":9}`, http.StatusPreconditionFailed},
		{"/api/ai/local-models/m/cancel", ``, http.StatusOK},
		{"/api/ai/local-models/m/load", ``, http.StatusPreconditionFailed},
		{"/api/ai/local-models/m/unload", ``, http.StatusOK},
		{"/api/ai/local-models/m/delete", ``, http.StatusConflict},
		{"/api/ai/local-models/m/explode", ``, http.StatusNotFound},
	} {
		if resp, out := postJSON(t, srv, c.path, c.body); resp.StatusCode != c.want {
			t.Errorf("%s %s = %d %s, want %d", c.path, c.body, resp.StatusCode, out, c.want)
		}
	}
	want := "download m 8,download m 9,download unpinned 9,cancel m,load m,unload m,delete m"
	if got := strings.Join(f.calls, ","); got != want {
		t.Fatalf("calls %s, want %s", got, want)
	}
}

// TestLocalModelsPageIsOffOnlyWhenNotConfigured: with no manager, nothing is
// advertised and every action is refused.
func TestLocalModelsPageIsOffOnlyWhenNotConfigured(t *testing.T) {
	srv := newPlaygroundServer(t, nil)
	if _, body := getJSON(t, srv, "/api/services"); strings.Contains(body, `"ai-local"`) {
		t.Fatalf("offered without a manager: %s", body)
	}
	if resp, _ := postJSON(t, srv, "/api/ai/local-models/m/download", `{"confirmBytes":9}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("download without a manager: %d", resp.StatusCode)
	}
}
