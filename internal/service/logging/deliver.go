package logging

// Sink delivery beyond log buckets (#1404), to
// docs.cloud.google.com/logging/docs/export/configure_export_v2 and the
// pages of each destination:
//
//   - storage.googleapis.com/BUCKET: entries are batched and written hourly,
//     as Google does, in the objects CopyLogEntries writes (copy.go, #1394):
//     LOG_ID/YYYY/MM/DD/HH:00:00_HH:59:59_S0.json, one LogEntry JSON per
//     line, _A0:UNIX for entries received in another hour than their
//     timestamp's. An hour's batch is written once the Logging clock is past
//     the hour it was received in: by a background check every
//     flushInterval, or at once by the CloudBurrow extension
//     POST /cloudburrow/logging/v2/clock:advance (ClockPath), which moves
//     the clock forward, as BigQuery's front clock does.
//   - pubsub.googleapis.com/projects/P/topics/T: each entry is published as
//     one message, its data the LogEntry JSON and its attribute
//     logging.googleapis.com/timestamp the entry's timestamp.
//   - bigquery.googleapis.com/projects/P/datasets/D: each entry is streamed
//     (tabledata.insertAll) into a table named for its log: the log ID with
//     every character but letters, digits and underscores made an
//     underscore, then _YYYYMMDD of its UTC timestamp, or, with
//     bigquery_options.use_partitioned_tables, one table partitioned by day
//     on its timestamp column. The table's schema is made from the first
//     entry and grows by the fields later entries add; an entry that does
//     not fit it goes to the dataset's export_errors table (bigquery.go).
//   - logging.googleapis.com/projects/P: the entry goes to P's _Default
//     bucket, unless one of P's exclusions matches it, and through P's own
//     sinks, once: a project destination of P's is not followed again.
//
// A delivery that fails is recorded on the sink (SinkStatus, for the
// console) and, as Google reports sink errors, as an ERROR entry in the
// sink's parent's log logging.googleapis.com/sink_error, resource
// logging_sink, once a day per sink and error code. The entry itself is
// dropped. That entry is stored in the parent's _Default bucket and is not
// routed by sinks, so a sink failing cannot feed itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ErrDestinationNotFound is what a destination writer wraps when the
// bucket, topic or dataset does not exist.
var ErrDestinationNotFound = errors.New("destination not found")

// Message is one Pub/Sub message a sink publishes.
type Message struct {
	Data       []byte
	Attributes map[string]string
}

// TopicPublisher publishes to the instance's Pub/Sub.
type TopicPublisher interface {
	Publish(ctx context.Context, topic string, msgs []Message) error
}

// TableWriter streams rows into the instance's BigQuery.
type TableWriter interface {
	// TableSchema is a table's schema; found is false for a table that does
	// not exist in a dataset that does. A missing dataset wraps
	// ErrDestinationNotFound.
	TableSchema(ctx context.Context, project, dataset, table string) (fields []Field, found bool, err error)
	// CreateTable makes a table, partitioned by day on its timestamp column
	// when partitioned is set.
	CreateTable(ctx context.Context, project, dataset, table string, fields []Field, partitioned bool) error
	// ExtendTable sets a table's schema to fields, which holds every field
	// it had.
	ExtendTable(ctx context.Context, project, dataset, table string, fields []Field) error
	// InsertRows streams rows, each with its insert ID.
	InsertRows(ctx context.Context, project, dataset, table string, rows []map[string]any, insertIDs []string) error
}

// Destinations are where sinks deliver, beyond log buckets; nil writers
// are services the instance does not serve.
type Destinations struct {
	Storage  ObjectWriter
	PubSub   TopicPublisher
	BigQuery TableWriter
}

// ClockPath is the CloudBurrow extension that moves the Logging clock
// forward: POST {"seconds": n}, n not negative, answers {"time": RFC 3339}.
// Cloud Storage sink batches whose hour has ended by then are written
// before it answers.
const ClockPath = "/cloudburrow/logging/v2/clock:advance"

// flushInterval is how often due Cloud Storage batches are looked for.
const flushInterval = 30 * time.Second

