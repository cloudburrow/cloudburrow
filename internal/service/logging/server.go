// Package logging implements the Cloud Logging write and read API
// (google.logging.v2.LoggingServiceV2) for local development (#304).
//
// Google publishes no Logging emulator. This keeps entries in a bounded
// in-memory store: WriteLogEntries, ListLogEntries with a documented filter
// subset (see filter.go), ListLogs, DeleteLog, TailLogEntries (tail.go) and
// ListMonitoredResourceDescriptors (descriptors.go). Log-based metrics, sinks,
// exclusions, buckets, views, links and settings are stored by Admin
// (admin.go, buckets.go, links.go). Entries may belong to any of the four
// parents (parent.go, #1395). Entries do not survive a restart, in any mode.
package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
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
	// routed is where sinks sent an entry beyond its own parent's _Default
	// bucket (#1395), for the entries that went anywhere else.
	routed map[*loggingpb.LogEntry]*route
	seq    uint64
	// observe is told about every written entry, for the console.
	observe func(*loggingpb.LogEntry)
	// tails are the open TailLogEntries streams, each told about every
	// written entry (#1215).
	tails  map[uint64]func(*loggingpb.LogEntry)
	tailID uint64
}

// subscribe adds fn to the writers' notifications and returns its removal.
func (s *Store) subscribe(fn func(*loggingpb.LogEntry)) (cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tails == nil {
		s.tails = map[uint64]func(*loggingpb.LogEntry){}
	}
	s.tailID++
	id := s.tailID
	s.tails[id] = fn
	return func() { s.mu.Lock(); delete(s.tails, id); s.mu.Unlock() }
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

// route is where one entry went: the log buckets sinks sent it to, and
// whether an exclusion kept it out of its own parent's _Default bucket.
type route struct {
	buckets []string // projects/{p}/locations/{l}/buckets/{b}
	hidden  bool
}

// Reset removes every entry.
func (s *Store) Reset() { s.mu.Lock(); s.entries, s.routed = nil, nil; s.mu.Unlock() }

func (s *Store) append(es []*loggingpb.LogEntry, routes map[*loggingpb.LogEntry]*route) {
	s.mu.Lock()
	s.entries = append(s.entries, es...)
	if len(routes) > 0 && s.routed == nil {
		s.routed = map[*loggingpb.LogEntry]*route{}
	}
	for e, r := range routes {
		s.routed[e] = r
	}
	if over := len(s.entries) - s.limit; over > 0 {
		for _, e := range s.entries[:over] {
			delete(s.routed, e)
		}
		s.entries = append([]*loggingpb.LogEntry(nil), s.entries[over:]...)
	}
	obs := s.observe
	tails := make([]func(*loggingpb.LogEntry), 0, len(s.tails))
	for _, fn := range s.tails {
		tails = append(tails, fn)
	}
	s.mu.Unlock()
	for _, e := range es {
		if obs != nil {
			obs(e)
		}
		for _, fn := range tails {
			fn(e)
		}
	}
}

func (s *Store) snapshot() []*loggingpb.LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*loggingpb.LogEntry, 0, len(s.entries))
	for _, e := range s.entries {
		if r := s.routed[e]; r == nil || !r.hidden {
			out = append(out, e)
		}
	}
	return out
}

// routedTo snapshots the entries sinks sent to bucket.
func (s *Store) routedTo(bucket string) []*loggingpb.LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*loggingpb.LogEntry
	for _, e := range s.entries {
		if r := s.routed[e]; r != nil {
			for _, b := range r.buckets {
				if b == bucket {
					out = append(out, e)
					break
				}
			}
		}
	}
	return out
}

// Server serves google.logging.v2.LoggingServiceV2.
type Server struct {
	loggingpb.UnimplementedLoggingServiceV2Server
	store *Store
	now   func() time.Time
	// admin holds the project exclusions WriteLogEntries applies (#1226).
	admin *Admin
}

