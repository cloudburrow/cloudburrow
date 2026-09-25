package storage

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Requests observed and faulted (#513): storage is a measured service. Each
// request is resolved to its API method (a discovery ID such as
// storage.objects.insert, or xml.<VERB> for the XML API) and, after it is
// served, reported as a Call: method, bucket, object, status and duration.
// A Call never carries the query string, so no resumable upload_id, rewrite
// token, signed-URL signature or page token reaches an event, a metric or a
// log; nor a request or response body, so no HMAC secret does either.
//
// A Faulter can fail or delay a request before it is served. The fault is
// written as storage's own error body, JSON or XML by the surface, not as a
// gRPC status.
//
// The server also keeps the last calls in a bounded buffer at GET
// /_cloudburrow/events?after=N, which a CLI scrapes from a server it does
// not run in-process (the in-cluster Deployment, #514).

// Call is one request as it was served.
type Call struct {
	Seq      uint64        `json:"seq"`
	Method   string        `json:"method"`
	Bucket   string        `json:"bucket,omitempty"`
	Object   string        `json:"object,omitempty"`
	Status   int           `json:"status"`
	Duration time.Duration `json:"durationNs"`
	Time     time.Time     `json:"time"`
}

// Faulter decides whether to fault a request, and how: a status to answer
// with (0 for none, delay only) and a delay first.
type Faulter interface {
	Fault(method, resource string) (status int, delay time.Duration, ok bool)
}

const (
	eventsPath     = "/_cloudburrow/events"
	eventRingSize  = 1000
	adminPathStart = "/_cloudburrow/"
)

type callRing struct {
	mu    sync.Mutex
	seq   uint64
	calls []Call
}

func (c *callRing) add(call Call) Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	call.Seq = c.seq
	c.calls = append(c.calls, call)
	if len(c.calls) > eventRingSize {
		c.calls = c.calls[len(c.calls)-eventRingSize:]
	}
	return call
}

func (c *callRing) after(n uint64) []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Call
	for _, call := range c.calls {
		if call.Seq > n {
			out = append(out, call)
		}
	}
	return out
}

// resolveMethod names the API method a request is for, and its bucket and
// object, from the path alone.
func (s *Server) resolveMethod(r *http.Request) (method, bucket, object string) {
	path := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(path, jsonPrefix):
		rest := strings.TrimPrefix(path, jsonPrefix)
		bucket, object = jsonTarget(r, jsonPrefix)
		return s.discoveryID(r.Method, rest, false), bucket, object
	case strings.HasPrefix(path, uploadPrefix), strings.HasPrefix(path, resumablePrefix):
		prefix := uploadPrefix
		if strings.HasPrefix(path, resumablePrefix) {
			prefix = resumablePrefix
		}
		bucket = pathVar(r, prefix, 1)
		return "storage.objects.insert", bucket, r.URL.Query().Get("name")
	case strings.HasPrefix(path, downloadPrefix):
		bucket, object = jsonTarget(r, downloadPrefix)
		return "storage.objects.get", bucket, object
	case path == batchPath || strings.HasPrefix(path, batchPath+"/"):
		return "storage.batch", "", ""
	case strings.HasPrefix(path, adminPathStart):
		return "admin", "", ""
	}
	bucket, object = s.xmlTarget(r)
	return "xml." + r.Method, bucket, object
}

func jsonTarget(r *http.Request, prefix string) (bucket, object string) {
	if pathVar(r, prefix, 0) == "b" {
		bucket = pathVar(r, prefix, 1)
		if pathVar(r, prefix, 2) == "o" {
			object = pathVar(r, prefix, 3)
		}
	}
	return bucket, object
}

func (s *Server) discoveryID(verb, rest string, upload bool) string {
	for _, m := range s.methods {
		if m.Verb == verb && m.re.MatchString(rest) && (!upload || m.Upload) {
			return m.ID
		}
	}
	return "storage.unknown"
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// observed serves one request under the observer and the faulter.
func (s *Server) observed(w http.ResponseWriter, r *http.Request, serve func(http.ResponseWriter, *http.Request)) {
	method, bucket, object := s.resolveMethod(r)
	if method == "admin" {
		serve(w, r)
		return
	}
	start := s.now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	resource := ""
	if bucket != "" {
		resource = "b/" + bucket
		if object != "" {
			resource += "/o/" + object
		}
	}
	if s.faults != nil {
		if status, delay, ok := s.faults.Fault(method, resource); ok {
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
				}
			}
			if status != 0 {
				msg := "injected fault: " + http.StatusText(status)
				if strings.HasPrefix(method, "xml.") {
					writeXMLError(rec, status, "InternalError", msg)
				} else {
					writeError(rec, errorf(status, "injectedFault", "%s", msg))
				}
				s.report(Call{Method: method, Bucket: bucket, Object: object, Status: rec.status, Duration: s.now().Sub(start), Time: start})
				return
			}
		}
	}
	serve(rec, r)
	s.report(Call{Method: method, Bucket: bucket, Object: object, Status: rec.status, Duration: s.now().Sub(start), Time: start})
}

func (s *Server) report(c Call) {
	c = s.ring.add(c)
	if s.observe != nil {
		s.observe(c)
	}
}

// serveEvents is GET /_cloudburrow/events?after=N.
func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	calls := s.ring.after(after)
	if calls == nil {
		calls = []Call{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"calls": calls})
}
