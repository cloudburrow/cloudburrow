package firestoreadmin

// Compacting the emulator's versions (#1969).
//
// The Firestore emulator keeps every version of every document in its heap
// for as long as it runs (pitr.go): overwriting 8 documents of 10 KB
// 100,000 times filled a 1 GiB heap, and the JVM exited
// (-XX:+ExitOnOutOfMemoryError), losing what was written since the last
// export (documents.go). So the front watches the emulator's heap and,
// before it fills, compacts it:
//
//   - the emulator's JVM logs each collection to HeapLogFile, in the
//     documents directory the two containers share (-Xlog:gc,gc+init,
//     internal/components): the heap's maximum when it starts, and what is
//     in use after each collection. The front reads it every second, and
//     takes what a full collection (or G1's marking and mixed ones) left
//     in use as what the heap holds;
//   - once that is more than compactAt of the heap, the front closes the
//     gate every documents call waits at (documents.go): new calls are
//     held, not refused, as they are while the documents are imported.
//     The calls already in the emulator are let finish, for at most
//     drainGrace; the streams that never end on their own (Listen, Write)
//     are ended at once, and any call still running after the grace is
//     ended too, with UNAVAILABLE and a RetryInfo, as Google ends a stream
//     it moves (the clients retry it). A call ended so was never answered,
//     so none acknowledged is lost;
//   - the documents are exported (a new generation, as every 30 seconds),
//     and the front asks the emulator's container to restart the JVM
//     (RestartFile): the emulator's own reset of a database's documents
//     keeps their versions (Compact). The new JVM is given the export, as
//     after any restart, and the gate opens again. A failed export, or a
//     container that never takes the request, changes nothing: the gate
//     opens and the emulator keeps its documents and versions.
//
// What a compaction costs is the versions: the emulator holds none from
// before the import, so the database's earliestVersionTime moves to the
// compaction (Admin.sawImport), and a read, export or clone at an earlier
// time is refused, FAILED_PRECONDITION, as one before the window is.
// Google keeps versions for the retention period; this is the one place
// CloudBurrow keeps fewer, and docs/compatibility.md says so.

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// HeapLogFile, in the documents directory, is the emulator JVM's
// collection log (-Xlog:gc,gc+init:file=...), which the front reads for
// the heap's use.
const HeapLogFile = "emulator-gc.log"

const (
	// compactAt is the share of the heap in use after a full collection
	// past which the front compacts the emulator's versions. Serial's old
	// generation is two thirds of the heap, and a full collection comes
	// when it is full, so a half leaves room for the next one: measured
	// (#1969), 1,800 writes of 10 KB a second filled what a 256 MiB heap
	// had left after a compaction at 190 MiB in 15 seconds.
	compactAt = 0.5
	// emergencyAt is the share in use after any collection, young ones
	// included (whose count holds the old generation's garbage too), past
	// which the front compacts whatever the full collections said.
	emergencyAt = 0.8
	// compactGap is the least time between two compactions.
	compactGap = 5 * time.Second
	// compactBackoff is the time between two compactions when the
	// documents alone, as exported, fill the heap: a compaction cannot
	// help, and is not repeated every few seconds.
	compactBackoff = 5 * time.Minute
	// drainGrace is how long a compaction waits for the calls already in
	// the emulator to finish before it ends them.
	drainGrace = 5 * time.Second
	// retryAfter is the delay a call ended by a compaction is told to
	// retry after.
	retryAfter = time.Second
)

// A collection line of the JVM's unified logging: "GC(12) Pause Full
// (Allocation Failure) 149M->149M(154M) 35.662ms": in use before, after,
// and the heap's committed size. A young collection's "after" counts the
// old generation's garbage too, which only a full collection (Serial, the
// JVM's choice in a container of under 1792 MiB, as the default heap's is)
// or G1's marking and mixed collections reclaim, so only those say what
// the heap really holds.
var (
	gcLine      = regexp.MustCompile(`(Pause Full|Pause Young \(Mixed\)|Pause Remark) .*?(\d+)([KMG])->(\d+)([KMG])\((\d+)([KMG])\)`)
	anyGCLine   = regexp.MustCompile(`GC\(\d+\) Pause .*?(\d+)([KMG])->(\d+)([KMG])\((\d+)([KMG])\)`)
	gcMaxHeap   = regexp.MustCompile(`Heap Max Capacity: (\d+)([KMG])`)
	unitOfBytes = map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30}
)

