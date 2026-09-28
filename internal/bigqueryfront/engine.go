package bigqueryfront

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// The emulator's SQL engine failing for good (#989).
//
// The pinned emulator's SQL engine, goccy/go-googlesql v0.3.0, is a
// WebAssembly module translated to Go (goccy/googlesqlwasm2go v0.1.0),
// whose memory may grow to 4 GiB (wasm2go.go: MaxMem 4294967296) but whose
// calls address it with signed 32-bit offsets: (*Module).invoke copies each
// request to `wasm2go.Memory(m.g)[reqPtr:]` with reqPtr an int32
// (googlesql.go:614, 624; handleCallback the same, 563-564). Once the
// engine's allocator hands out an address past 2 GiB, every call panics
// "slice bounds out of range [-2147...:]". The engine never gives memory
// back, and it grows with every query job that returns rows (the emulator
// keeps each result in a dataset of its own, named after the job:
// server/handler.go, addQueryResultToDynamicDestinationTable, and
// googlesqlite v0.3.1 builds a catalog with every builtin function for
// each dataset: internal/catalog.go, getOrCreateSubCatalog) and with every
// DROP TABLE (resetCatalog rebuilds the catalog of every dataset). The
// panic lands in a request, which googlesqlite recovers and the emulator
// answers 400 or 500 "googlesqlite: panic runtime error: slice bounds out
// of range [-...", or in a finalizer that frees an engine object
// ((*LanguageOptions).free), which nothing recovers: the process exits
// (code 2) and Kubernetes restarts it.
//
// Measured on the pinned image with scripts/bigquery-engine-soak.sh: the
// emulator container went from 455 MiB to 3840 MiB in 40 query jobs and 8
// CREATE TABLE and DROP TABLE pairs, and after 60 jobs a DROP TABLE was
// answered "googlesqlite: panic runtime error: slice bounds out of range
// [-2146889680:]"; from then every request, a `SELECT 1` or datasets.list
// alike, was answered 500 "failed to get projects: googlesqlite: panic
// ...", until the process was restarted. Newer releases do not change it
// (go-googlesql v0.4.0's invoke is the same). Upstream, the growth per
// query: goccy/bigquery-emulator#512 and #313, and a fix in review,
// goccy/googlesqlite#80; no upstream issue names the signed offsets.
//
// The emulator keeps its data in memory, so nothing in it survives the
// engine's failure: it answers nothing more. So the front (engineGuard)
// watches the emulator's error answers for that panic; at the first, it
// answers that request 501 with what happened, and every one after it
// until the emulator has restarted 503 with Retry-After (the clients'
// libraries retry a 503), and fails the engine's liveness path
// (EngineLivenessPath), so that the emulator is restarted at once rather
// than when a finalizer kills it. The restart empties the emulator, as the
// crash does.
//
// Since #1091 the emulator's process runs under a supervisor in its
// container (internal/supervisor), which gets the liveness path and
// restarts the process itself, within a second or two, when the path
// fails or the process ends: Kubernetes, which restarted the container
// before, waits longer after each restart (CrashLoopBackOff, up to five
// minutes), and after seven restarts on one instance left the emulator
// down for minutes. While the emulator's port refuses connections the
// front answers 503 with Retry-After too (Proxy), not 502.
//
// Since #1017 the front has a query job's result written to one dataset
// (results.go), so a query job no longer adds a catalog, and drops what it
// keeps of the emulator's jobs when the emulator restarts (restart.go,
// #1016).

// EngineLivenessPath is the front's path the emulator container's liveness
// probe gets: 200 while the engine works, 503 once it has failed for good,
// until the emulator has restarted. It is not a BigQuery path.
const EngineLivenessPath = "/cloudburrow/bigquery-engine-live"

// RestartEmulatorPath is the front's path that restarts the emulator: a
// POST to it does what the engine's failure does (the liveness path fails
// until the emulator has restarted, and requests meanwhile are answered
// 503), for a test, a soak, or an instance whose emulator has grown slow
// (#1091). It is not a BigQuery path.
const RestartEmulatorPath = "/cloudburrow/bigquery-restart-emulator"

// retryAfter is the Retry-After, in seconds, of the front's 503s while the
// emulator restarts.
const retryAfter = "1"

// writeUnavailable answers 503 backendError with Retry-After.
func writeUnavailable(w http.ResponseWriter, message string) {
	w.Header().Set("Retry-After", retryAfter)
	writeError(w, http.StatusServiceUnavailable, "backendError", message)
}

// enginePanic is the text the emulator answers with once the engine's
// memory has passed 2 GiB (a negative lower slice bound).
var enginePanic = []byte("googlesqlite: panic runtime error: slice bounds out of range [-")

// maxSniff bounds the error body the guard keeps to look for the panic.
const maxSniff = 1 << 20

// engineGuard stands between the front and the emulator (above).
type engineGuard struct {
	next http.Handler
	// alive reports whether the emulator accepts connections; nil means
	// the guard cannot watch it restart and stays tripped.
	alive func() bool
	logf  func(string, ...any)
	poll  time.Duration

	mu      sync.Mutex
	tripped bool
}

