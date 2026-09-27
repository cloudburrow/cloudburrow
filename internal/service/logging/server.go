// Package logging implements the Cloud Logging write and read API
// (google.logging.v2.LoggingServiceV2) for local development (#304).
//
// Google publishes no Logging emulator. This keeps entries in a bounded
// in-memory store: WriteLogEntries, ListLogEntries with a documented filter
// subset (see filter.go), ListLogs and DeleteLog. Sinks, exclusions, buckets,
// log-based metrics and TailLogEntries are not served and answer
// UNIMPLEMENTED. Entries do not survive a restart, in any mode.
package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// DefaultLimit is how many entries the store keeps before the oldest go.
const DefaultLimit = 20000

// Store is a bounded, ordered log store.
type Store struct {
	mu      sync.Mutex
	limit   int
	entries []*loggingpb.LogEntry
	seq     uint64
	// observe is told about every written entry, for the console.
	observe func(*loggingpb.LogEntry)
}

// NewStore returns a store keeping at most limit entries.
func NewStore(limit int) *Store {
	if limit <= 0 {
		limit = DefaultLimit
	}
	return &Store{limit: limit}
}

// Observe sets who is told about each written entry.
func (s *Store) Observe(fn func(*loggingpb.LogEntry)) { s.mu.Lock(); s.observe = fn; s.mu.Unlock() }

// Reset removes every entry.
func (s *Store) Reset() { s.mu.Lock(); s.entries = nil; s.mu.Unlock() }

func (s *Store) append(es []*loggingpb.LogEntry) {
	s.mu.Lock()
	s.entries = append(s.entries, es...)
	if over := len(s.entries) - s.limit; over > 0 {
		s.entries = append([]*loggingpb.LogEntry(nil), s.entries[over:]...)
	}
	obs := s.observe
	s.mu.Unlock()
	if obs != nil {
		for _, e := range es {
			obs(e)
		}
	}
}

func (s *Store) snapshot() []*loggingpb.LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*loggingpb.LogEntry(nil), s.entries...)
}

// Server serves google.logging.v2.LoggingServiceV2.
type Server struct {
	loggingpb.UnimplementedLoggingServiceV2Server
	store *Store
	now   func() time.Time
}

// NewServer returns the API over a store.
func NewServer(st *Store) *Server { return &Server{store: st, now: time.Now} }

// Register adds the service to a gRPC server, or to the JSON transcoder.
// ConfigServiceV2 and MetricsServiceV2 are not registered, so sinks,
// exclusions, buckets and metrics answer UNIMPLEMENTED.
func (s *Server) Register(g grpc.ServiceRegistrar) { loggingpb.RegisterLoggingServiceV2Server(g, s) }

// Limits Google documents for entries.write
// (https://cloud.google.com/logging/quotas). Documented, not measured
// against Google.
const (
	// MaxEntryBytes is the largest LogEntry accepted: 256 KiB. Google says
	// its limit is approximate and based on internal data sizes; this
	// measures the entry's wire size as sent.
	MaxEntryBytes = 256 << 10
	// MaxRequestBytes is the largest entries.write request: 10 MB.
	MaxRequestBytes = 10_000_000
	// MaxFutureSkew is how far ahead of now a timestamp may be. Google
	// rejects entries more than a day in the future with INVALID_ARGUMENT.
	MaxFutureSkew = 24 * time.Hour
	// Retention is the _Default bucket's retention. Google accepts entries
	// older than a bucket's retention but does not store them, so they are
	// never listed; custom retention is not modelled.
	Retention = 30 * 24 * time.Hour
)

// parseLogName checks a log name, returning its project, as an API error.
func parseLogName(name string) (string, error) {
	bare := strings.TrimPrefix(name, "/")
	for _, p := range []string{"organizations/", "folders/", "billingAccounts/"} {
		if strings.HasPrefix(bare, p) {
			return "", apierror.InvalidArgument("log name %q: only projects/{project}/logs/{log_id} is supported", name)
		}
	}
	project, _, err := resource.ParseLogName(name)
	if err != nil {
		return "", apierror.InvalidArgument("log name: %v", err)
	}
	return project, nil
}

// parseProjectParent checks a "projects/{project}" parent or resource name.
func parseProjectParent(field, name string) (string, error) {
	project, err := resource.ParseProject(name)
	if err != nil {
		return "", apierror.InvalidArgument("%s %q: only projects/{project} is supported: %v", field, name, err)
	}
	return project, nil
}

