package bigqueryfront

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
)

// panicAnswer is the pinned emulator's answer once its engine's memory has
// passed 2 GiB, as scripts/bigquery-engine-soak.sh measured it (#989).
const panicAnswer = "failed to get projects: googlesqlite: panic runtime error: slice bounds out of range [-2146998320:]\n" +
	"goroutine 5915 [running]:\nruntime/debug.Stack()\n"

// TestEngineGuard (#989): the emulator's answer that its engine has failed
// for good is answered 501 with what happened, as is every request after
// it, which the emulator is not sent; the liveness path fails until the
// emulator has restarted (its port closed and opened again), and then the
// guard passes requests on again. Other answers, errors included, pass
// through as they were.
func TestEngineGuard(t *testing.T) {
	var broken atomic.Bool
	var sent atomic.Int32
	emu := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		switch {
		case broken.Load():
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, panicAnswer)
		case r.URL.Path == "/bad":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"Not found: Table p:ds.t"}}`)
		case r.URL.Path == "/other-panic":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "googlesqlite: panic runtime error: invalid memory address or nil pointer dereference")
		default:
			_, _ = io.WriteString(w, `{"kind":"bigquery#datasetList"}`)
		}
	})
	var mu sync.Mutex
	up := true
	alive := func() bool { mu.Lock(); defer mu.Unlock(); return up }
	g := &engineGuard{next: emu, alive: alive, poll: time.Millisecond}
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}

	if w := get(EngineLivenessPath); w.Code != 200 {
		t.Fatalf("liveness before: %d", w.Code)
	}
	for path, want := range map[string]int{"/ok": 200, "/bad": 404, "/other-panic": 500} {
		w := get(path)
		if w.Code != want || strings.Contains(w.Body.String(), "SQL engine") {
			t.Errorf("%s: %d %s, want %d as the emulator answered", path, w.Code, w.Body, want)
		}
	}
	if w := get("/bad"); !strings.Contains(w.Body.String(), "Not found: Table p:ds.t") {
		t.Errorf("an error passed through as %s", w.Body)
	}

	broken.Store(true)
	w := get("/bigquery/v2/projects/p/queries")
	if w.Code != 501 || !strings.Contains(w.Body.String(), "notImplemented") || !strings.Contains(w.Body.String(), "#989") ||
		strings.Contains(w.Body.String(), "goroutine") || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("the panic's answer: %d %v %s", w.Code, w.Header(), w.Body)
	}
	n := sent.Load()
	if w := get("/bigquery/v2/projects/p/datasets"); w.Code != 501 || sent.Load() != n {
		t.Errorf("after the panic: %d, sent %d more", w.Code, sent.Load()-n)
	}
	if w := get(EngineLivenessPath); w.Code != 503 {
		t.Errorf("liveness after the panic: %d", w.Code)
	}

	// The emulator restarts: its port closes, then opens.
	mu.Lock()
	up = false
	mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if w := get(EngineLivenessPath); w.Code != 503 {
		t.Errorf("liveness while the emulator is down: %d", w.Code)
	}
	broken.Store(false)
	mu.Lock()
	up = true
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for get(EngineLivenessPath).Code != 200 {
		if time.Now().After(deadline) {
			t.Fatal("liveness still failing after the emulator restarted")
		}
		time.Sleep(time.Millisecond)
	}
	if w := get("/ok"); w.Code != 200 {
		t.Errorf("after the restart: %d %s", w.Code, w.Body)
	}
}

// TestEngineGuardWithoutWatch: a guard that cannot watch the emulator
// restart stays failed, rather than send requests to an engine that
// cannot run them.
func TestEngineGuardWithoutWatch(t *testing.T) {
	emu := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, panicAnswer)
	})
	g := Guard(emu, "", nil)
	for range 2 {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("POST", "/bigquery/v2/projects/p/queries", nil))
		if w.Code != 501 {
			t.Errorf("%d %s", w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", EngineLivenessPath, nil))
	if w.Code != 503 {
		t.Errorf("liveness: %d", w.Code)
	}
}

// TestEngineLivenessPathIsTheManifests: the emulator's liveness probe gets
// the path the front serves.
func TestEngineLivenessPathIsTheManifests(t *testing.T) {
	if components.BigQueryEngineLivenessPath != EngineLivenessPath {
		t.Errorf("the manifest probes %s, the front serves %s", components.BigQueryEngineLivenessPath, EngineLivenessPath)
	}
}

// TestEngineGuardPassesLargeErrors: an error answer too long to be the
// panic's is let through whole.
func TestEngineGuardPassesLargeErrors(t *testing.T) {
	big := strings.Repeat("x", maxSniff+10)
	emu := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, big[:100])
		_, _ = io.WriteString(w, big[100:])
	})
	w := httptest.NewRecorder()
	Guard(emu, "", nil).ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 400 || w.Body.String() != big {
		t.Errorf("%d, %d bytes", w.Code, w.Body.Len())
	}
}
