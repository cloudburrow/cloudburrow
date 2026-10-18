package logging

// entries:tail over the JSON API (#1410). logging_v2.yaml binds
// TailLogEntries to POST /v2/entries:tail, body "*", and Google's REST
// reference documents it ("Streaming read of log entries as they are
// received", docs.cloud.google.com/logging/docs/reference/v2/rest/v2/
// entries/tail). The shared transcoder serves unary calls only, so this
// handler serves it: the body is one TailLogEntriesRequest, and the response
// is the stream as gRPC transcoding streams a response, a JSON array whose
// elements are TailLogEntriesResponse objects, each flushed as it is sent,
// until the client goes away. A request TailLogEntries refuses answers the
// usual JSON error before anything is streamed. UNVERIFIED: no official
// client reads it (the Go apiv2 REST transport says TailLogEntries is "not
// yet supported for REST clients").
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/entries/tail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// tailJSONPath is TailLogEntries' JSON binding.
const tailJSONPath = "/v2/entries:tail"

// tailRecorder registers services with the transcoder and keeps the
// LoggingServiceV2 server, for entries:tail, and the ConfigServiceV2's
// Admin, for interceptChildren (intercept.go).
type tailRecorder struct {
	next  grpc.ServiceRegistrar
	srv   *Server
	admin *Admin
}

func (r *tailRecorder) RegisterService(desc *grpc.ServiceDesc, impl any) {
	if s, ok := impl.(*Server); ok {
		r.srv = s
	}
	if c, ok := impl.(configServer); ok {
		r.admin = c.a
	}
	r.next.RegisterService(desc, impl)
}

// jsonTail is a TailLogEntries stream over HTTP.
type jsonTail struct {
	ctx    context.Context
	w      http.ResponseWriter
	req    *loggingpb.TailLogEntriesRequest
	mu     sync.Mutex
	read   bool
	opened bool // the 200 and the array's "[" are written
	sent   int  // array elements written
}

// open writes the 200 and opens the array, once. TailLogEntries calls
// SendHeader when it has subscribed, so a client that sees "[" knows every
// entry written after it is tailed (internal/service/logging run 38941061488:
// opening first lost the entries written before the subscription).
func (t *jsonTail) open() {
	if t.opened {
		return
	}
	t.opened = true
	t.w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	t.w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(t.w, "[\n")
	if f, ok := t.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *jsonTail) Context() context.Context    { return t.ctx }
func (t *jsonTail) SetHeader(metadata.MD) error { return nil }
func (t *jsonTail) SendHeader(metadata.MD) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.open()
	return nil
}
func (t *jsonTail) SetTrailer(metadata.MD) {}
func (t *jsonTail) SendMsg(m any) error    { return t.Send(m.(*loggingpb.TailLogEntriesResponse)) }
func (t *jsonTail) RecvMsg(m any) error    { return apierror.Internal(nil, "RecvMsg is not used") }
func (t *jsonTail) Recv() (*loggingpb.TailLogEntriesRequest, error) {
	t.mu.Lock()
	first := !t.read
	t.read = true
	t.mu.Unlock()
	if first {
		return t.req, nil
	}
	// The body held the one request; the stream has nothing more to read.
	<-t.ctx.Done()
	return nil, io.EOF
}

// Send writes one array element and flushes it.
func (t *jsonTail) Send(resp *loggingpb.TailLogEntriesResponse) error {
	b, err := protojson.Marshal(resp)
	if err != nil {
		return apierror.Internal(err, "encoding a tail response")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.open()
	sep := ",\n"
	if t.sent == 0 {
		sep = ""
	}
	t.sent++
	if _, err := io.WriteString(t.w, sep); err != nil {
		return err
	}
	if _, err := t.w.Write(b); err != nil {
		return err
	}
	if f, ok := t.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// serveTailJSON serves POST /v2/entries:tail.
func (s *Server) serveTailJSON(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		writeTailError(w, apierror.InvalidArgument("reading the body: %v", err))
		return
	}
	req := &loggingpb.TailLogEntriesRequest{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, req); err != nil {
		writeTailError(w, apierror.InvalidArgument("the body is not a TailLogEntriesRequest: %v", err))
		return
	}
	// A tail TailLogEntries would refuse is the usual JSON error; an
	// accepted one opens the array as soon as it has subscribed (SendHeader),
	// so the client sees the stream start before any entry arrives.
	if _, _, _, err := s.tailSpec(req); err != nil {
		writeTailError(w, err)
		return
	}
	t := &jsonTail{ctx: r.Context(), w: w, req: req}
	_ = s.TailLogEntries(t)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.open()
	_, _ = io.WriteString(w, "\n]")
}

// writeTailError is the error envelope the transcoder writes for the other
// JSON methods: code, message and status.
func writeTailError(w http.ResponseWriter, err error) {
	e := apierror.From(err)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(e.HTTPStatus())
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": e.HTTPStatus(), "message": e.Message, "status": rpccode.Code(e.Code).String()}})
}
