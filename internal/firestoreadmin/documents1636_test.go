package firestoreadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
)

// exportingEmulator is enough of the emulator for the documents' export:
// the sentinel document is there, and each :export is recorded and
// answered with the metadata file the real emulator writes.
type exportingEmulator struct {
	mu       sync.Mutex
	exported []string
}

func (e *exportingEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == sentinelPath:
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":export"):
		var body struct {
			Database string `json:"database"`
			Dir      string `json:"export_directory"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = os.MkdirAll(filepath.Join(body.Dir, "export"), 0o755)
		_ = os.WriteFile(filepath.Join(body.Dir, "export", "export.overall_export_metadata"), nil, 0o644)
		e.mu.Lock()
		e.exported = append(e.exported, body.Database)
		e.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func (e *exportingEmulator) take() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.exported
	e.exported = nil
	sort.Strings(out)
	return out
}

// TestAFrontStateResetKeepsTheDocumentsExported (#1636): `POST
// /cloudburrow/front-state/reset` forgets the Admin API's databases but
// leaves the emulator's documents, so the next export still names every
// database that holds them: one only a documents call named, and one the
// Admin API made. CI's restart probe lost its document this way: written,
// the front's container killed before its next export, the front state
// reset, and `stop`'s export then left the probe's databases out.
func TestAFrontStateResetKeepsTheDocumentsExported(t *testing.T) {
	emu := &exportingEmulator{}
	srv := httptest.NewServer(emu)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := New(strings.TrimPrefix(srv.URL, "http://"))
	if err := a.KeepState(ctx, t.TempDir(), t.Logf); err != nil {
		t.Fatal(err)
	}
	d, err := a.KeepDocuments(ctx, t.TempDir(), time.Hour, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-d.Done() })

	written := "projects/cb-firestore-probe/databases/(default)"
	made := "projects/cb-firestore-probe/databases/probe-1615"
	a.SawResource(written + "/documents/durable1615/probe")
	a.mu.Lock()
	a.databases[made] = &adminpb.Database{Name: made}
	a.mu.Unlock()

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/cloudburrow/front-state/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body)
	}
	if err := d.Export(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := emu.take(), []string{written, made}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the export after the reset named %v; want %v, whose documents the emulator still holds", got, want)
	}
	// A purge of the project clears their documents (this emulator
	// refuses the clearing, which is beside the point), so they go.
	_ = a.ResetProject(ctx, "cb-firestore-probe")
	a.mu.Lock()
	left := a.seenList("cb-firestore-probe")
	a.mu.Unlock()
	if len(left) != 0 {
		t.Errorf("after the project's purge, seen = %v; want none", left)
	}
}