func gcBytes(n, unit string) int64 {
	v, _ := strconv.ParseInt(n, 10, 64)
	return v * unitOfBytes[unit]
}

// heapWatch reads the emulator's collection log as it grows: the heap's
// maximum, which the JVM logs as it starts, and what the last full (or
// marking) collection left in use.
type heapWatch struct {
	path     string
	offset   int64
	partial  []byte
	maxBytes int64
	used     int64 // after the last full collection; 0 for none since the JVM started
	latest   int64 // after the last collection of any kind
}

// read reads what the log gained since the last read. A log shorter than
// what was read is a new one: the JVM restarted (or the log rotated).
func (h *heapWatch) read() {
	f, err := os.Open(h.path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	if info.Size() < h.offset {
		h.offset, h.partial, h.used, h.latest = 0, nil, 0, 0
	}
	if _, err := f.Seek(h.offset, io.SeekStart); err != nil {
		return
	}
	b, _ := io.ReadAll(io.LimitReader(f, 64<<20))
	h.offset += int64(len(b))
	b = append(h.partial, b...)
	i := bytes.LastIndexByte(b, '\n')
	if i < 0 {
		h.partial = b
		return
	}
	h.partial = append([]byte(nil), b[i+1:]...)
	b = b[:i]
	if m := gcMaxHeap.FindSubmatch(b); m != nil {
		h.maxBytes = gcBytes(string(m[1]), string(m[2]))
	}
	if all := anyGCLine.FindAllSubmatch(b, -1); len(all) > 0 {
		m := all[len(all)-1]
		h.latest = gcBytes(string(m[3]), string(m[4]))
	}
	if all := gcLine.FindAllSubmatch(b, -1); len(all) > 0 {
		m := all[len(all)-1]
		h.used = gcBytes(string(m[4]), string(m[5]))
	}
}

// full reports whether the last full collection left more than compactAt
// of the heap in use, or the last of any kind more than emergencyAt.
func (h *heapWatch) full() bool {
	if h.maxBytes <= 0 {
		return false
	}
	return float64(h.used) >= compactAt*float64(h.maxBytes) || float64(h.latest) >= emergencyAt*float64(h.maxBytes)
}

// call is a documents call the front has let through to the emulator.
type call struct {
	stream bool // a Listen or Write stream, which ends when its client ends it
	cancel context.CancelFunc
	cut    atomic.Bool
}

// enter waits at the gate and registers r's call, atomically with the
// gate being open, so a compaction that closes it sees every call that
// got through.
func (d *Documents) enter(r *http.Request) *call {
	c := &call{stream: isStream(r)}
	for deadline := time.Now().Add(gateWait); ; {
		d.wait(r.Context())
		d.mu.Lock()
		open := false
		select {
		case <-d.restored:
			open = true
		default:
		}
		if open || r.Context().Err() != nil || !time.Now().Before(deadline) {
			if d.inflight == nil {
				d.inflight = map[*call]struct{}{}
			}
			d.inflight[c] = struct{}{}
			d.mu.Unlock()
			return c
		}
		d.mu.Unlock()
	}
}

func (d *Documents) leave(c *call) {
	d.mu.Lock()
	delete(d.inflight, c)
	idle := len(d.inflight) == 0
	d.mu.Unlock()
	if idle {
		select {
		case d.idle <- struct{}{}:
		default:
		}
	}
}

// isStream reports whether r is a stream that lasts as long as its client
// keeps it open.
func isStream(r *http.Request) bool {
	p := r.URL.Path
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		return p == "/google.firestore.v1.Firestore/Listen" || p == "/google.firestore.v1.Firestore/Write"
	}
	return strings.HasSuffix(p, ":listen") || strings.HasSuffix(p, ":write")
}

