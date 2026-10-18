package firestoreadmin

// The emulator's documents, kept across its restarts (#1615).
//
// The Firestore emulator holds documents in memory alone, so they went
// with every exit of its JVM (a full heap, #1611) and with every `stop`.
// It exports a database's documents to a directory of its own filesystem
// (POST /emulator/v1/projects/{p}:export, one database a call) and imports
// such an export into a running emulator (:import, given the export's
// overall_export_metadata file), so the front, which sees every call,
// keeps them:
//
//   - every interval (30 seconds, as the Datastore emulator saves its
//     store) in which a call that may write went through the front, every
//     database the front knows of is exported into a new generation
//     directory; a manifest naming each database's export is then
//     replaced whole, and the older generation removed, so a crash during
//     an export leaves the previous generation in place;
//   - when the front stops (SIGTERM), what was written since the last
//     export is exported, and the file StoppedFile is written, which the
//     emulator's container waits for before it stops the emulator (it
//     ignores SIGTERM for that long, internal/components);
//   - each time the emulator starts (the front's first call, and every
//     restart of the emulator's container, which the front notices by a
//     sentinel document of its own going missing), the manifest's exports
//     are imported before any call is let through. In ephemeral mode a
//     front that starts discards the exports of the one before it, so the
//     documents outlive the emulator's exit alone.
//
// The directory is on the Firestore pod's volume, shared by the front's
// container and the emulator's, which run as the same user: a
// PersistentVolumeClaim in persistent mode, with the Admin API's own
// state beside it, so both survive `stop` and `up`; the pod's emptyDir in
// ephemeral mode, so the documents outlive a restart of the emulator
// alone and go with the pod.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// StoppedFile, followed by the pod's host name, is written in the
	// documents directory once the front's last export is done, as it
	// stops.
	StoppedFile = "stopped-"
	// lockFile is held, flock(2), by the running front: a deleted pod's
	// replacement, which its ReplicaSet starts at once on the same volume,
	// waits for the old front's last export before it reads the manifest
	// (as Datastore's emulator waits, #1307).
	lockFile = "lock"
	// manifestFile names each database's export of the current generation.
	manifestFile = "manifest.json"
	// sentinelProject holds the document whose absence says the emulator
	// started empty. No call through the front names it, so it is never
	// exported, listed or purged.
	sentinelProject = "cloudburrow-firestore-documents"
	sentinelPath    = "/v1/projects/" + sentinelProject + "/databases/(default)/documents/kept/sentinel"
	// gateWait bounds how long a call waits for the documents to be
	// imported; past it the call goes through.
	gateWait = 2 * time.Minute
	// stopExport bounds the export a stopping front makes. 100,000
	// documents of 1 KB export in 1.4 seconds (#1615, measured), so it
	// holds about 600,000 of them; `cloudburrow stop` exports first,
	// through ExportPath, bounded by the instance's shutdown timeout
	// alone, so a clean stop's own export finds nothing left to write.
	stopExport = 8 * time.Second
)

// ExportPath, POSTed to the front, exports what was written since the last
// export at once, and answers once it is on the volume: `cloudburrow stop`
// calls it before it stops the cluster (#1615).
const ExportPath = "/cloudburrow/firestore/documents:export"

// manifest is the current generation's exports.
type manifest struct {
	Generation int64 `json:"generation"`
	// Exports is each database's overall_export_metadata file, by
	// database name.
	Exports map[string]string `json:"exports"`
}

// Documents keeps the emulator's documents in a directory (above).
type Documents struct {
	admin    *Admin
	emulator string
	dir      string
	interval time.Duration
	client   *http.Client
	logf     func(string, ...any)

	// writes counts the calls that may write, each counted when it starts
	// and again when it ends; exported is its value when the last export
	// started.
	writes   atomic.Int64
	exported atomic.Int64

	mu       sync.Mutex
	restored chan struct{} // closed while the emulator holds the documents
	current  manifest
	// exportMu serializes exports.
	exportMu sync.Mutex
	done     chan struct{}
	stopped  string
	lock     *os.File
}

