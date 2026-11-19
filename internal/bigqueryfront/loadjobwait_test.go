package bigqueryfront

import (
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFailedLoadWaitsForTheEmulatorsJob (main, Landing 153): a load whose
// data the front fails, through the proxy to an emulator over the network,
// is answered only once the emulator has its job, so jobs.get right after
// reports the job failed, as BigQuery does, not 404. The front cuts the
// data stream; the proxy answers at once (502, its request body failed)
// while the emulator goes on. Measured first, on an instance: after the
// emulator had restarted, jobs.get of the job right after the failure was
// 404 (TestBigQueryJobsAcrossAnEmulatorRestart).
func TestFailedLoadWaitsForTheEmulatorsJob(t *testing.T) {
	var mu sync.Mutex
	recorded := false
	emu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs/j1") {
			mu.Lock()
			ok := recorded
			mu.Unlock()
			if !ok {
				http.Error(w, `{"error":{"code":404,"message":"job j1 is not found"}}`, http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"jobReference":{"projectId":"p","jobId":"j1"},"status":{"state":"DONE"}}`)
			return
		}
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/upload/") {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		if p, err := mr.NextPart(); err == nil {
			_, _ = io.Copy(io.Discard, p)
		}
		// The emulator adds the job once it has read the job's metadata,
		// before the data (server/handler.go); on an emulator just
		// restarted that commit was slower than the front's cut stream.
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		recorded = true
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, `{"error":{"code":500,"message":"failed to read csv: unexpected EOF"}}`, http.StatusInternalServerError)
	}))
	defer emu.Close()
	h := Wrap(proxyThrough(strings.TrimPrefix(emu.URL, "http://"), t.Logf, http.DefaultTransport))
	w := upload(t, h, loadJob(`,"nullMarker":"\\N"`), ",x\n")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("the load: %d %s; want 400", w.Code, w.Body)
	}
	code, job := do(t, h, "GET", base+"/jobs/j1", "")
	if code != http.StatusOK {
		t.Fatalf("jobs.get right after the failed load: %d %v; want 200", code, job)
	}
	st, _ := job["status"].(map[string]any)
	if st == nil || st["errorResult"] == nil {
		t.Errorf("jobs.get: status %v; want the job failed", st)
	}
}