// SinkStatus is what a sink has delivered, for the console.
type SinkStatus struct {
	Delivered    int64
	LastDelivery time.Time
	Pending      int // entries batched for Cloud Storage, not yet written
	// ErrorRows are the entries a BigQuery sink wrote to its dataset's
	// export_errors table (#1451).
	ErrorRows   int64
	LastError   string
	LastErrorAt time.Time
}

// delivery is one entry a sink sends to a destination other than a log
// bucket.
type delivery struct {
	sink        string // the sink's resource name
	parent      parent
	dest        string
	partitioned bool
	e           *loggingpb.LogEntry
}

// deliverer holds what delivery needs.
type deliverer struct {
	mu     sync.Mutex
	dest   Destinations
	status map[string]*SinkStatus
	// batches are the Cloud Storage entries not yet written, by sink and
	// bucket.
	batches map[string]*storageBatch
	// reported is when each sink error group was last written, for once a
	// day.
	reported map[string]time.Time
	flushMu  sync.Mutex // one flush at a time
	bqMu     sync.Mutex // one schema change at a time
	linkMu   sync.Mutex // linked datasets' rows in write order (analytics.go)
	wg       sync.WaitGroup
	offset   atomic.Int64 // how far the clock was advanced
}

type storageBatch struct {
	sink    string
	parent  parent
	bucket  string
	entries []*loggingpb.LogEntry
}

// UseDestinations sets where sinks deliver beyond log buckets.
func (s *Server) UseDestinations(d Destinations) {
	s.d.mu.Lock()
	s.d.dest = d
	s.d.mu.Unlock()
}

// clock is the Logging clock: now, plus how far ClockPath advanced it.
func (s *Server) clock() time.Time { return s.now().Add(time.Duration(s.d.offset.Load())) }

// Advance moves the clock forward by d and writes the Cloud Storage
// batches due by then.
func (s *Server) Advance(ctx context.Context, d time.Duration) time.Time {
	if d > 0 {
		s.d.offset.Add(int64(d))
	}
	s.flushDue(ctx, s.clock())
	// A linked dataset's views measure retention by this clock (#1602).
	if d > 0 && s.admin != nil {
		s.admin.EnsureLinkedDatasets(context.WithoutCancel(ctx))
	}
	return s.clock()
}

