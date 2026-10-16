// Package calllog is the call log a front in an emulator's pod keeps for the
// CLI (#1266, #1267): every call a client makes through the front, gRPC or
// REST, in a bounded ring the CLI reads at Path through the host tunnel, as
// it reads the builtin storage server's (#513), since a pod cannot reach the
// CLI, which binds loopback (ADR-0004). The CLI feeds each call into
// /metrics, /admin/events (the console's request log) and, with tracing on,
// a span.
//
// A call keeps its method, the resource it named, its outcome, when it
// started and how long it took, and the W3C traceparent it arrived with, so
// its span continues the client's trace. Never a payload, a header other
// than traceparent, or a query string: nothing a message or a token is in
// can reach an event or a metric label.
package calllog

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	// Path serves the call log: GET Path?after=N answers {"calls": [...]},
	// the calls numbered after N, oldest first.
	Path = "/_cloudburrow/calls"
	// RingSize bounds the log; a CLI polls it every few seconds.
	RingSize = 1000
)

// Call is one completed call through a front.
type Call struct {
	Seq uint64 `json:"seq"`
	// Method is the full gRPC method, /google.pubsub.v1.Publisher/Publish,
	// or for REST the HTTP method and path, "POST /v1/projects/p/topics/t:publish".
	Method string `json:"method"`
	// Transport is "grpc" or "http".
	Transport string `json:"transport"`
	// Code is the gRPC code for a gRPC call, as a number, and the HTTP
	// status for a REST one.
	Code int `json:"code"`
	// Resource is what the call named, when it named something.
	Resource string `json:"resource,omitempty"`
	// Stream reports a streaming call.
	Stream      bool          `json:"stream,omitempty"`
	Time        time.Time     `json:"time"`
	Duration    time.Duration `json:"durationNs"`
	TraceParent string        `json:"traceparent,omitempty"`
}

// Ring keeps the last RingSize calls. The zero value is ready to use.
type Ring struct {
	mu    sync.Mutex
	seq   uint64
	calls []Call
	next  int
	full  bool
}

// Add keeps one call, numbering it after the last.
func (r *Ring) Add(c Call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = make([]Call, RingSize)
	}
	r.seq++
	c.Seq = r.seq
	r.calls[r.next] = c
	r.next = (r.next + 1) % len(r.calls)
	if r.next == 0 {
		r.full = true
	}
}

// After returns the kept calls numbered after seq, oldest first.
func (r *Ring) After(seq uint64) []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ordered []Call
	if r.full {
		ordered = append(ordered, r.calls[r.next:]...)
	}
	ordered = append(ordered, r.calls[:r.next]...)
	out := []Call{}
	for _, c := range ordered {
		if c.Seq > seq {
			out = append(out, c)
		}
	}
	return out
}

// ServeHTTP is GET Path?after=N.
func (r *Ring) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	after, _ := strconv.ParseUint(req.URL.Query().Get("after"), 10, 64)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"calls": r.After(after)})
}

// recordingStream notes the resource the first request names. A handler
// may still be receiving on another goroutine when it returns (the Bigtable
// front's proxy pumps requests apart from responses), so the fields are
// read and written under mu (main run 38941061488, -race).
type recordingStream struct {
	grpc.ServerStream
	name     func(any) string
	mu       sync.Mutex
	resource string
	seen     bool
}

func (s *recordingStream) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if err == nil {
		s.mu.Lock()
		if !s.seen {
			s.seen = true
			s.resource = s.name(m)
		}
		s.mu.Unlock()
	}
	return err
}

// named is the resource the first request named, if one has been read.
func (s *recordingStream) named() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resource
}

// StreamInterceptor records every call a front's gRPC server serves, by
// grpc-go's own transport and by ServeHTTP alike, except CloudBurrow's own
// methods (/cloudburrow.*), which a client of the API never makes. name reads
// the resource a call's first request names, and streaming reports a
// streaming method.
func StreamInterceptor(r *Ring, name func(any) string, streaming func(method string) bool) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if strings.HasPrefix(info.FullMethod, "/cloudburrow.") {
			return handler(srv, ss)
		}
		start := time.Now()
		rs := &recordingStream{ServerStream: ss, name: name}
		err := handler(srv, rs)
		r.Add(Call{Method: info.FullMethod, Transport: "grpc", Code: int(status.Code(err)), Resource: rs.named(),
			Stream: streaming(info.FullMethod), Time: start, Duration: time.Since(start),
			TraceParent: traceParentOf(ss.Context())})
		return err
	}
}

func traceParentOf(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("traceparent"); len(v) > 0 {
		return v[0]
	}
	return ""
}

// FirstName reads field 1 of an encoded request when it is a resource name,
// "projects/...": where Pub/Sub's and Bigtable's requests name their topic,
// subscription, table or parent (Publish.topic, Pull.subscription,
// ReadRows.table_name, CreateTable.parent, ...).
func FirstName(b []byte) string {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return ""
		}
		b = b[n:]
		if num == 1 && typ == protowire.BytesType {
			v, n := protowire.ConsumeBytes(b)
			if n < 0 || !strings.HasPrefix(string(v), "projects/") {
				return ""
			}
			return string(v)
		}
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			return ""
		}
		b = b[n:]
	}
	return ""
}

// REST wraps a REST API so each request is recorded once answered.
func REST(r *Ring, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		sw := &codeWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, req)
		c := Call{Method: req.Method + " " + req.URL.Path, Transport: "http", Code: sw.code,
			Time: start, Duration: time.Since(start), TraceParent: req.Header.Get("traceparent")}
		if _, rest, ok := strings.Cut(strings.TrimPrefix(req.URL.Path, "/"), "/"); ok && strings.HasPrefix(rest, "projects/") {
			c.Resource, _, _ = strings.Cut(rest, ":")
		}
		r.Add(c)
	})
}

// codeWriter keeps the status a handler answered with.
type codeWriter struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (w *codeWriter) WriteHeader(code int) {
	if !w.wrote && code >= 200 {
		w.code, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *codeWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *codeWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *codeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
