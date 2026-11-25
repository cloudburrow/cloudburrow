package dataproc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// TestResolveApp: a workload's Spark UI path names its workload ID, and
// is resolved to the local-MILLIS application whose event log records
// it, a plain log (a batch) or a rolling zstd one (a session).
func TestResolveApp(t *testing.T) {
	dir := t.TempDir()
	env := func(w string) string {
		return `{"Event":"SparkListenerLogStart","Spark Version":"3.5.9"}` + "\n" +
			`{"Event":"SparkListenerEnvironmentUpdate","Spark Properties":{"spark.app.name":"x","spark.cloudburrow.workload":"` + w + `"}}` + "\n" +
			`{"Event":"SparkListenerApplicationStart","App ID":"ignored"}` + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "local-1"), []byte(env("app-aaaa")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "local-2.inprogress"), []byte(env("app-bbbb")), 0o644); err != nil {
		t.Fatal(err)
	}
	roll := filepath.Join(dir, "eventlog_v2_local-3")
	if err := os.Mkdir(roll, 0o755); err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	if err := os.WriteFile(filepath.Join(roll, "events_1_local-3.zstd"), enc.EncodeAll([]byte(env("app-cccc")), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{opts: Options{HistoryDir: dir}}
	for w, want := range map[string]string{"app-aaaa": "local-1", "app-bbbb": "local-2", "app-cccc": "local-3", "app-dddd": ""} {
		if got := s.resolveApp(w); got != want {
			t.Errorf("resolveApp(%s) = %q, want %q", w, got, want)
		}
	}
}

// TestResolveAppFromHistory: an event log the CLI cannot read (Spark's
// 0660, another user's on a Linux host) is resolved through the History
// Server's REST API, each application's environment read once.
func TestResolveAppFromHistory(t *testing.T) {
	envs := map[string]int{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications":
			_, _ = w.Write([]byte(`[{"id":"local-1","name":"a"},{"id":"local-2","name":"b"}]`))
		case "/api/v1/applications/local-1/environment":
			envs["local-1"]++
			_, _ = w.Write([]byte(`{"sparkProperties":[["spark.app.name","a"],["spark.cloudburrow.workload","app-aaaa"]]}`))
		case "/api/v1/applications/local-2/environment":
			envs["local-2"]++
			_, _ = w.Write([]byte(`{"sparkProperties":[["spark.cloudburrow.workload","app-bbbb"]]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hs.Close()
	addr := strings.TrimPrefix(hs.URL, "http://")
	s := &Server{opts: Options{HistoryDir: t.TempDir()}}
	for w, want := range map[string]string{"app-aaaa": "local-1", "app-bbbb": "local-2", "app-cccc": ""} {
		if got := s.resolveAppFromHistory(context.Background(), addr, w); got != want {
			t.Errorf("resolveAppFromHistory(%s) = %q, want %q", w, got, want)
		}
	}
	if envs["local-1"] != 1 || envs["local-2"] != 1 {
		t.Errorf("environment reads %v; want each application's once", envs)
	}
	if got := s.resolveApp("app-aaaa"); got != "local-1" {
		t.Errorf("resolveApp(app-aaaa) after the History Server resolved it = %q", got)
	}
}