// serveCall serves r through next as the call c, which a compaction may
// end (cutCalls): its context is cancelled, what the emulator still sends
// is dropped, and the client is answered UNAVAILABLE with a RetryInfo.
func (d *Documents) serveCall(c *call, next http.Handler, w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c.cancel = cancel
	cw := &cutWriter{ResponseWriter: w, c: c}
	func() {
		defer func() {
			if v := recover(); v != nil && !(v == http.ErrAbortHandler && c.cut.Load()) {
				panic(v)
			}
		}()
		next.ServeHTTP(cw, r.WithContext(ctx))
	}()
	if c.cut.Load() {
		cw.refuse(r)
	}
}

// cutCalls ends the registered calls: every one when all is set, the
// streams alone when not.
func (d *Documents) cutCalls(all bool) int {
	d.mu.Lock()
	var cut []*call
	for c := range d.inflight {
		if all || c.stream {
			cut = append(cut, c)
		}
	}
	d.mu.Unlock()
	for _, c := range cut {
		c.cut.Store(true)
		if c.cancel != nil {
			c.cancel()
		}
	}
	return len(cut)
}

func (d *Documents) inflightCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.inflight)
}

// drain waits, with the gate closed, until no call is in the emulator:
// the streams are ended at once, the others after drainGrace.
func (d *Documents) drain(ctx context.Context) {
	d.cutCalls(false)
	grace := time.NewTimer(drainGrace)
	defer grace.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	cutAll := false
	for d.inflightCount() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-grace.C:
			if !cutAll {
				cutAll = true
				if n := d.cutCalls(true); n > 0 {
					d.logf("firestore documents: ended %d calls still running after %s, UNAVAILABLE", n, drainGrace)
				}
				grace.Reset(drainGrace)
				continue
			}
			d.logf("firestore documents: %d calls did not end; compacting without them", d.inflightCount())
			return
		case <-d.idle:
		case <-poll.C:
			d.cutCalls(false) // a stream that began as the gate closed
		}
	}
}

// cutWriter is the response of a call a compaction may end: once ended,
// what the emulator still sends is dropped.
type cutWriter struct {
	http.ResponseWriter
	c *call

	mu      sync.Mutex
	written bool // the response's header is sent
	done    bool // the refusal is sent
}

var errCut = fmt.Errorf("the call was ended by a compaction of the emulator's versions")

func (w *cutWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.c.cut.Load() || w.written {
		return
	}
	w.written = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *cutWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.c.cut.Load() {
		return 0, errCut
	}
	w.written = true
	return w.ResponseWriter.Write(b)
}

func (w *cutWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.c.cut.Load() {
		return
	}
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *cutWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// unavailableMessage is the refusal's message.
const unavailableMessage = "The datastore operation was interrupted: the database is being compacted. Retry the request."

// refuse answers the ended call UNAVAILABLE, with a RetryInfo: over gRPC
// as a status, in the trailers when the header is already sent; over REST
// as Google's JSON error, when the header is not.
func (w *cutWriter) refuse(r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return
	}
	w.done = true
	h := w.ResponseWriter.Header()
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		st, _ := status.New(codes.Unavailable, unavailableMessage).WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(retryAfter)})
		details, _ := proto.Marshal(st.Proto())
		fields := map[string]string{
			"Grpc-Status":             strconv.Itoa(int(codes.Unavailable)),
			"Grpc-Message":            url.PathEscape(unavailableMessage),
			"Grpc-Status-Details-Bin": base64.RawStdEncoding.EncodeToString(details),
		}
		if !w.written {
			h.Del("Trailer")
			h.Set("Content-Type", "application/grpc")
			for k, v := range fields {
				h.Set(k, v)
			}
			w.ResponseWriter.WriteHeader(http.StatusOK)
			w.written = true
			return
		}
		for k, v := range fields {
			h.Set(http.TrailerPrefix+k, v)
		}
		return
	}
	if w.written {
		return // the body is cut short; nothing more can be said
	}
	for k := range h {
		delete(h, k)
	}
	h.Set("Content-Type", "application/json; charset=UTF-8")
	h.Set("Retry-After", strconv.Itoa(int(retryAfter/time.Second)))
	w.ResponseWriter.WriteHeader(http.StatusServiceUnavailable)
	w.written = true
	_, _ = fmt.Fprintf(w.ResponseWriter, `{"error":{"code":503,"message":%q,"status":"UNAVAILABLE"}}`, unavailableMessage)
}

