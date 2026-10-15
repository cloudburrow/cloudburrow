package pubsubfront

// The front's call log (#1266): every Pub/Sub call a client makes through
// the front, gRPC or REST, is kept in a bounded ring that the CLI reads at
// CallsPath through the host tunnel, as it reads the builtin storage
// server's (#513), since a pod cannot reach the CLI, which binds loopback
// (ADR-0004). The CLI feeds each call into /metrics, /admin/events (the
// console's request log) and, with tracing on, a span.
//
// A call keeps its method, the resource it named, its outcome, when it
// started and how long it took, and the W3C traceparent it arrived with, so
// its span continues the client's trace. Never a payload, a header other
// than traceparent, or a query string: nothing a message or a token is in
// can reach an event or a metric label.

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
	// CallsPath serves the call log: GET CallsPath?after=N answers
	// {"calls": [...]}, the calls numbered after N, oldest first.
	CallsPath = "/_cloudburrow/calls"
	// callRingSize bounds the log; a CLI polls it every few seconds.
	callRingSize = 1000
)

// Call is one completed call through the front.
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
	// Resource is the topic, subscription, snapshot or project the call
	// named, when it named one.
	Resource string `json:"resource,omitempty"`
	// Stream reports a streaming call, StreamingPull.
	Stream      bool          `json:"stream,omitempty"`
	Time        time.Time     `json:"time"`
	Duration    time.Duration `json:"durationNs"`
	TraceParent string        `json:"traceparent,omitempty"`
}

type callRing struct {
	mu    sync.Mutex
	seq   uint64
	calls []Call
	next  int
	full  bool
}

func (r *callRing) add(c Call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = make([]Call, callRingSize)
	}
	r.seq++
	c.Seq = r.seq
	r.calls[r.next] = c
	r.next = (r.next + 1) % len(r.calls)
	if r.next == 0 {
		r.full = true
	}
}

// after returns the kept calls numbered after seq, oldest first.
func (r *callRing) after(seq uint64) []Call {
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

// Calls returns the calls numbered after seq, oldest first.
func (f *Front) Calls(after uint64) []Call { return f.calls.after(after) }

// record keeps one call, unless it is one of CloudBurrow's own methods,
// which a client of Pub/Sub never makes.
func (f *Front) record(c Call) {
	if strings.HasPrefix(c.Method, "/cloudburrow.") {
		return
	}
	f.calls.add(c)
}

// serveCalls is GET CallsPath?after=N.
func (f *Front) serveCalls(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"calls": f.Calls(after)})
}

// recordingStream notes the resource the first request names.
type recordingStream struct {
	grpc.ServerStream
	resource string
	seen     bool
}

func (s *recordingStream) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if err == nil && !s.seen {
		s.seen = true
		if fr, ok := m.(*frame); ok {
			s.resource = firstName(*fr)
		}
	}
	return err
}

// streamRecorder records every call the gRPC server serves, by the
// official clients' own transport and by ServeHTTP alike.
func (f *Front) streamRecorder(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	start := time.Now()
	rs := &recordingStream{ServerStream: ss}
	err := handler(srv, rs)
	c := Call{Method: info.FullMethod, Transport: "grpc", Code: int(status.Code(err)), Resource: rs.resource,
		Stream: info.FullMethod == subscriber+"StreamingPull", Time: start, Duration: time.Since(start),
		TraceParent: traceParentOf(ss.Context())}
	f.record(c)
	return err
}

func traceParentOf(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("traceparent"); len(v) > 0 {
		return v[0]
	}
	return ""
}

// firstName reads field 1 of a request when it is a resource name: every
// Pub/Sub request names its topic, subscription, snapshot or project there
// (Publish.topic, Pull.subscription, Topic.name, ListTopics.project, ...).
func firstName(b []byte) string {
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

// recordREST wraps the REST API so each request is recorded once answered.
func (f *Front) recordREST(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &codeWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		c := Call{Method: r.Method + " " + r.URL.Path, Transport: "http", Code: sw.code,
			Time: start, Duration: time.Since(start), TraceParent: r.Header.Get("traceparent")}
		if _, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/"); ok && strings.HasPrefix(rest, "projects/") {
			c.Resource, _, _ = strings.Cut(rest, ":")
		}
		f.record(c)
	})
}

// codeWriter keeps the status a handler answered with.
type codeWriter struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (w *codeWriter) WriteHeader(code int) {
	if !w.wrote {
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
