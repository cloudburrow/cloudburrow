package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleLogNamePicker (#1554): /api/logs/names, behind the Logs
// Explorer's Log name picker, is Cloud Logging's ListLogs of the scope's
// parent: a project's log names, each once, and not another project's; a parent ListLogs refuses is refused in its words. An instance with no
// Logging API answers 501, and the page draws no picker. The picker's choice
// is the resource filter, which a log's entries carry as their resource.
func TestConsoleLogNamePicker(t *testing.T) {
	st := logging.NewStore(100)
	svc := &loggingService{store: st, api: logging.NewServer(st), admin: logging.NewAdmin(store.NewMemory())}
	svc.api.UseAdmin(svc.admin)
	srv := console.New("127.0.0.1:0", nil)
	srv.SetLogNameLister(svc.logNameLister(nil))
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	now := time.Now()
	writeConsoleEntries(t, svc,
		textEntry("projects/proj-1/logs/app", "a", "INFO", now),
		textEntry("projects/proj-1/logs/cloudaudit.googleapis.com%2Factivity", "b", "INFO", now),
		textEntry("projects/proj-1/logs/app", "c", "INFO", now),
		textEntry("projects/proj-2/logs/other", "d", "INFO", now),
	)

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(hs.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	names := func(parent string) []string {
		t.Helper()
		code, body := get("/api/logs/names?parent=" + parent)
		var got struct{ LogNames []string }
		if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil {
			t.Fatalf("names of %s = %d %s", parent, code, body)
		}
		return got.LogNames
	}
	if got, want := names("projects/proj-1"), []string{"projects/proj-1/logs/app", "projects/proj-1/logs/cloudaudit.googleapis.com%2Factivity"}; !slices.Equal(got, want) {
		t.Errorf("proj-1's log names = %v; want %v (each once, not proj-2's)", got, want)
	}
	if got := names("projects/empty-proj"); got == nil || len(got) != 0 {
		t.Errorf("an empty project's log names = %#v; want []", got)
	}
	if code, body := get("/api/logs/names?parent=nonsense"); code != http.StatusBadRequest || !strings.Contains(body, "nonsense") {
		t.Errorf("a parent ListLogs refuses = %d %s", code, body)
	}
	if code, _ := get("/api/logs/names"); code != http.StatusBadRequest {
		t.Errorf("no parent = %d; want 400", code)
	}

	bare := httptest.NewServer(console.New("127.0.0.1:0", nil).Handler())
	defer bare.Close()
	resp, err := http.Get(bare.URL + "/api/logs/names?parent=projects/proj-1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("with no Logging API, names = %d; want 501", resp.StatusCode)
	}

	js, err := os.ReadFile("../../internal/console/assets/console.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id: "log-name"`, "/api/logs/names?", "resource.value = logName.value"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("the Logs Explorer is missing %q", want)
		}
	}
}
