package dataproc

import (
	"os"
	"path/filepath"
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
