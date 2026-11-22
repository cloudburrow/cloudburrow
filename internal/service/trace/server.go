// Package trace serves Cloud Trace (cloudtrace.googleapis.com, #2118) in
// the CLI process, gRPC and JSON on one port as Google serves both:
//
//   - the Cloud Trace API v2 (google.devtools.cloudtrace.v2.TraceService):
//     BatchWriteSpans and CreateSpan, with the span rules and limits the
//     reference documents (v2.go);
//   - the Cloud Trace API v1 (google.devtools.cloudtrace.v1.TraceService):
//     ListTraces with its filter language, views, interval and ordering,
//     GetTrace, and PatchTraces (v1.go, filter.go);
//   - the Telemetry API's OTLP trace ingestion (telemetry.googleapis.com,
//     otlp.go): opentelemetry.proto.collector.trace.v1.TraceService/Export
//     over gRPC and POST /v1/traces over HTTP, protobuf or JSON.
//
// Every write lands in one store, read back by every read: a span written
// through OTLP is listed by v1 ListTraces, and one written by v1
// PatchTraces is a v2 span. Spans are kept for 30 days ("Span data is
// stored for 30 days", docs.cloud.google.com/trace/docs/finding-traces,
// read 2026-11-21), in the instance's directory in persistent mode.
//
// docs/trace.md is the user's guide.
package trace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	cloudtracepb "cloud.google.com/go/trace/apiv1/tracepb"
	cloudtracev2pb "cloud.google.com/go/trace/apiv2/tracepb"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Retention is how long a span is kept, as Google documents.
const Retention = 30 * 24 * time.Hour

// Span is one stored span: the v2 resource, and what CloudBurrow knows of
// where it came from.
type Span struct {
	// Project is the project the span was written to.
	Project string
	// TraceID and SpanID are lowercase hex, 32 and 16 characters.
	TraceID string
	SpanID  string
	// V2 is the span, as v2 reads it back (name, attributes, events, ...).
	V2 *cloudtracev2pb.Span
	// Received is when the span was written, for retention.
	Received time.Time
}

// Start and End are the span's times.
func (s *Span) Start() time.Time { return s.V2.GetStartTime().AsTime() }

// End is the span's end time.
func (s *Span) End() time.Time { return s.V2.GetEndTime().AsTime() }

// Name is the span's display name.
func (s *Span) Name() string { return s.V2.GetDisplayName().GetValue() }

// Server is Cloud Trace.
type Server struct {
	v1 v1Server
	v2 v2Server
	ot otlpServer

	state string // the file; "" keeps everything in memory
	now   func() time.Time
	// defaultProject is the project an OTLP request that names none is
	// written to.
	defaultProject string

	// onWrite is told of every span written, after it is stored: the
	// Observability API's _Trace bucket and its linked datasets (#2117).
	onWrite func([]*Span)
	// router places OTLP spans whose resource names no gcp.project_id: the
	// project, "" to keep the one chosen, or keep false to drop the span.
	router func(attrs map[string]string) (project string, keep bool)

	mu sync.Mutex
	// traces by project, then trace ID; each trace's spans in write order.
	// A span ID written twice is kept twice, as CreateSpan documents ("If a
	// span ID already exists, an additional copy of the span will be
	// stored").
	traces map[string]map[string][]*Span
}

// NewServer is Cloud Trace keeping its spans in the state file, or in
// memory when state is "".
func NewServer(state string) (*Server, error) {
	s := &Server{state: state, now: time.Now, traces: map[string]map[string][]*Span{}}
	s.v1.s, s.v2.s, s.ot.s = s, s, s
	if state != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// SetDefaultProject is the project OTLP data naming no gcp.project_id is
// written to.
func (s *Server) SetDefaultProject(p string) { s.defaultProject = p }

// OnWrite sets what is told of the spans each write stores, outside the
// server's lock.
func (s *Server) OnWrite(f func([]*Span)) { s.onWrite = f }

// SetRouter sets what places OTLP spans that name no project: Cloud Run's
// request spans, exported by Knative, go to the service's project, and
// Knative's own control-plane spans are not Cloud Trace's (#2118).
func (s *Server) SetRouter(f func(attrs map[string]string) (project string, keep bool)) {
	s.router = f
}

// written tells onWrite of stored spans.
func (s *Server) written(spans []*Span) {
	if s.onWrite != nil && len(spans) > 0 {
		s.onWrite(spans)
	}
}

// SetClock replaces the clock, for tests.
func (s *Server) SetClock(now func() time.Time) { s.now = now }

// Register serves the v1 and v2 TraceServices and OTLP's on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	cloudtracepb.RegisterTraceServiceServer(g, &s.v1)
	cloudtracev2pb.RegisterTraceServiceServer(g, &s.v2)
	coltracepb.RegisterTraceServiceServer(g, &s.ot)
}