// removeOldMarkers removes the StoppedFiles of pods stopped more than a
// minute ago.
func removeOldMarkers(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), StoppedFile) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Minute {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// KeepDocuments keeps the emulator's documents in dir, exporting them
// every interval while there are writes, until ctx ends (above). The
// returned Documents gates calls (Wrap) and is done (Done) once its last
// export, as ctx ends, is.
//
// fresh discards the exports an earlier front made, keeping only this
// one's: ephemeral mode's, whose documents must not outlive the emulator
// and front that held them, as the kind node's restart (`stop` and `up`)
// restarts both and keeps the pod's emptyDir.
func (a *Admin) KeepDocuments(ctx context.Context, dir string, interval time.Duration, fresh bool, logf func(string, ...any)) (*Documents, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		logf("firestore documents: waiting for the previous front's last export to %s", dir)
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			_ = lock.Close()
			return nil, err
		}
	}
	host, _ := os.Hostname()
	stopped := filepath.Join(dir, StoppedFile+host)
	_ = os.Remove(stopped)
	removeOldMarkers(dir)
	d := &Documents{admin: a, emulator: a.emulator, dir: dir, interval: interval,
		client: &http.Client{Timeout: 5 * time.Minute}, logf: logf,
		restored: make(chan struct{}), done: make(chan struct{}), stopped: stopped, lock: lock}
	if fresh {
		_ = os.Remove(filepath.Join(dir, manifestFile))
	}
	if b, err := os.ReadFile(filepath.Join(dir, manifestFile)); err == nil {
		if err := json.Unmarshal(b, &d.current); err != nil {
			logf("firestore documents: the manifest in %s is unreadable, starting without it: %v", dir, err)
			d.current = manifest{}
		}
	}
	d.removeOtherGenerations(d.current.Generation)
	go d.run(ctx)
	return d, nil
}

// Done is closed once the documents are exported for the last time.
func (d *Documents) Done() <-chan struct{} { return d.done }

func (d *Documents) run(ctx context.Context) {
	defer close(d.done)
	check := time.NewTicker(time.Second)
	defer check.Stop()
	export := time.NewTicker(d.interval)
	defer export.Stop()
	d.checkRestored(ctx)
	for {
		select {
		case <-ctx.Done():
			// An emulator that has not been given the documents back
			// must not replace their exports with its empty databases.
			if d.dirty() && d.isRestored() {
				ectx, cancel := context.WithTimeout(context.Background(), stopExport)
				if err := d.Export(ectx); err != nil {
					d.logf("firestore documents: the last export: %v", err)
				}
				cancel()
			}
			if err := os.WriteFile(d.stopped, nil, 0o644); err != nil {
				d.logf("firestore documents: %v", err)
			}
			_ = d.lock.Close() // the next front may read the manifest
			return
		case <-check.C:
			d.checkRestored(ctx)
		case <-export.C:
			if d.dirty() && d.isRestored() {
				if err := d.Export(ctx); err != nil && ctx.Err() == nil {
					d.logf("firestore documents: export: %v", err)
				}
			}
		}
	}
}

func (d *Documents) dirty() bool { return d.writes.Load() != d.exported.Load() }

func (d *Documents) isRestored() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.restored:
		return true
	default:
		return false
	}
}

// checkRestored imports the exports when the emulator has started empty:
// its sentinel document is missing. An emulator not answering closes the
// gate until it does.
func (d *Documents) checkRestored(ctx context.Context) {
	code, err := d.call(ctx, http.MethodGet, sentinelPath, nil)
	if err != nil || code >= 500 {
		d.close()
		return
	}
	if code == http.StatusOK {
		d.open()
		return
	}
	d.close()
	if err := d.importAll(ctx); err != nil {
		d.logf("firestore documents: import: %v", err)
		return
	}
	body := []byte(`{"fields":{"at":{"stringValue":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}}}`)
	if code, err := d.call(ctx, http.MethodPatch, sentinelPath, body); err != nil || code != http.StatusOK {
		d.logf("firestore documents: the sentinel document: %d, %v", code, err)
		return
	}
	d.open()
}

