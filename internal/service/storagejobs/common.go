// Package storagejobs serves the Cloud Storage companion APIs (#1957) over
// the instance's own buckets, in the CLI process, each on a port of its own
// and JSON only, as the official clients' REST transports and gcloud call
// them:
//
//   - the Storage Batch Operations API v1 (storagebatchoperations.googleapis.com,
//     batch.go): jobs that delete objects, rewrite their metadata, rewrite
//     them to a Cloud KMS key, set or clear holds or edit object contexts,
//     over prefixes or a CSV manifest, with counters, per-object error
//     summaries, cancel and delete;
//   - the Storage Insights API v1 (storageinsights.googleapis.com,
//     insights.go): inventory report configurations, which write CSV or
//     Parquet inventory reports of a bucket into a destination bucket on
//     their daily or weekly schedule, and the report details of each;
//   - the Storage Transfer Service API v1 (storagetransfer.googleapis.com,
//     transfer.go): transfer jobs between the instance's buckets and local
//     POSIX paths (through agent pools served by a built-in local agent),
//     with schedules, object conditions, transfer options, operations with
//     counters that can be paused, resumed and cancelled, and Pub/Sub
//     notifications.
//
// Every object is read and written through the instance's Cloud Storage
// JSON API (gcs.go), as any client would, so versioning, holds, retention,
// soft delete and CMEK behave as they do for every other caller. Sources
// outside this machine (Amazon S3, Azure Blob Storage, HTTP URL lists,
// HDFS) are refused with that reason: CloudBurrow never reaches them.
//
// Schedules follow one clock shared by the three APIs (clock.go), which a
// CloudBurrow extension moves forward (POST /cloudburrow/v1/clock:advance,
// on any of the three ports). docs/storage-companions.md is the user's
// guide.
package storagejobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/sysparams"
)

// apiError is an error with the HTTP status and Google status it answers.
type apiError struct {
	code   int
	status string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errorf(code int, status, format string, args ...any) error {
	return &apiError{code: code, status: status, msg: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return errorf(http.StatusBadRequest, "INVALID_ARGUMENT", format, args...)
}

func notFound(format string, args ...any) error {
	return errorf(http.StatusNotFound, "NOT_FOUND", format, args...)
}

func precondition(format string, args ...any) error {
	return errorf(http.StatusBadRequest, "FAILED_PRECONDITION", format, args...)
}

func exists(format string, args ...any) error {
	return errorf(http.StatusConflict, "ALREADY_EXISTS", format, args...)
}

func unimplemented(format string, args ...any) error {
	return errorf(http.StatusNotImplemented, "UNIMPLEMENTED", format, args...)
}

// localOnly is the refusal of a source or sink outside this machine.
func localOnly(what string) error {
	return unimplemented("%s is not available in CloudBurrow: CloudBurrow runs everything locally and never reaches "+
		"Amazon S3, Azure Blob Storage, HDFS or HTTP URLs outside this machine. Transfer between the instance's Cloud Storage "+
		"buckets (gcsDataSource, gcsDataSink) or from and to a local POSIX file system (posixDataSource, posixDataSink) instead.", what)
}

// Runtime is what the three APIs share: the instance's Cloud Storage, its
// Pub/Sub, the clock, and where their records are kept.
type Runtime struct {
	GCS *GCS
	// Publisher publishes transfer notifications; nil when Pub/Sub is not
	// served.
	Publisher Publisher
	// Clock is the clock every schedule follows.
	Clock *Clock
	// StateDir is where the records are kept; "" keeps them in memory.
	StateDir string
	// Linked makes Storage Insights linked datasets in the instance's
	// BigQuery; nil when BigQuery is not served (#1976).
	Linked LinkedDatasets
	// Projects lists the instance's projects for a dataset configuration's
	// scope; nil scopes each configuration to its own project.
	Projects ProjectLister

	// activity is the Cloud Storage requests the activity views are made
	// from (activity.go, #1979).
	activity activityLog

	once    sync.Once
	cancel  context.CancelFunc
	mu      sync.Mutex
	tickers []func(ctx context.Context, now time.Time)
}

// Publisher publishes one message to a Pub/Sub topic
// (projects/P/topics/T).
type Publisher interface {
	Publish(ctx context.Context, topic string, data []byte, attributes map[string]string) error
}

// NewRuntime returns a runtime over gcs with a clock at the real time.
func NewRuntime(gcs *GCS, pub Publisher, stateDir string) *Runtime {
	return &Runtime{GCS: gcs, Publisher: pub, Clock: NewClock(time.Now), StateDir: stateDir}
}

// onTick registers work run every tickInterval and after each clock
// advance, with the clock's time.
func (rt *Runtime) onTick(f func(ctx context.Context, now time.Time)) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tickers = append(rt.tickers, f)
}

const tickInterval = 2 * time.Second

// start runs the tickers in the background until stop; it is idempotent.
func (rt *Runtime) start() {
	rt.once.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		rt.cancel = cancel
		go func() {
			t := time.NewTicker(tickInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					rt.tick(ctx)
				}
			}
		}()
	})
}

