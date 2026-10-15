package netfwd

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Call is one request a guarded tunnel carried (#1267), for the request
// metrics and the request log of an emulator CloudBurrow does not serve
// itself: Firestore's and Datastore's calls from the host pass through the
// guard, which sees each request's method and outcome though it passes the
// bytes through unread. Never a body, a query string or a header but
// traceparent.
type Call struct {
	// Method is the full gRPC method for a gRPC call, or for REST the HTTP
	// method and path, "GET /v1/projects/p/databases/(default)/documents/c".
	Method string
	// GRPC reports a gRPC call; Code is then its gRPC code, else the HTTP
	// status.
	GRPC bool
	Code int
	// Resource is what the call names, from the routing headers the
	// official clients send (google-cloud-resource-prefix,
	// x-goog-request-params) or a REST path: "projects/p/databases/(default)"
	// or "projects/p"; "" when it names none.
	Resource    string
	Time        time.Time
	Duration    time.Duration
	TraceParent string
}

// observeGuard wraps a guard's handler so observe is told about each request
// once it is answered. An h2c upgrade is not a call: the connection is then
// the client's and the emulator's, and what crosses it is not seen.
func observeGuard(next http.Handler, observe func(Call)) http.Handler {
	if observe == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := h2cUpgrade(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		c := Call{Method: r.Method + " " + r.URL.Path, Code: sw.code, Time: start, Duration: time.Since(start),
			TraceParent: r.Header.Get("traceparent")}
		if isGRPC(r) {
			c.Method, c.GRPC, c.Code = r.URL.Path, true, grpcCode(w.Header(), sw.code)
			c.Resource = resourceOf(r.Header)
		} else if _, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/"); ok && strings.HasPrefix(rest, "projects/") {
			c.Resource, _, _ = strings.Cut(rest, ":")
		}
		observe(c)
	})
}

// grpcCode is a gRPC response's status: grpc-status, in the headers of a
// Trailers-Only answer or among the trailers ReverseProxy copied into the
// header map. A response with neither failed in transport: UNAVAILABLE (14)
// for a 502 or 503, else UNKNOWN (2).
func grpcCode(h http.Header, status int) int {
	for _, k := range []string{"Grpc-Status", http.TrailerPrefix + "Grpc-Status"} {
		if v := h.Get(k); v != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	if status == http.StatusBadGateway || status == http.StatusServiceUnavailable {
		return grpcUnavailable
	}
	return 2
}

// statusWriter keeps the status a handler answered with. It unwraps, so
// ReverseProxy's flushes, and its hijack of an upgraded connection, reach
// the server's writer.
type statusWriter struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote && code >= 200 {
		w.code, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// resourceOf reads the resource a gRPC call names from the routing headers
// the official clients send: Firestore's google-cloud-resource-prefix
// ("projects/p/databases/(default)"), or x-goog-request-params, whose
// "database=..." or "project_id=p" names it.
func resourceOf(h http.Header) string {
	if v := h.Get("Google-Cloud-Resource-Prefix"); strings.HasPrefix(v, "projects/") {
		return v
	}
	q, err := url.ParseQuery(h.Get("X-Goog-Request-Params"))
	if err != nil {
		return ""
	}
	for _, k := range []string{"database", "name", "parent"} {
		if v := q.Get(k); strings.HasPrefix(v, "projects/") {
			return v
		}
	}
	if p := q.Get("project_id"); p != "" {
		return "projects/" + p
	}
	return ""
}
