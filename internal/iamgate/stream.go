package iamgate

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// Routed stands, in a checked resource, for the database a call's routing
// header (x-goog-request-params database_id, or a REST call's databaseId)
// names, "(default)" for none: the Datastore Admin API's requests name
// only their project.
const Routed = "{routed-database}"

type routingKey struct{}

// withRouting is ctx carrying a call's routing parameters (the
// x-goog-request-params form, "a=b&c=d").
func withRouting(ctx context.Context, params string) context.Context {
	if params == "" {
		return ctx
	}
	return context.WithValue(ctx, routingKey{}, params)
}

// routedDatabase is the database the routing parameters in ctx name.
func routedDatabase(ctx context.Context) string {
	params, _ := ctx.Value(routingKey{}).(string)
	if q, err := url.ParseQuery(params); err == nil && q.Get("database_id") != "" {
		return q.Get("database_id")
	}
	return "(default)"
}

// clientStreaming reports whether full ("/pkg.Svc/M") takes a stream of
// requests.
func clientStreaming(full string) bool {
	svc, name, ok := strings.Cut(strings.TrimPrefix(full, "/"), "/")
	if !ok {
		return false
	}
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
	if err != nil {
		return false
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return false
	}
	md := sd.Methods().ByName(protoreflect.Name(name))
	return md != nil && md.IsStreamingClient()
}

// serveStream serves a client-streaming call whose first message (first)
// passed, checking each later message before the emulator sees it: a
// message whose checks fail is not passed on, the call to the emulator is
// ended, and the client gets the refusal as the call's status, after
// whatever the emulator had answered before it. A later message that does
// not name the database (a Firestore Write's) is checked on the first's.
func (g *Gate) serveStream(ctx context.Context, w http.ResponseWriter, r *http.Request, next http.Handler,
	bearer string, first protoreflect.Message) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sw := &streamWriter{w: w}
	fc := &frameChecker{src: bufio.NewReader(r.Body), closer: r.Body, skip: 1}
	fc.check = func(flag byte, body []byte) error {
		msg, err := decodeFrame(r.URL.Path, r.Header.Get("Grpc-Encoding"), flag, body)
		if err != nil || msg == nil {
			return err
		}
		inherit(msg, first, "database")
		return g.Authorize(ctx, bearer, r.URL.Path, msg)
	}
	fc.deny = func(err error) {
		sw.deny(err)
		cancel()
	}
	r2 := r.WithContext(ctx)
	r2.Body = fc
	func() {
		defer func() {
			if v := recover(); v != nil && (v != http.ErrAbortHandler || sw.denied() == nil) {
				panic(v)
			}
		}()
		next.ServeHTTP(sw, r2)
	}()
	sw.finish()
}

// inherit sets m's string field name from first's when m leaves it empty.
func inherit(m, first protoreflect.Message, name string) {
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil || fd.Kind() != protoreflect.StringKind || fd.IsList() || m.Get(fd).String() != "" {
		return
	}
	if v := String(first, name); v != "" {
		m.Set(fd, protoreflect.ValueOfString(v))
	}
}

// frameChecker is a gRPC request body checking each message (after the
// first skip) before handing it on.
type frameChecker struct {
	src    *bufio.Reader
	closer io.Closer
	skip   int
	check  func(flag byte, body []byte) error
	deny   func(error)

	pending []byte
	done    bool
}

func (f *frameChecker) Read(p []byte) (int, error) {
	for len(f.pending) == 0 {
		if f.done {
			return 0, io.EOF
		}
		var hdr [5]byte
		if _, err := io.ReadFull(f.src, hdr[:]); err != nil {
			return 0, err
		}
		n := binary.BigEndian.Uint32(hdr[1:])
		if n > maxBody {
			f.done = true
			f.deny(status.Errorf(codes.ResourceExhausted, "request message of %d bytes is too large to check", n))
			return 0, io.EOF
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(f.src, body); err != nil {
			return 0, err
		}
		if f.skip > 0 {
			f.skip--
		} else if err := f.check(hdr[0], body); err != nil {
			f.done = true
			f.deny(err)
			return 0, io.EOF
		}
		f.pending = append(hdr[:], body...)
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

func (f *frameChecker) Close() error { return f.closer.Close() }

// streamWriter is the response of a checked stream: once a message is
// refused, nothing more of the emulator's answer reaches the client, and
// the refusal is the call's status.
type streamWriter struct {
	w http.ResponseWriter

	mu          sync.Mutex
	err         error
	wroteHeader bool
}

func (s *streamWriter) denied() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *streamWriter) deny(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *streamWriter) Header() http.Header { return s.w.Header() }

func (s *streamWriter) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil || s.wroteHeader {
		return
	}
	s.wroteHeader = true
	s.w.WriteHeader(code)
}

func (s *streamWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return len(b), nil
	}
	s.wroteHeader = true
	return s.w.Write(b)
}

func (s *streamWriter) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	if f, ok := s.w.(http.Flusher); ok {
		s.wroteHeader = true
		f.Flush()
	}
}

func (s *streamWriter) Unwrap() http.ResponseWriter { return s.w }

// finish ends a refused stream with the refusal: Trailers-Only when
// nothing was answered yet, else as the call's trailers.
func (s *streamWriter) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		return
	}
	st := status.Convert(s.err)
	if !s.wroteHeader {
		netfwd.WriteGRPCStatus(s.w, int(st.Code()), st.Message())
		return
	}
	h := s.w.Header()
	h.Set(http.TrailerPrefix+"Grpc-Status", strconv.Itoa(int(st.Code())))
	h.Set(http.TrailerPrefix+"Grpc-Message", netfwd.GRPCPercentEncode(st.Message()))
}