// Guard returns next, the path to the emulator at upstream (host:port),
// guarded against the engine's failure (above).
func Guard(next http.Handler, upstream string, logf func(string, ...any)) http.Handler {
	g := &engineGuard{next: next, logf: logf, poll: 100 * time.Millisecond}
	if upstream != "" {
		g.alive = func() bool {
			c, err := net.DialTimeout("tcp", upstream, time.Second)
			if err != nil {
				return false
			}
			_ = c.Close()
			return true
		}
	}
	return g
}

func (g *engineGuard) isTripped() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tripped
}

// trip marks the engine failed and, when it can, watches for the
// emulator's restart: its port closing, then opening again.
func (g *engineGuard) trip(why string) {
	g.mu.Lock()
	if g.tripped {
		g.mu.Unlock()
		return
	}
	g.tripped = true
	g.mu.Unlock()
	if g.logf != nil {
		g.logf("bigquery front: %s; failing its liveness path so that it is restarted", why)
	}
	if g.alive == nil {
		return
	}
	go func() {
		for g.alive() {
			time.Sleep(g.poll)
		}
		for !g.alive() {
			time.Sleep(g.poll)
		}
		g.mu.Lock()
		g.tripped = false
		g.mu.Unlock()
		if g.logf != nil {
			g.logf("bigquery front: the emulator has restarted")
		}
	}()
}

const engineFailed = "Not implemented here: the BigQuery emulator behind CloudBurrow can no longer run any request: its " +
	"SQL engine (goccy/go-googlesql) addresses its memory with signed 32-bit offsets and failed once that memory passed " +
	"2 GiB (\"googlesqlite: panic runtime error: slice bounds out of range\"). The memory grows with every query job and " +
	"DROP TABLE the instance has run. CloudBurrow is restarting the emulator, which keeps its data in memory: every " +
	"dataset, table and job in it is lost. Retry once the emulator is back, after recreating what the request needs. " +
	"See docs/compatibility.md, BigQuery (#989)."

const engineRestarting = "cloudburrow: the BigQuery emulator is restarting, and answers nothing until it is back, " +
	"in a second or two; retry. It keeps its data in memory, so the restarted emulator holds no dataset, table or job. " +
	"See docs/compatibility.md, BigQuery (#989, #1091)."

func (g *engineGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == EngineLivenessPath {
		if g.isTripped() {
			http.Error(w, "the BigQuery emulator's SQL engine has failed; restart it", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	if r.URL.Path == RestartEmulatorPath {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "badRequest", "POST "+RestartEmulatorPath)
			return
		}
		g.trip("a restart of the emulator was asked for (" + RestartEmulatorPath + ")")
		writeJSON(w, http.StatusAccepted, map[string]any{"restarting": true})
		return
	}
	if g.isTripped() {
		if g.alive == nil {
			// It cannot see the emulator restart, so it never passes
			// requests again.
			writeError(w, http.StatusNotImplemented, "notImplemented", engineFailed)
			return
		}
		writeUnavailable(w, engineRestarting)
		return
	}
	s := &panicSniffer{w: w}
	g.next.ServeHTTP(s, r)
	if s.sniffing && bytes.Contains(s.body.Bytes(), enginePanic) {
		g.trip(fmt.Sprintf("the emulator's SQL engine failed (%s)", enginePanic))
		h := w.Header()
		for k := range h {
			delete(h, k)
		}
		writeError(w, http.StatusNotImplemented, "notImplemented", engineFailed)
		return
	}
	s.flushSniffed()
}

// panicSniffer passes an answer through, but holds one with an error
// status until it is complete, so that the guard can replace it.
type panicSniffer struct {
	w        http.ResponseWriter
	code     int
	sniffing bool
	passed   bool // the held answer has been let through
	body     bytes.Buffer
}

func (s *panicSniffer) Header() http.Header { return s.w.Header() }

func (s *panicSniffer) WriteHeader(code int) {
	if s.code != 0 {
		return
	}
	s.code = code
	if code >= 400 && code != http.StatusBadGateway {
		s.sniffing = true
		return
	}
	s.w.WriteHeader(code)
}

func (s *panicSniffer) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.WriteHeader(http.StatusOK)
	}
	if s.sniffing && !s.passed {
		if s.body.Len()+len(b) <= maxSniff {
			return s.body.Write(b)
		}
		// Too long to be the panic's answer: let it through.
		s.flushSniffed()
	}
	return s.w.Write(b)
}

// Flush flushes an answer the sniffer is not holding.
func (s *panicSniffer) Flush() {
	if s.sniffing && !s.passed {
		return
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

// flushSniffed lets a held answer through.
func (s *panicSniffer) flushSniffed() {
	if !s.sniffing || s.passed {
		return
	}
	s.passed = true
	s.w.WriteHeader(s.code)
	_, _ = s.w.Write(s.body.Bytes())
}