func (d *Documents) open() {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.restored:
	default:
		close(d.restored)
	}
}

func (d *Documents) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.restored:
		d.restored = make(chan struct{})
	default:
	}
}

func (d *Documents) importAll(ctx context.Context) error {
	d.exportMu.Lock()
	m := d.current
	d.exportMu.Unlock()
	names := make([]string, 0, len(m.Exports))
	for n := range m.Exports {
		names = append(names, n)
	}
	sort.Strings(names)
	start := time.Now()
	for _, db := range names {
		project := strings.SplitN(db, "/", 3)[1]
		body, _ := json.Marshal(map[string]string{"database": db, "export_directory": m.Exports[db]})
		code, err := d.call(ctx, http.MethodPost, "/emulator/v1/projects/"+url.PathEscape(project)+":import", body)
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			// One database's export unreadable: the others are still
			// worth having.
			d.logf("firestore documents: import of %s from %s: HTTP %d", db, m.Exports[db], code)
		}
	}
	if len(names) > 0 {
		d.logf("firestore documents: imported %d databases of generation %d in %s", len(names), m.Generation, time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// Export exports every database the front knows of into a new
// generation, then makes it the current one.
func (d *Documents) Export(ctx context.Context) error {
	d.exportMu.Lock()
	defer d.exportMu.Unlock()
	seq := d.writes.Load()
	dbs := map[string]bool{}
	for n := range d.current.Exports {
		dbs[n] = true
	}
	d.admin.mu.Lock()
	for n := range d.admin.seen {
		dbs[n] = true
	}
	for n := range d.admin.databases {
		dbs[n] = true
	}
	d.admin.mu.Unlock()
	names := make([]string, 0, len(dbs))
	for n := range dbs {
		if databaseOf(n) == n {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	gen := d.current.Generation + 1
	gdir := filepath.Join(d.dir, "gen-"+strconv.FormatInt(gen, 10))
	next := manifest{Generation: gen, Exports: map[string]string{}}
	start := time.Now()
	for i, db := range names {
		edir := filepath.Join(gdir, strconv.Itoa(i))
		if err := os.MkdirAll(edir, 0o755); err != nil {
			return err
		}
		project := strings.SplitN(db, "/", 3)[1]
		body, _ := json.Marshal(map[string]string{"database": db, "export_directory": edir, "export_name": "export"})
		code, err := d.call(ctx, http.MethodPost, "/emulator/v1/projects/"+url.PathEscape(project)+":export", body)
		if err != nil {
			_ = os.RemoveAll(gdir)
			return err
		}
		if code != http.StatusOK {
			_ = os.RemoveAll(gdir)
			return fmt.Errorf("export of %s: HTTP %d", db, code)
		}
		next.Exports[db] = filepath.Join(edir, "export", "export.overall_export_metadata")
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(d.dir, manifestFile), b); err != nil {
		_ = os.RemoveAll(gdir)
		return err
	}
	d.current = next
	d.exported.Store(seq)
	d.removeOtherGenerations(gen)
	if took := time.Since(start); took > time.Second {
		d.logf("firestore documents: exported %d databases in %s", len(names), took.Round(time.Millisecond))
	}
	return nil
}

// removeOtherGenerations removes every generation directory but keep's.
func (d *Documents) removeOtherGenerations(keep int64) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n, ok := strings.CutPrefix(e.Name(), "gen-")
		if !ok || !e.IsDir() {
			continue
		}
		if g, err := strconv.ParseInt(n, 10, 64); err == nil && g == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(d.dir, e.Name())); err != nil {
			d.logf("firestore documents: %v", err)
		}
	}
}

func (d *Documents) call(ctx context.Context, method, path string, body []byte) (int, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+d.emulator+path, r)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	_ = res.Body.Close()
	return res.StatusCode, nil
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Wrap holds each call until the emulator holds the documents, and
// counts each call that may write, when it starts and when it ends, and,
// over gRPC, at each response message too: a Write stream lasts as long
// as its client keeps it open, and the emulator answers each of its
// writes once it is committed, so a write in the middle of a long stream
// is exported within the interval, not only once the stream ends.
func (d *Documents) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ExportPath && r.Method == http.MethodPost {
			d.serveExport(w, r)
			return
		}
		if mayWrite(r) {
			d.wait(r.Context())
			d.writes.Add(1)
			defer d.writes.Add(1)
			if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
				w = &markingWriter{ResponseWriter: w, writes: &d.writes}
			}
		} else if isDocumentsCall(r) {
			d.wait(r.Context())
		}
		next.ServeHTTP(w, r)
	})
}