// ServeClock serves ClockPath.
func (s *Server) ServeClock(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(code int, msg string) {
		reply(code, map[string]any{"error": map[string]any{"code": code, "message": msg}})
	}
	if r.Method != http.MethodPost {
		fail(http.StatusMethodNotAllowed, "clock:advance takes POST")
		return
	}
	var body struct {
		Seconds *float64 `json:"seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Seconds == nil || *body.Seconds < 0 {
		fail(http.StatusBadRequest, `the body must be {"seconds": n}, n not negative: the clock only moves forward`)
		return
	}
	now := s.Advance(r.Context(), time.Duration(*body.Seconds*float64(time.Second)))
	reply(http.StatusOK, map[string]string{"time": now.UTC().Format(time.RFC3339Nano)})
}

// RunDelivery writes due Cloud Storage batches every flushInterval until
// ctx ends.
func (s *Server) RunDelivery(ctx context.Context) {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.flushDue(ctx, s.clock())
		}
	}
}

// SinkStatus is what the named sink has delivered, or nil before anything.
func (s *Server) SinkStatus(sink string) *SinkStatus {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	st := s.d.status[sink]
	if st == nil {
		return nil
	}
	out := *st
	return &out
}

// waitDelivery waits for the deliveries started so far, for tests.
func (s *Server) waitDelivery() { s.d.wg.Wait() }

func (d *deliverer) statusOf(sink string) *SinkStatus {
	if d.status == nil {
		d.status = map[string]*SinkStatus{}
	}
	st := d.status[sink]
	if st == nil {
		st = &SinkStatus{}
		d.status[sink] = st
	}
	return st
}

func (s *Server) delivered(sink string, n int) {
	s.d.mu.Lock()
	st := s.d.statusOf(sink)
	st.Delivered += int64(n)
	st.LastDelivery = s.clock()
	s.d.mu.Unlock()
}

// deliver sends what WriteLogEntries routed beyond log buckets, in the
// background, as Google's routing is.
func (s *Server) deliver(ds []delivery) {
	if len(ds) == 0 {
		return
	}
	// Cloud Storage entries are batched now, so a clock advance right after
	// the write sees them.
	var rest []delivery
	s.d.mu.Lock()
	for _, x := range ds {
		bucket, ok := strings.CutPrefix(x.dest, "storage.googleapis.com/")
		if !ok {
			rest = append(rest, x)
			continue
		}
		if s.d.batches == nil {
			s.d.batches = map[string]*storageBatch{}
		}
		key := x.sink + "|" + bucket
		b := s.d.batches[key]
		if b == nil {
			b = &storageBatch{sink: x.sink, parent: x.parent, bucket: bucket}
			s.d.batches[key] = b
		}
		b.entries = append(b.entries, x.e)
		s.d.statusOf(x.sink).Pending++
	}
	s.d.mu.Unlock()
	if len(rest) == 0 {
		return
	}
	s.d.wg.Add(1)
	go func() {
		defer s.d.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.deliverNow(ctx, rest)
	}()
}

// group is one sink's entries for one destination.
type group struct {
	sink, dest  string
	parent      parent
	partitioned bool
	entries     []*loggingpb.LogEntry
}

func groupBy(ds []delivery) []*group {
	var out []*group
	idx := map[string]*group{}
	for _, x := range ds {
		k := x.sink + "|" + x.dest
		g := idx[k]
		if g == nil {
			g = &group{sink: x.sink, dest: x.dest, parent: x.parent, partitioned: x.partitioned}
			idx[k] = g
			out = append(out, g)
		}
		g.entries = append(g.entries, x.e)
	}
	return out
}

func (s *Server) deliverNow(ctx context.Context, ds []delivery) {
	s.d.mu.Lock()
	dest := s.d.dest
	s.d.mu.Unlock()
	for _, g := range groupBy(ds) {
		switch {
		case strings.HasPrefix(g.dest, "pubsub.googleapis.com/"):
			topic := strings.TrimPrefix(g.dest, "pubsub.googleapis.com/")
			if dest.PubSub == nil {
				s.sinkError(g, "topic_not_found", "Pub/Sub is not served on this instance")
				continue
			}
			msgs := make([]Message, 0, len(g.entries))
			for _, e := range g.entries {
				data, err := protojson.Marshal(e)
				if err != nil {
					continue
				}
				msgs = append(msgs, Message{Data: data, Attributes: map[string]string{
					"logging.googleapis.com/timestamp": e.GetTimestamp().AsTime().UTC().Format(time.RFC3339Nano)}})
			}
			if err := dest.PubSub.Publish(ctx, topic, msgs); err != nil {
				s.sinkError(g, errorCode(err, "topic_not_found"), err.Error())
				continue
			}
			s.delivered(g.sink, len(msgs))
		case strings.HasPrefix(g.dest, "bigquery.googleapis.com/"):
			parts := strings.Split(g.dest, "/") // bigquery.googleapis.com/projects/P/datasets/D
			if dest.BigQuery == nil {
				s.sinkError(g, "dataset_not_found", "BigQuery is not served on this instance")
				continue
			}
			s.d.bqMu.Lock()
			n, rejected, err := streamToBigQuery(ctx, dest.BigQuery, g.sink, parts[2], parts[4], g.partitioned, g.entries)
			s.d.bqMu.Unlock()
			code, detail := "", ""
			if err != nil {
				code, detail = errorCode(err, "dataset_not_found"), err.Error()
			}
			s.recordDelivery(g, n, rejected, code, detail)
		}
	}
}

// errorCode is notFound for a missing destination, else a write failure.
func errorCode(err error, notFound string) string {
	if errors.Is(err, ErrDestinationNotFound) {
		return notFound
	}
	if errors.Is(err, errSchemaMismatch) {
		return "schema_mismatch"
	}
	return "write_failed"
}

// flushDue writes every Cloud Storage batch's entries received in an hour
// that has ended by now.
func (s *Server) flushDue(ctx context.Context, now time.Time) {
	s.d.flushMu.Lock()
	defer s.d.flushMu.Unlock()
	type due struct {
		b       storageBatch
		entries []*loggingpb.LogEntry
	}
	var work []due
	s.d.mu.Lock()
	storage := s.d.dest.Storage
	keys := make([]string, 0, len(s.d.batches))
	for k := range s.d.batches {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := s.d.batches[k]
		var ready, keep []*loggingpb.LogEntry
		for _, e := range b.entries {
			if !e.GetReceiveTimestamp().AsTime().Truncate(time.Hour).Add(time.Hour).After(now) {
				ready = append(ready, e)
			} else {
				keep = append(keep, e)
			}
		}
		if len(ready) == 0 {
			continue
		}
		b.entries = keep
		s.d.statusOf(b.sink).Pending -= len(ready)
		work = append(work, due{b: *b, entries: ready})
		if len(keep) == 0 {
			delete(s.d.batches, k)
		}
	}
	s.d.mu.Unlock()
	for _, w := range work {
		g := &group{sink: w.b.sink, parent: w.b.parent, dest: "storage.googleapis.com/" + w.b.bucket, entries: w.entries}
		if storage == nil {
			s.sinkError(g, "bucket_not_found", "Cloud Storage is not served on this instance")
			continue
		}
		files, _ := copyFiles(w.entries, now)
		written := 0
		for _, f := range files {
			body, err := f.body()
			if err == nil {
				err = storage.WriteObject(ctx, w.b.bucket, f.name, "application/json", body)
			}
			if err != nil {
				s.sinkError(g, errorCode(err, "bucket_not_found"), err.Error())
				continue
			}
			written += len(f.entries)
		}
		if written > 0 {
			s.delivered(w.b.sink, written)
		}
	}
}

// sinkErrorLog is the log Google writes sink errors to.
const sinkErrorLog = "logging.googleapis.com/sink_error"

// sinkError records a failed delivery on the sink and, once a day per sink
// and code, as the entry Google documents
// (docs.cloud.google.com/logging/docs/export/troubleshoot).
func (s *Server) sinkError(g *group, code, detail string) {
	s.recordDelivery(g, 0, 0, code, detail)
}

// recordDelivery records what one delivery did, in one step so the console
// never reads part of it (#1529): n entries written, rejected entries sent
// to the dataset's export_errors table, and the error, code empty for
// none, reported as sinkError says.
func (s *Server) recordDelivery(g *group, n, rejected int, code, detail string) {
	if n == 0 && rejected == 0 && code == "" {
		return
	}
	now := s.clock()
	s.d.mu.Lock()
	st := s.d.statusOf(g.sink)
	if n > 0 {
		st.Delivered += int64(n)
		st.LastDelivery = now
	}
	st.ErrorRows += int64(rejected)
	if code == "" {
		s.d.mu.Unlock()
		return
	}
	st.LastError, st.LastErrorAt = code+": "+detail, now
	key := g.sink + "|" + code
	last, seen := s.d.reported[key]
	if seen && now.Sub(last) < 24*time.Hour {
		s.d.mu.Unlock()
		return
	}
	if s.d.reported == nil {
		s.d.reported = map[string]time.Time{}
	}
	s.d.reported[key] = now
	s.d.mu.Unlock()
	id := g.sink[strings.LastIndex(g.sink, "/")+1:]
	labels := map[string]string{"name": id, "destination": g.dest}
	if g.parent.isProject() {
		labels["project_id"] = g.parent.id
	}
	e := &loggingpb.LogEntry{
		LogName:  g.parent.String() + "/logs/" + url.PathEscape(sinkErrorLog),
		Resource: &mrpb.MonitoredResource{Type: "logging_sink", Labels: labels},
		Severity: ltype.LogSeverity_ERROR,
		Labels: map[string]string{"activity_type_name": "LoggingSinkConfigErrorV2", "destination": g.dest,
			"error_code": code, "error_detail": detail, "sink_id": id},
		Payload: &loggingpb.LogEntry_TextPayload{TextPayload: fmt.Sprintf("Cloud Logging sink configuration error in %s, sink %s: %s (%s)",
			g.parent.id, id, code, detail)},
		Timestamp: timestamppb.New(now), ReceiveTimestamp: timestamppb.New(now),
	}
	s.store.mu.Lock()
	s.store.seq++
	e.InsertId = fmt.Sprintf("cb%x", s.store.seq)
	s.store.mu.Unlock()
	s.store.append([]*loggingpb.LogEntry{e}, nil)
}