// UseAdmin makes the project exclusions admin stores apply to writes: an
// entry an enabled exclusion matches is accepted and not stored, as Google's
// _Default sink drops it.
// It also gives CopyLogEntries (copy.go) the entries it copies.
func (s *Server) UseAdmin(a *Admin) { s.admin = a; a.useEntries(s.store.snapshot) }

// CountMatching counts project's stored entries that filter matches with a
// timestamp at or after since, for a log-based metric's console page. The
// filter is compiled as ListLogEntries compiles it.
func (s *Server) CountMatching(project, filter string, since time.Time) (int, error) {
	match, err := compile(filter)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range s.store.snapshot() {
		p, _, err := resource.ParseLogName(e.GetLogName())
		if err == nil && p == project && !e.GetTimestamp().AsTime().Before(since) && match(e) {
			n++
		}
	}
	return n, nil
}

// NewServer returns the API over a store.
func NewServer(st *Store) *Server { return &Server{store: st, now: time.Now} }

// Register adds the service to a gRPC server, or to the JSON transcoder.
// MetricsServiceV2 and ConfigServiceV2 are Admin's (admin.go).
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

// logIDRe is a decoded log ID's documented alphabet and length.
var logIDRe = regexp.MustCompile(`^[A-Za-z0-9/_.-]{1,511}$`)

// parseLogName checks a log name, {parent}/logs/{log_id} with the log ID
// URL-encoded, returning its parent, as an API error. The parent's existence
// is not checked.
func parseLogName(name string) (parent, error) {
	p, rest, err := cutParent("log name", name)
	if err != nil {
		return parent{}, err
	}
	raw, ok := strings.CutPrefix(rest, "logs/")
	if !ok || raw == "" || strings.Contains(raw, "/") {
		return parent{}, apierror.InvalidArgument("log name %q is not {parent}/logs/{log_id} with a URL-encoded log_id", name)
	}
	if id, uerr := url.PathUnescape(raw); uerr != nil || !logIDRe.MatchString(id) {
		return parent{}, apierror.InvalidArgument("log name %q: the log ID must be fewer than 512 characters of letters, digits, '/', '_', '-' and '.'", name)
	}
	return p, nil
}