// tick runs every ticker once, now.
func (rt *Runtime) tick(ctx context.Context) {
	rt.mu.Lock()
	ts := append([]func(context.Context, time.Time){}, rt.tickers...)
	rt.mu.Unlock()
	now := rt.Clock.Now()
	for _, f := range ts {
		f(ctx, now)
	}
}

func (rt *Runtime) stop() {
	if rt.cancel != nil {
		rt.cancel()
	}
}

// Clock is the companion APIs' clock: the real time plus how far it was
// advanced. It never moves back.
type Clock struct {
	now    func() time.Time
	mu     sync.Mutex
	offset time.Duration
	// moves are when, on the wall clock, the offset changed, and to what.
	moves []clockMove
}

type clockMove struct {
	at     time.Time
	offset time.Duration
}

// At is the clock's time at wall-clock time t: t with the offset in force
// then.
func (c *Clock) At(t time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var off time.Duration
	for _, m := range c.moves {
		if m.at.After(t) {
			break
		}
		off = m.offset
	}
	return t.Add(off).UTC()
}

// NewClock returns a clock reading now.
func NewClock(now func() time.Time) *Clock { return &Clock{now: now} }

// Now is the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now().Add(c.offset).UTC()
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	c.offset += d
	c.moves = append(c.moves, clockMove{at: c.now(), offset: c.offset})
	if len(c.moves) > 1000 {
		c.moves = c.moves[len(c.moves)-1000:]
	}
	c.mu.Unlock()
	return c.Now()
}

// ClockPath is the CloudBurrow extension that reads the clock (GET) and,
// with ":advance", moves it forward (POST {"duration":"3600s"}).
const ClockPath = "/cloudburrow/v1/clock"

// Advance moves the shared clock forward by d and runs every schedule now
// due before returning.
func (rt *Runtime) Advance(ctx context.Context, d time.Duration) time.Time {
	now := rt.Clock.Advance(d)
	rt.tick(ctx)
	return now
}