// RegisterCloudTrace serves the Cloud Trace v1 and v2 services only, for
// the JSON transcoder: OTLP's HTTP binding is served by OTLPHandler.
func (s *Server) RegisterCloudTrace(g grpc.ServiceRegistrar) {
	cloudtracepb.RegisterTraceServiceServer(g, &s.v1)
	cloudtracev2pb.RegisterTraceServiceServer(g, &s.v2)
}

// add stores spans; the caller holds s.mu.
func (s *Server) add(spans []*Span) error {
	for _, sp := range spans {
		byTrace := s.traces[sp.Project]
		if byTrace == nil {
			byTrace = map[string][]*Span{}
			s.traces[sp.Project] = byTrace
		}
		byTrace[sp.TraceID] = append(byTrace[sp.TraceID], sp)
	}
	return s.save()
}

// expire drops spans past retention; the caller holds s.mu.
func (s *Server) expire() {
	cutoff := s.now().Add(-Retention)
	for p, byTrace := range s.traces {
		for id, spans := range byTrace {
			kept := spans[:0]
			for _, sp := range spans {
				if sp.Received.After(cutoff) {
					kept = append(kept, sp)
				}
			}
			if len(kept) == 0 {
				delete(byTrace, id)
			} else {
				byTrace[id] = kept
			}
		}
		if len(byTrace) == 0 {
			delete(s.traces, p)
		}
	}
}

// Trace is one trace as the console and v1 read it.
type Trace struct {
	Project string
	TraceID string
	Spans   []*Span
}

// Root is the trace's root span: the earliest span with no parent, or
// whose parent is not in the trace; nil for no spans.
func (t Trace) Root() *Span {
	ids := map[string]bool{}
	for _, sp := range t.Spans {
		ids[sp.SpanID] = true
	}
	var root *Span
	for _, sp := range t.Spans {
		if p := sp.V2.GetParentSpanId(); p != "" && ids[p] {
			continue
		}
		if root == nil || sp.Start().Before(root.Start()) {
			root = sp
		}
	}
	return root
}

// Start and End bound the trace's spans.
func (t Trace) Start() time.Time {
	var st time.Time
	for i, sp := range t.Spans {
		if i == 0 || sp.Start().Before(st) {
			st = sp.Start()
		}
	}
	return st
}

// End is the latest span end.
func (t Trace) End() time.Time {
	var e time.Time
	for _, sp := range t.Spans {
		if sp.End().After(e) {
			e = sp.End()
		}
	}
	return e
}

// Traces are the projects' traces, each a copy, in trace ID order.
func (s *Server) Traces(projects ...string) []Trace {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	var out []Trace
	for _, p := range projects {
		for id, spans := range s.traces[p] {
			out = append(out, Trace{Project: p, TraceID: id, Spans: append([]*Span(nil), spans...)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TraceID != out[j].TraceID {
			return out[i].TraceID < out[j].TraceID
		}
		return out[i].Project < out[j].Project
	})
	return out
}

// Get is one trace of a project, and whether it exists.
func (s *Server) Get(project, traceID string) (Trace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	spans, ok := s.traces[project][traceID]
	if !ok {
		return Trace{}, false
	}
	return Trace{Project: project, TraceID: traceID, Spans: append([]*Span(nil), spans...)}, true
}

// ResetProject deletes a project's traces, or every trace for "".
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if project == "" {
		s.traces = map[string]map[string][]*Span{}
	} else {
		delete(s.traces, project)
	}
	return s.save()
}

type diskSpan struct {
	Project  string          `json:"project"`
	Received time.Time       `json:"received"`
	Span     json.RawMessage `json:"span"`
}

func (s *Server) save() error {
	if s.state == "" {
		return nil
	}
	var all []diskSpan
	for p, byTrace := range s.traces {
		for _, spans := range byTrace {
			for _, sp := range spans {
				raw, err := protojson.Marshal(sp.V2)
				if err != nil {
					return err
				}
				all = append(all, diskSpan{Project: p, Received: sp.Received, Span: raw})
			}
		}
	}
	raw, err := json.Marshal(all)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.state), 0o755); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
}

func (s *Server) load() error {
	raw, err := os.ReadFile(s.state)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var all []diskSpan
	if err := json.Unmarshal(raw, &all); err != nil {
		return fmt.Errorf("reading %s: %w", s.state, err)
	}
	for _, d := range all {
		v2 := &cloudtracev2pb.Span{}
		if err := protojson.Unmarshal(d.Span, v2); err != nil {
			return fmt.Errorf("reading %s: %w", s.state, err)
		}
		m := spanNameRE.FindStringSubmatch(v2.GetName())
		if m == nil {
			continue
		}
		sp := &Span{Project: d.Project, TraceID: m[2], SpanID: m[3], V2: v2, Received: d.Received}
		byTrace := s.traces[d.Project]
		if byTrace == nil {
			byTrace = map[string][]*Span{}
			s.traces[d.Project] = byTrace
		}
		byTrace[sp.TraceID] = append(byTrace[sp.TraceID], sp)
	}
	return nil
}

func cloneSpan(sp *cloudtracev2pb.Span) *cloudtracev2pb.Span {
	return proto.Clone(sp).(*cloudtracev2pb.Span)
}
