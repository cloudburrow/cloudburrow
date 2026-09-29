package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestScanJobsReadsSeveralAtOnce (#1135): the first time a project's
// functions are asked for, the jobs the emulator ran before the front
// started are read several at once, a job whose configuration the front
// has is not read, and every CREATE FUNCTION among them is known.
func TestScanJobsReadsSeveralAtOnce(t *testing.T) {
	const n = 64
	var inFlight, most, gets atomic.Int64
	emu := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/jobs"):
			var jobs []string
			for i := 0; i < n; i++ {
				jobs = append(jobs, fmt.Sprintf(`{"jobReference":{"jobId":"j%d"},"statistics":{"creationTime":"1"}}`, i))
			}
			_, _ = io.WriteString(w, `{"jobs":[`+strings.Join(jobs, ",")+`]}`)
		case strings.Contains(path, "/jobs/"):
			gets.Add(1)
			now := inFlight.Add(1)
			defer inFlight.Add(-1)
			for m := most.Load(); now > m && !most.CompareAndSwap(m, now); m = most.Load() {
			}
			time.Sleep(10 * time.Millisecond)
			id := path[strings.LastIndex(path, "/")+1:]
			q := "SELECT 1"
			if id == "j7" || id == "j40" {
				q = "CREATE FUNCTION ds.fn_" + id + "(x INT64) AS (x)"
			}
			b, _ := json.Marshal(map[string]any{"jobReference": map[string]any{"jobId": id},
				"configuration": map[string]any{"query": map[string]any{"query": q}}})
			_, _ = w.Write(b)
		default:
			http.NotFound(w, r)
		}
	})
	configs := &jobConfigs{}
	configs.add("p", "j3", json.RawMessage(`{"query":{"query":"CREATE FUNCTION ds.fn_j3(x INT64) AS (x)"}}`))
	f := front{next: emu, base: "/bigquery/v2/projects/p", configs: configs, functions: &knownFunctions{started: 1000}}
	r := httptest.NewRequest(http.MethodGet, "/bigquery/v2/projects/p/datasets", nil)
	got := map[string]bool{}
	for _, p := range f.functionsIn(r, "p", "ds") {
		got[strings.Join(p, ".")] = true
	}
	want := map[string]bool{"ds.fn_j3": true, "ds.fn_j7": true, "ds.fn_j40": true}
	if len(got) != len(want) || !got["ds.fn_j3"] || !got["ds.fn_j7"] || !got["ds.fn_j40"] {
		t.Errorf("functions %v, want %v", got, want)
	}
	if gets.Load() != n-1 {
		t.Errorf("%d jobs.get, want %d: j3's configuration was known", gets.Load(), n-1)
	}
	if most.Load() < 2 || most.Load() > scanReaders {
		t.Errorf("at most %d jobs.get at once, want 2 to %d", most.Load(), scanReaders)
	}
	if _, ok := configs.get("p", "j40"); !ok {
		t.Errorf("a job read was not kept in jobConfigs")
	}
}