// prepare validates one entry and fills what the request supplies, the way
// the API documents request-level defaults. keep is false for an entry that
// is accepted but, being older than the retention period, is not stored.
func (s *Server) prepare(req *loggingpb.WriteLogEntriesRequest, in *loggingpb.LogEntry, now time.Time) (e *loggingpb.LogEntry, keep bool, err error) {
	if n := proto.Size(in); n > MaxEntryBytes {
		return nil, false, apierror.InvalidArgument("the entry is %d bytes, over the %d-byte (256 KiB) limit", n, MaxEntryBytes)
	}
	e = proto.Clone(in).(*loggingpb.LogEntry)
	if e.GetLogName() == "" {
		e.LogName = req.GetLogName()
	}
	if e.GetResource() == nil {
		e.Resource = req.GetResource()
	}
	if e.GetResource() == nil {
		return nil, false, apierror.InvalidArgument("a monitored resource is required")
	}
	if len(req.GetLabels()) > 0 {
		labels := map[string]string{}
		for k, v := range req.GetLabels() {
			labels[k] = v
		}
		for k, v := range e.GetLabels() {
			labels[k] = v
		}
		e.Labels = labels
	}
	if _, err := parseLogName(e.GetLogName()); err != nil {
		return nil, false, err
	}
	// Listing never shows the leading slash Google tolerates.
	e.LogName = strings.TrimPrefix(e.GetLogName(), "/")
	if e.GetTimestamp() == nil {
		e.Timestamp = timestamppb.New(now)
	} else if err := e.GetTimestamp().CheckValid(); err != nil {
		return nil, false, apierror.InvalidArgument("timestamp: %v", err)
	}
	ts := e.GetTimestamp().AsTime()
	if ts.After(now.Add(MaxFutureSkew)) {
		return nil, false, apierror.InvalidArgument("timestamp %s is more than 24 hours in the future", ts.Format(time.RFC3339Nano))
	}
	e.ReceiveTimestamp = timestamppb.New(now)
	return e, !ts.Before(now.Add(-Retention)), nil
}

// WriteLogEntries writes a batch.
//
// Without partial_success the batch is all or nothing: the first invalid
// entry fails the call and nothing is written. With it, the valid entries are
// written and the call fails with the first failed entry's status, carrying
// WriteLogEntriesPartialErrors keyed by index; when every entry failed nothing
// is written and no per-entry errors are attached, as the proto documents.
func (s *Server) WriteLogEntries(_ context.Context, req *loggingpb.WriteLogEntriesRequest) (*loggingpb.WriteLogEntriesResponse, error) {
	if len(req.GetEntries()) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("entries is required"))
	}
	if n := proto.Size(req); n > MaxRequestBytes {
		return nil, apierror.Wrap(apierror.InvalidArgument("the request is %d bytes, over the %d-byte (10 MB) limit", n, MaxRequestBytes))
	}
	now := s.now()
	out := make([]*loggingpb.LogEntry, 0, len(req.GetEntries()))
	failed := map[int32]*spb.Status{}
	var first *apierror.Error
	for i, in := range req.GetEntries() {
		e, keep, err := s.prepare(req, in, now)
		if err != nil {
			ae := apierror.From(fmt.Errorf("entries[%d]: %w", i, err))
			if !req.GetPartialSuccess() {
				return nil, ae
			}
			if first == nil {
				first = ae
			}
			failed[int32(i)] = status.New(ae.Code, ae.Message).Proto()
			continue
		}
		if keep {
			out = append(out, e)
		}
	}
	if first != nil && len(failed) == len(req.GetEntries()) {
		return nil, first
	}
	if !req.GetDryRun() && len(out) > 0 {
		s.store.mu.Lock()
		for _, e := range out {
			if e.GetInsertId() == "" {
				s.store.seq++
				e.InsertId = "cb" + strconv.FormatUint(s.store.seq, 36)
			}
		}
		s.store.mu.Unlock()
		s.store.append(out)
	}
	if first != nil {
		st, err := status.New(first.Code, fmt.Sprintf("%s (%d of %d entries not written)", first.Message, len(failed), len(req.GetEntries()))).
			WithDetails(&loggingpb.WriteLogEntriesPartialErrors{LogEntryErrors: failed})
		if err != nil {
			return nil, apierror.Internal(err, "attaching per-entry errors")
		}
		return nil, st.Err()
	}
	return &loggingpb.WriteLogEntriesResponse{}, nil
}

// cursor is where a ListLogEntries page ended: the last entry's position in
// the (timestamp, logName, insertId) order. Resuming strictly after a
// position rather than at an offset means entries written or evicted between
// pages cannot shift the walk into a duplicate or a skip.
type cursor struct {
	Seconds int64  `json:"t"`
	Nanos   int32  `json:"n"`
	Log     string `json:"l"`
	Insert  string `json:"i"`
}

