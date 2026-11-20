package firestoreadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHeapWatchReadsFullCollections (#1969): the heap's maximum is read
// from the JVM's start, its use from the full collections alone (a young
// one's "after" counts the old generation's garbage), and a log shorter
// than what was read is a new JVM's.
func TestHeapWatchReadsFullCollections(t *testing.T) {
	path := filepath.Join(t.TempDir(), HeapLogFile)
	write := func(s string, appendTo bool) {
		t.Helper()
		flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if appendTo {
			flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		f, err := os.OpenFile(path, flag, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	h := &heapWatch{path: path}
	write("[0.001s] Using Serial\n[0.001s] Heap Max Capacity: 256M\n[1.0s] GC(0) Pause Young (Allocation Failure) 200M->180M(250M) 1ms\n", false)
	h.read()
	if h.maxBytes != 256<<20 || h.used != 0 || h.full() {
		t.Fatalf("after a young collection: max %d, used %d, full %v; want 256 MiB, none, not full", h.maxBytes, h.used, h.full())
	}
	write("[2.0s] GC(1) Pause Full (Allocation Failure) 240M->100M(250M) 30ms\n[2.1s] GC(2) Pause Yo", true)
	h.read()
	if h.used != 100<<20 || h.full() {
		t.Fatalf("after a full collection that left 100 MiB: used %d, full %v", h.used, h.full())
	}
	write("ung (Allocation Failure) 250M->245M(250M) 1ms\n[3.0s] GC(3) Pause Full (Allocation Failure) 250M->200M(250M) 30ms\n", true)
	h.read()
	if h.used != 200<<20 || !h.full() {
		t.Fatalf("after a full collection that left 200 MiB of 256: used %d, full %v; want full", h.used, h.full())
	}
	write("[0.001s] Heap Max Capacity: 256M\n", false)
	h.read()
	if h.used != 0 || h.full() || h.maxBytes != 256<<20 {
		t.Fatalf("after the JVM restarted: used %d, full %v, max %d", h.used, h.full(), h.maxBytes)
	}
	// G1's mixed collections and remarks count too.
	write("[5.0s] GC(9) Pause Young (Mixed) (G1 Evacuation Pause) 220M->190M(256M) 3ms\n", true)
	h.read()
	if h.used != 190<<20 {
		t.Fatalf("after a mixed collection: used %d; want 190 MiB", h.used)
	}
}

// restartingEmulator is enough of the emulator for a compaction: its
// documents, which a restart (the front's RestartFile, taken as the
// container's shell takes it) empties, the sentinel with them; and an
// export and import of them.
type restartingEmulator struct {
	dir  string
	host string

	mu       sync.Mutex
	jvm      int
	docs     map[string]string
	sentinel bool
	dead     bool // between the kill and the next JVM's start
}

func (e *restartingEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dead {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	switch {
	case r.URL.Path == sentinelPath && r.Method == http.MethodGet:
		if !e.sentinel {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	case r.URL.Path == sentinelPath && r.Method == http.MethodPatch:
		e.sentinel = true
		_, _ = w.Write([]byte(`{}`))
	case strings.HasSuffix(r.URL.Path, ":export"):
		var body struct {
			Dir string `json:"export_directory"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = os.MkdirAll(filepath.Join(body.Dir, "export"), 0o755)
		b, _ := json.Marshal(e.docs)
		_ = os.WriteFile(filepath.Join(body.Dir, "export", "export.overall_export_metadata"), b, 0o644)
		_, _ = w.Write([]byte(`{}`))
	case strings.HasSuffix(r.URL.Path, ":import"):
		var body struct {
			File string `json:"export_directory"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := os.ReadFile(body.File)
		var docs map[string]string
		_ = json.Unmarshal(b, &docs)
		for k, v := range docs {
			e.docs[k] = v
		}
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

// shell takes the front's restart requests until ctx ends: it kills the
// JVM, removes the request, and starts another, empty, a moment later.
func (e *restartingEmulator) shell(ctx context.Context) {
	req := filepath.Join(e.dir, RestartFile+e.host)
	for ctx.Err() == nil {
		time.Sleep(20 * time.Millisecond)
		if _, err := os.Stat(req); err != nil {
			continue
		}
		e.mu.Lock()
		e.dead, e.docs, e.sentinel = true, map[string]string{}, false
		e.jvm++
		e.mu.Unlock()
		_ = os.Remove(req)
		time.Sleep(200 * time.Millisecond)
		e.mu.Lock()
		e.dead = false
		e.mu.Unlock()
	}
}

// commit is the emulator's write, as the proxy behind the front would
// send it: refused while the JVM is down.
func (e *restartingEmulator) commit(key, value string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dead {
		return false
	}
	e.docs[key] = value
	return true
}

// TestACompactionLosesNoAcknowledgedWrite (#1969): writes keep going
// through the front while it compacts the emulator's versions twice: each
// is held at the gate, or finishes before the export, so every write
// acknowledged is in the emulator after the restarts; a Listen stream open
// across the compaction is ended UNAVAILABLE with a RetryInfo; and the
// database's earliest version time moves to the compaction.
func TestACompactionLosesNoAcknowledgedWrite(t *testing.T) {
	dir := t.TempDir()
	host, _ := os.Hostname()
	emu := &restartingEmulator{dir: dir, host: host, docs: map[string]string{}}
	srv := httptest.NewServer(emu)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go emu.shell(ctx)
	a := New(strings.TrimPrefix(srv.URL, "http://"))
	a.SawResource("projects/p/databases/(default)/documents/c/d")
	d, err := a.KeepDocuments(ctx, dir, time.Hour, false, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-d.Done() })

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Listen") {
			w.Header().Set("Content-Type", "application/grpc")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{0, 0, 0, 0, 0})
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			panic(http.ErrAbortHandler) // as httputil.ReverseProxy does
		}
		key := r.URL.Query().Get("k")
		if !emu.commit(key, "v") {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	front := httptest.NewServer(d.Wrap(next))
	t.Cleanup(front.Close)

	// The gate opens once the (empty) export is imported.
	for deadline := time.Now().Add(10 * time.Second); !d.isRestored(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the documents were never restored")
		}
	}
	historySince := func() time.Time { a.mu.Lock(); defer a.mu.Unlock(); return a.historySince }
	before := historySince()

	// A Listen stream, open across the compaction.
	listen := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/google.firestore.v1.Firestore/Listen", nil)
		req.Header.Set("Content-Type", "application/grpc")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("Listen: %v", err)
			listen <- nil
			return
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		listen <- res
	}()

	var acked sync.Map
	var n atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				key := fmt.Sprintf("w%d-%d", w, i)
				res, err := http.Post(front.URL+"/v1/projects/p/databases/(default)/documents:commit?k="+key, "application/json", nil)
				if err != nil {
					continue
				}
				_ = res.Body.Close()
				if res.StatusCode == http.StatusOK {
					acked.Store(key, true)
					n.Add(1)
				}
			}
		}(w)
	}
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 2; i++ {
		if err := d.Compact(ctx); err != nil {
			t.Fatalf("compaction %d: %v", i+1, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	if d.Compactions() != 2 || emu.jvm != 2 {
		t.Errorf("compactions %d, emulator restarts %d; want 2 and 2", d.Compactions(), emu.jvm)
	}
	emu.mu.Lock()
	lost := 0
	acked.Range(func(k, _ any) bool {
		if _, ok := emu.docs[k.(string)]; !ok {
			lost++
		}
		return true
	})
	emu.mu.Unlock()
	if lost > 0 || n.Load() == 0 {
		t.Errorf("%d of %d acknowledged writes lost across the compactions", lost, n.Load())
	}
	select {
	case res := <-listen:
		if res == nil {
			break
		}
		if got := res.Trailer.Get("Grpc-Status") + res.Header.Get("Grpc-Status"); got != "14" || res.Trailer.Get("Grpc-Status-Details-Bin") == "" {
			t.Errorf("the Listen stream ended with grpc-status %q, details %q; want 14 (UNAVAILABLE) with a RetryInfo", got, res.Trailer.Get("Grpc-Status-Details-Bin"))
		}
	case <-time.After(5 * time.Second):
		t.Error("the Listen stream was not ended by the compaction")
	}
	if after := historySince(); !after.After(before) {
		t.Errorf("the earliest version time is %v after the compaction, %v before; want it moved to the compaction", after, before)
	}
}