func (rt *Runtime) serveClock(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.URL.Path == ClockPath && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"now": rt.Clock.Now().Format(time.RFC3339Nano)})
	case r.URL.Path == ClockPath+":advance" && r.Method == http.MethodPost:
		var body struct {
			Duration string `json:"duration"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			writeError(w, invalid("the body must be {\"duration\": \"3600s\"}: %v", err))
			return true
		}
		d, err := parseDuration(body.Duration)
		if err != nil || d <= 0 {
			writeError(w, invalid("duration %q must be a positive duration such as \"3600s\"", body.Duration))
			return true
		}
		now := rt.Advance(r.Context(), d)
		writeJSON(w, map[string]any{"now": now.Format(time.RFC3339Nano)})
	default:
		return false
	}
	return true
}

// parseDuration reads a protobuf JSON duration ("3600s", "1.5s") or a Go
// duration ("1h").
func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "s") {
		if f, err := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64); err == nil {
			return time.Duration(f * float64(time.Second)), nil
		}
	}
	return time.ParseDuration(s)
}

// server is one API's HTTP server.
type server struct {
	name    string
	addr    string
	rt      *Runtime
	handler func(r *http.Request) (string, any, error)
	// Observe, if set, is called once per request with the method name and
	// the answer's HTTP status.
	Observe func(method string, code int)

	srv *http.Server
	ln  net.Listener
}

// Name is the server's name.
func (s *server) Name() string { return s.name }

// Start binds the listener and serves in the background.
func (s *server) Start(context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("%s: listen on %s: %w", s.name, s.addr, err)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: hostguard.Wrap(sysparams.Wrap(s, sysparams.Options{})), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	s.rt.start()
	return nil
}

// Stop shuts the server down.
func (s *server) Stop(ctx context.Context) error {
	s.rt.stop()
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// Addr is the bound address, empty before Start.
func (s *server) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.rt.serveClock(w, r) {
		return
	}
	method, out, err := s.handler(r)
	code := http.StatusOK
	if err != nil {
		code = writeError(w, err)
	} else {
		writeJSON(w, out)
	}
	if s.Observe != nil {
		s.Observe(method, code)
	}
}

func writeError(w http.ResponseWriter, err error) int {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = &apiError{code: http.StatusInternalServerError, status: "INTERNAL", msg: err.Error()}
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(ae.code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": ae.code, "message": ae.msg, "status": ae.status,
	}})
	return ae.code
}

func writeJSON(w http.ResponseWriter, out any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	if m, ok := out.(proto.Message); ok {
		b, err := protojson.MarshalOptions{}.Marshal(m)
		if err != nil {
			writeError(w, err)
			return
		}
		_, _ = w.Write(b)
		return
	}
	if out == nil {
		out = map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// decode reads a JSON request body into m, which may be empty.
func decode(r *http.Request, m proto.Message) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return invalid("reading the request body: %v", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, m); err != nil {
		return invalid("Invalid JSON payload received. %v", err)
	}
	return nil
}

// marshalAny is m as a JSON object with its "@type", for an operation's
// metadata or response.
func marshalAny(typeURL string, m proto.Message) map[string]any {
	out := map[string]any{}
	if b, err := protojson.Marshal(m); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	out["@type"] = typeURL
	return out
}

// newID is a random lowercase hex ID of n bytes.
func newID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newUUID is a random RFC 4122 version 4 UUID.
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// pageOf pages items of a list by an offset page token.
func pageOf[T any](items []T, pageSize int32, token string, def, max int) ([]T, string, error) {
	start := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 || n > len(items) {
			return nil, "", invalid("Invalid page token %q.", token)
		}
		start = n
	}
	size := int(pageSize)
	if size <= 0 {
		size = def
	}
	if size > max {
		size = max
	}
	end := min(start+size, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next, nil
}

// persist keeps v as JSON in the runtime's state directory, under file.
func (rt *Runtime) persist(file string, v any) {
	if rt.StateDir == "" {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = os.MkdirAll(rt.StateDir, 0o700)
	tmp := filepath.Join(rt.StateDir, file+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(rt.StateDir, file))
	}
}

// restore reads what persist kept under file into v.
func (rt *Runtime) restore(file string, v any) {
	if rt.StateDir == "" {
		return
	}
	if b, err := os.ReadFile(filepath.Join(rt.StateDir, file)); err == nil {
		_ = json.Unmarshal(b, v)
	}
}

// protoJSON and fromProtoJSON keep a message in a persisted record.
func protoJSON(m proto.Message) json.RawMessage {
	b, _ := protojson.Marshal(m)
	return b
}

func fromProtoJSON(b json.RawMessage, m proto.Message) {
	if len(b) > 0 {
		_ = protojson.Unmarshal(b, m)
	}
}