// maybeCompact compacts the emulator's versions when its heap is past
// compactAt (above).
func (d *Documents) maybeCompact(ctx context.Context) {
	if d.heap == nil {
		return
	}
	d.heap.read()
	if !d.heap.full() || !d.isRestored() || time.Since(d.lastCompact) < d.compactWait {
		return
	}
	d.lastCompact, d.compactWait = time.Now(), compactGap
	d.logf("firestore documents: the emulator's heap holds %d of %d MiB after a full collection: compacting its versions",
		d.heap.used>>20, d.heap.maxBytes>>20)
	if err := d.Compact(ctx); err != nil && ctx.Err() == nil {
		d.logf("firestore documents: compaction: %v", err)
	}
}

// RestartFile, followed by the pod's host name, asks the emulator's
// container to restart the JVM: its shell (internal/components) removes it
// and kills the JVM, then starts another, the container staying up.
const RestartFile = "restart-"

// restartWait bounds how long a compaction waits for the emulator's shell
// to take the restart request, and then for the new JVM to answer.
const restartWait = 2 * time.Minute

// Compact drops the emulator's versions, keeping its documents (above):
// the gate is closed and the calls drained, the documents exported, the
// JVM restarted (removing a database's documents through the emulator's
// own reset drops none of their versions, measured: a 160 MiB heap that
// held 6,000 versions of a 10 KB document still exhausted itself on 9,000
// more after the reset), and the export imported into the new one, as
// after any restart (checkRestored).
func (d *Documents) Compact(ctx context.Context) error {
	start := time.Now()
	d.close()
	d.drain(ctx)
	if err := d.Export(ctx); err != nil {
		d.open()
		return fmt.Errorf("export (the emulator keeps its versions): %w", err)
	}
	req := filepath.Join(d.dir, RestartFile+d.host)
	if err := os.WriteFile(req, nil, 0o644); err != nil {
		d.open()
		return fmt.Errorf("asking for the emulator's restart: %w", err)
	}
	deadline := time.Now().Add(restartWait)
	for {
		if _, err := os.Stat(req); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			// The shell never took it: the old JVM still runs, with
			// every document and version.
			_ = os.Remove(req)
			d.open()
			return fmt.Errorf("the emulator's container did not take the restart request in %s", restartWait)
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.compactions.Add(1)
	if d.heap != nil {
		d.heap.used, d.heap.latest = 0, 0
		// The documents alone, as exported, near the heap: compacting
		// again soon cannot help.
		if b := dirBytes(filepath.Join(d.dir, "gen-"+strconv.FormatInt(d.generation(), 10))); d.heap.maxBytes > 0 && float64(b) >= compactAt*float64(d.heap.maxBytes)/2 {
			d.compactWait = compactBackoff
			d.logf("firestore documents: the documents alone are %d MiB exported, which a %d MiB heap barely holds: raise --emulator-heap-mib; the next compaction waits %s",
				b>>20, d.heap.maxBytes>>20, compactBackoff)
		}
	}
	for !d.isRestored() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the emulator has not come back in %s; the documents are imported once it does", restartWait)
		}
		time.Sleep(100 * time.Millisecond)
		d.checkRestored(ctx)
	}
	d.logf("firestore documents: compacted the emulator's versions in %s: it was restarted and given its documents back; their earliest version time is now",
		time.Since(start).Round(time.Millisecond))
	return nil
}

// Compactions is how many compactions have restarted the emulator.
func (d *Documents) Compactions() int64 { return d.compactions.Load() }

func (d *Documents) generation() int64 {
	d.exportMu.Lock()
	defer d.exportMu.Unlock()
	return d.current.Generation
}

// dirBytes is the size of the files under dir.
func dirBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