func cursorOf(e *loggingpb.LogEntry) cursor {
	return cursor{Seconds: e.GetTimestamp().GetSeconds(), Nanos: e.GetTimestamp().GetNanos(), Log: e.GetLogName(), Insert: e.GetInsertId()}
}

// compare orders cursors by timestamp, then log name, then insert ID.
func (a cursor) compare(b cursor) int {
	switch {
	case a.Seconds != b.Seconds:
		return cmpInt(a.Seconds, b.Seconds)
	case a.Nanos != b.Nanos:
		return cmpInt(int64(a.Nanos), int64(b.Nanos))
	case a.Log != b.Log:
		return strings.Compare(a.Log, b.Log)
	default:
		return strings.Compare(a.Insert, b.Insert)
	}
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ListLogEntries lists entries in timestamp order.
//
// The page token is a paging token scoped to the resource names, filter and
// order, so a token from one listing is INVALID_ARGUMENT on another.
func (s *Server) ListLogEntries(_ context.Context, req *loggingpb.ListLogEntriesRequest) (*loggingpb.ListLogEntriesResponse, error) {
	if len(req.GetResourceNames()) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("resource_names is required"))
	}
	projects := map[string]bool{}
	for _, r := range req.GetResourceNames() {
		p, err := parseProjectParent("resource name", r)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		projects[p] = true
	}
	match, err := compile(req.GetFilter())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	var desc bool
	order := strings.ToLower(strings.TrimSpace(req.GetOrderBy()))
	switch order {
	case "", "timestamp asc":
		order = "timestamp asc"
	case "timestamp desc":
		desc = true
	default:
		return nil, apierror.Wrap(apierror.InvalidArgument(`order_by must be "timestamp asc" or "timestamp desc"`))
	}
	names := append([]string(nil), req.GetResourceNames()...)
	sort.Strings(names)
	scope := "logging|" + strings.Join(names, ",") + "|filter=" + req.GetFilter() + "|order=" + order
	after, err := paging.DecodeToken(scope, req.GetPageToken())
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("page_token: %v", err))
	}
	var from *cursor
	if after != "" {
		var c cursor
		if err := json.Unmarshal([]byte(after), &c); err != nil {
			return nil, apierror.Wrap(apierror.InvalidArgument("page_token: %v", paging.ErrInvalidToken))
		}
		from = &c
	}

	var found []*loggingpb.LogEntry
	for _, e := range s.store.snapshot() {
		p, _, err := resource.ParseLogName(e.GetLogName())
		if err == nil && projects[p] && match(e) {
			found = append(found, e)
		}
	}
	less := func(a, b cursor) bool {
		if desc {
			return a.compare(b) > 0
		}
		return a.compare(b) < 0
	}
	sort.SliceStable(found, func(i, j int) bool { return less(cursorOf(found[i]), cursorOf(found[j])) })
	start := 0
	if from != nil {
		start = sort.Search(len(found), func(i int) bool { return less(*from, cursorOf(found[i])) })
	}
	size := int(req.GetPageSize())
	if size <= 0 || size > 1000 {
		size = 1000
	}
	end := start + size
	if end > len(found) {
		end = len(found)
	}
	resp := &loggingpb.ListLogEntriesResponse{Entries: found[start:end]}
	if end < len(found) {
		b, err := json.Marshal(cursorOf(found[end-1]))
		if err != nil {
			return nil, apierror.Internal(err, "encoding the page token")
		}
		resp.NextPageToken = paging.EncodeToken(scope, string(b))
	}
	return resp, nil
}

func (s *Server) ListLogs(_ context.Context, req *loggingpb.ListLogsRequest) (*loggingpb.ListLogsResponse, error) {
	project, err := parseProjectParent("parent", req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	seen := map[string]bool{}
	var names []string
	for _, e := range s.store.snapshot() {
		if p, _, err := resource.ParseLogName(e.GetLogName()); err == nil && p == project && !seen[e.GetLogName()] {
			seen[e.GetLogName()] = true
			names = append(names, e.GetLogName())
		}
	}
	sort.Strings(names)
	return &loggingpb.ListLogsResponse{LogNames: names}, nil
}

// DeleteLog removes every entry of one log.
func (s *Server) DeleteLog(_ context.Context, req *loggingpb.DeleteLogRequest) (*emptypb.Empty, error) {
	if _, err := parseLogName(req.GetLogName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	logName := strings.TrimPrefix(req.GetLogName(), "/")
	s.store.mu.Lock()
	kept := s.store.entries[:0]
	removed := false
	for _, e := range s.store.entries {
		if e.GetLogName() == logName {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	s.store.entries = kept
	s.store.mu.Unlock()
	if !removed {
		return nil, apierror.Wrap(apierror.NotFound("log %s not found", req.GetLogName()))
	}
	return &emptypb.Empty{}, nil
}
