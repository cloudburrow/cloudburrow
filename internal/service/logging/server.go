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
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
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

// Register adds the service to a gRPC server. ConfigServiceV2 and
// MetricsServiceV2 are not registered, so sinks, exclusions, buckets and
// metrics answer UNIMPLEMENTED.
func (s *Server) Register(g *grpc.Server) { loggingpb.RegisterLoggingServiceV2Server(g, s) }

// parentOf is the "projects/{p}" (or organizations/..., folders/...,
// billingAccounts/...) a log name belongs to.
func parentOf(logName string) (string, string, error) {
	i := strings.Index(logName, "/logs/")
	if i <= 0 || i+len("/logs/") >= len(logName) {
		return "", "", apierror.InvalidArgument("log name %q must be projects/{project}/logs/{log_id}", logName)
	}
	parent := logName[:i]
	if !strings.HasPrefix(parent, "projects/") || strings.Count(parent, "/") != 1 {
		return "", "", apierror.InvalidArgument("log name %q: only projects/{project}/logs/{log_id} is supported", logName)
	}
	return parent, logName[i+len("/logs/"):], nil
}

func (s *Server) WriteLogEntries(_ context.Context, req *loggingpb.WriteLogEntriesRequest) (*loggingpb.WriteLogEntriesResponse, error) {
	if len(req.GetEntries()) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("entries is required"))
	}
	now := s.now()
	out := make([]*loggingpb.LogEntry, 0, len(req.GetEntries()))
	for i, in := range req.GetEntries() {
		e := proto.Clone(in).(*loggingpb.LogEntry)
		// Request-level defaults fill what an entry leaves out, as the API
		// documents.
		if e.GetLogName() == "" {
			e.LogName = req.GetLogName()
		}
		if e.GetResource() == nil {
			e.Resource = req.GetResource()
		}
		if e.GetResource() == nil {
			return nil, apierror.Wrap(apierror.InvalidArgument("entries[%d]: a monitored resource is required", i))
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
		if _, _, err := parentOf(e.GetLogName()); err != nil {
			return nil, apierror.Wrap(fmt.Errorf("entries[%d]: %w", i, err))
		}
		if e.GetTimestamp() == nil {
			e.Timestamp = timestamppb.New(now)
		}
		e.ReceiveTimestamp = timestamppb.New(now)
		out = append(out, e)
	}
	if req.GetDryRun() {
		return &loggingpb.WriteLogEntriesResponse{}, nil
	}
	s.store.mu.Lock()
	for _, e := range out {
		if e.GetInsertId() == "" {
			s.store.seq++
			e.InsertId = "cb" + strconv.FormatUint(s.store.seq, 36)
		}
	}
	s.store.mu.Unlock()
	s.store.append(out)
	return &loggingpb.WriteLogEntriesResponse{}, nil
}

func (s *Server) ListLogEntries(_ context.Context, req *loggingpb.ListLogEntriesRequest) (*loggingpb.ListLogEntriesResponse, error) {
	if len(req.GetResourceNames()) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("resource_names is required"))
	}
	parents := map[string]bool{}
	for _, r := range req.GetResourceNames() {
		if !strings.HasPrefix(r, "projects/") || strings.Count(r, "/") != 1 {
			return nil, apierror.Wrap(apierror.InvalidArgument(
				"resource name %q: only projects/{project} is supported", r))
		}
		parents[r] = true
	}
	match, err := compile(req.GetFilter())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	var found []*loggingpb.LogEntry
	for _, e := range s.store.snapshot() {
		parent, _, _ := parentOf(e.GetLogName())
		if parents[parent] && match(e) {
			found = append(found, e)
		}
	}
	switch strings.ToLower(strings.TrimSpace(req.GetOrderBy())) {
	case "", "timestamp asc":
		sort.SliceStable(found, func(i, j int) bool {
			return found[i].GetTimestamp().AsTime().Before(found[j].GetTimestamp().AsTime())
		})
	case "timestamp desc":
		sort.SliceStable(found, func(i, j int) bool {
			return found[i].GetTimestamp().AsTime().After(found[j].GetTimestamp().AsTime())
		})
	default:
		return nil, apierror.Wrap(apierror.InvalidArgument(`order_by must be "timestamp asc" or "timestamp desc"`))
	}
	start := 0
	if tok := req.GetPageToken(); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 0 || n > len(found) {
			return nil, apierror.Wrap(apierror.InvalidArgument("page_token is not one this server issued"))
		}
		start = n
	}
	size := int(req.GetPageSize())
	if size <= 0 || size > 1000 {
		size = 1000
	}
	end := start + size
	resp := &loggingpb.ListLogEntriesResponse{}
	if end < len(found) {
		resp.NextPageToken = strconv.Itoa(end)
	} else {
		end = len(found)
	}
	resp.Entries = found[start:end]
	return resp, nil
}

func (s *Server) ListLogs(_ context.Context, req *loggingpb.ListLogsRequest) (*loggingpb.ListLogsResponse, error) {
	parent := req.GetParent()
	if !strings.HasPrefix(parent, "projects/") || strings.Count(parent, "/") != 1 {
		return nil, apierror.Wrap(apierror.InvalidArgument("parent %q: only projects/{project} is supported", parent))
	}
	seen := map[string]bool{}
	var names []string
	for _, e := range s.store.snapshot() {
		if p, _, _ := parentOf(e.GetLogName()); p == parent && !seen[e.GetLogName()] {
			seen[e.GetLogName()] = true
			names = append(names, e.GetLogName())
		}
	}
	sort.Strings(names)
	return &loggingpb.ListLogsResponse{LogNames: names}, nil
}

// DeleteLog removes every entry of one log.
func (s *Server) DeleteLog(_ context.Context, req *loggingpb.DeleteLogRequest) (*emptypb.Empty, error) {
	if _, _, err := parentOf(req.GetLogName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.store.mu.Lock()
	kept := s.store.entries[:0]
	removed := false
	for _, e := range s.store.entries {
		if e.GetLogName() == req.GetLogName() {
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