// entryParent is the parent of a stored entry's log.
func entryParent(e *loggingpb.LogEntry) parent {
	p, _ := parseLogName(e.GetLogName())
	return p
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
	p, err := parseLogName(e.GetLogName())
	if err != nil {
		return nil, false, err
	}
	if err := s.admin.check(p); err != nil {
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
	exclusions := map[parent]pred{}
	sinks := map[parent][]sinkRoute{}
	routes := map[*loggingpb.LogEntry]*route{}
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
		if !keep {
			continue
		}
		hidden := s.excluded(e, exclusions)
		buckets := s.routeOf(e, sinks)
		if hidden && len(buckets) == 0 {
			continue
		}
		if hidden || len(buckets) > 0 {
			routes[e] = &route{buckets: buckets, hidden: hidden}
		}
		out = append(out, e)
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
		s.store.append(out, routes)
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

// excluded reports whether an enabled exclusion of the entry's own parent
// matches it. Exclusions keep an entry out of that parent's _Default bucket
// only, as Google documents: sinks of the folders and organization above it
// still see it. Each parent's exclusions are read once per request.
func (s *Server) excluded(e *loggingpb.LogEntry, cache map[parent]pred) bool {
	if s.admin == nil {
		return false
	}
	project := entryParent(e)
	p, ok := cache[project]
	if !ok {
		p = s.admin.excluder(project)
		cache[project] = p
	}
	return p != nil && p(e)
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
	if len(req.GetResourceNames()) > 100 {
		return nil, apierror.Wrap(apierror.InvalidArgument("resource_names: at most 100 resources, not %d", len(req.GetResourceNames())))
	}
	sources, err := s.sources(req.GetResourceNames())
	if err != nil {
		return nil, apierror.Wrap(err)
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
	seen := map[*loggingpb.LogEntry]bool{}
	add := func(e *loggingpb.LogEntry) {
		if !seen[e] && match(e) {
			seen[e] = true
			found = append(found, e)
		}
	}
	for _, e := range s.store.snapshot() {
		if sources.parents[entryParent(e)] {
			add(e)
		}
	}
	for _, b := range sources.buckets {
		for _, e := range s.store.routedTo(b) {
			add(e)
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
	project, err := s.admin.resolve("parent", req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	seen := map[string]bool{}
	var names []string
	for _, e := range s.store.snapshot() {
		if entryParent(e) == project && !seen[e.GetLogName()] {
			seen[e.GetLogName()] = true
			names = append(names, e.GetLogName())
		}
	}
	sort.Strings(names)
	return &loggingpb.ListLogsResponse{LogNames: names}, nil
}

// DeleteLog removes every entry of one log.
func (s *Server) DeleteLog(_ context.Context, req *loggingpb.DeleteLogRequest) (*emptypb.Empty, error) {
	p, err := parseLogName(req.GetLogName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.admin.check(p); err != nil {
		return nil, apierror.Wrap(err)
	}
	logName := strings.TrimPrefix(req.GetLogName(), "/")
	s.store.mu.Lock()
	kept := s.store.entries[:0]
	removed := false
	for _, e := range s.store.entries {
		if e.GetLogName() == logName {
			removed = true
			delete(s.store.routed, e)
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

// sources is what a ListLogEntries reads: the parents whose own entries it
// lists, and the log buckets whose routed entries it lists too.
type sources struct {
	parents map[parent]bool
	buckets []string
}

// sources resolves resource_names: parents, and bucket views. A parent's own
// entries are those of its _Default bucket, so its _Default view is the
// parent and what sinks routed there; another bucket's view is what sinks
// routed to that bucket. A view's filter is not applied.
func (s *Server) sources(names []string) (sources, error) {
	out := sources{parents: map[parent]bool{}}
	for _, r := range names {
		p, rest, err := cutParent("resource name", r)
		if err != nil {
			return out, err
		}
		if err := s.admin.check(p); err != nil {
			return out, err
		}
		if rest == "" {
			out.parents[p] = true
			continue
		}
		ref, view, err := parseView("resource name", r)
		if err != nil {
			return out, apierror.InvalidArgument("resource name %q is neither a parent nor {parent}/locations/{location}/buckets/{bucket}/views/{view}", r)
		}
		if s.admin != nil {
			s.admin.mu.Lock()
			_, err := s.admin.getBucket(ref)
			if err == nil {
				_, err = s.admin.getView(ref, view)
			}
			s.admin.mu.Unlock()
			if err != nil {
				return out, err
			}
		}
		if ref.location == "global" && ref.bucket == bucketDefault {
			out.parents[ref.parent] = true
		}
		out.buckets = append(out.buckets, ref.name())
	}
	return out, nil
}

// sinkRoute is one sink that can send an entry to a log bucket.
type sinkRoute struct {
	match, excluded pred
	bucket          string
}

// routeOf is the log buckets the sinks of the entry's own parent, and the
// include_children sinks of the folders and organization above it, send it
// to. Only a destination that is a log bucket is delivered to; Cloud
// Storage, BigQuery, Pub/Sub and project destinations receive nothing.
func (s *Server) routeOf(e *loggingpb.LogEntry, cache map[parent][]sinkRoute) []string {
	if s.admin == nil {
		return nil
	}
	own := entryParent(e)
	var routes []sinkRoute
	for i, p := range append([]parent{own}, s.admin.ancestors(own)...) {
		key := p
		if i > 0 {
			key.id += "/children" // an ancestor's routes: include_children sinks only
		}
		rs, ok := cache[key]
		if !ok {
			rs = s.admin.sinkRoutes(p, i > 0)
			cache[key] = rs
		}
		routes = append(routes, rs...)
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range routes {
		if seen[r.bucket] || !r.match(e) || (r.excluded != nil && r.excluded(e)) {
			continue
		}
		seen[r.bucket] = true
		out = append(out, r.bucket)
	}
	return out
}