// serveExport exports the documents now, when anything was written since
// the last export (ExportPath).
func (d *Documents) serveExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !d.dirty() {
		_, _ = io.WriteString(w, `{"exported":false}`)
		return
	}
	if !d.isRestored() {
		http.Error(w, `{"error":"the emulator does not hold the documents yet"}`, http.StatusServiceUnavailable)
		return
	}
	start := time.Now()
	if err := d.Export(r.Context()); err != nil {
		d.logf("firestore documents: export on request: %v", err)
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(b)
		return
	}
	_, _ = fmt.Fprintf(w, `{"exported":true,"seconds":%.3f}`, time.Since(start).Seconds())
}

// markingWriter counts each response body write of a call that may
// write: a gRPC response message, sent once the write it answers is
// committed.
type markingWriter struct {
	http.ResponseWriter
	writes *atomic.Int64
}

func (w *markingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if n > 0 {
		w.writes.Add(1)
	}
	return n, err
}

func (w *markingWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *markingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (d *Documents) wait(ctx context.Context) {
	d.mu.Lock()
	ch := d.restored
	d.mu.Unlock()
	t := time.NewTimer(gateWait)
	defer t.Stop()
	select {
	case <-ch:
	case <-ctx.Done():
	case <-t.C:
	}
}

// readMethods are the Firestore methods that never write.
var readMethods = map[string]bool{
	"GetDocument": true, "ListDocuments": true, "BatchGetDocuments": true, "RunQuery": true,
	"RunAggregationQuery": true, "PartitionQuery": true, "Listen": true, "ListCollectionIds": true,
	"BeginTransaction": true, "Rollback": true,
}

// mayWrite reports whether r is a call that may change documents: every
// Firestore and Firestore Admin call but the reads.
func mayWrite(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		i := strings.LastIndexByte(r.URL.Path, '/')
		if i < 0 {
			return false
		}
		svc, m := r.URL.Path[:i], r.URL.Path[i+1:]
		switch svc {
		case "/google.firestore.v1.Firestore":
			return !readMethods[m]
		case "/google.firestore.admin.v1.FirestoreAdmin":
			return !strings.HasPrefix(m, "Get") && !strings.HasPrefix(m, "List")
		}
		return false
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/projects/") {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	case http.MethodPost:
		_, verb, _ := strings.Cut(r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:], ":")
		switch verb {
		case "runQuery", "runAggregationQuery", "partitionQuery", "batchGet", "listCollectionIds",
			"beginTransaction", "rollback", "listen":
			return false
		}
	}
	return true
}

// isDocumentsCall reports whether r is a Firestore documents call.
func isDocumentsCall(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		return strings.HasPrefix(r.URL.Path, "/google.firestore.v1.Firestore/")
	}
	return documentsPath.MatchString(r.URL.Path)
}
