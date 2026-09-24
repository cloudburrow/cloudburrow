package rest

import (
	"net/http"
	"time"
)

// Request is one completed HTTP request, as an observer sees it.
//
// The path and never the query string, and no body: the same rule as the gRPC
// observer, for the same reason. A query string is where a token or a key ends
// up when a caller is careless, and a recorder that kept it would be keeping it
// for everyone who can read the admin API.
type Request struct {
	Method   string
	Path     string
	Status   int
	Duration time.Duration
}

// Observe wraps next so o is told about each request when it completes.
func Observe(next http.Handler, o func(Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		o(Request{Method: r.Method, Path: r.URL.Path, Status: sw.status, Duration: time.Since(start)})
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

// Flush passes through, so a streaming response is not buffered by the wrapper.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
