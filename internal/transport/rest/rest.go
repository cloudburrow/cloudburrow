// Package rest implements the HTTP transport for the APIs CloudBurrow serves
// itself: Cloud Tasks and the Cloud Run v2 adapter.
//
// It converts wire formats to and from service calls and contains no service
// behaviour of its own (docs/architecture.md §3). Routing is explicit: an
// unknown path returns a protocol-correct 404 rather than being absorbed by a
// catch-all that might fabricate a plausible response.
package rest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/identity-wael/cloudburrow/internal/apierror"
)

// MaxRequestBytes bounds a request body. Unbounded decoding lets one client
// exhaust the process's memory.
const MaxRequestBytes = 32 << 20 // 32 MiB

// Handler is a service method. Returning an error is rendered by WriteError.
type Handler func(w http.ResponseWriter, r *http.Request) error

// Router dispatches by method and path template.
//
// Templates use the Go 1.22+ ServeMux pattern syntax, so path variables are
// matched by the standard library rather than by hand-rolled parsing.
type Router struct {
	mux *http.ServeMux
	// registered records what exists, so a 404 can say what the surface
	// actually offers instead of leaving a caller guessing.
	registered []string
}

// NewRouter returns a router with an explicit not-found fallback.
//
// The fallback is registered rather than handled in ServeHTTP because
// ServeMux.Handler does not populate path values — only ServeHTTP does — so
// inspecting the match up front would silently break every {variable}.
func NewRouter() *Router {
	r := &Router{mux: http.NewServeMux()}
	r.mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, apierror.NotFound("no such method: %s %s", req.Method, req.URL.Path))
	})
	return r
}

// Handle registers a handler for a method and pattern, e.g.
// "POST /v2/projects/{project}/locations/{location}/queues".
func (r *Router) Handle(pattern string, h Handler) {
	r.registered = append(r.registered, pattern)
	r.mux.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
		req.Body = http.MaxBytesReader(w, req.Body, MaxRequestBytes)
		if err := h(w, req); err != nil {
			WriteError(w, err)
		}
	})
}

// Routes returns the registered patterns, in registration order.
func (r *Router) Routes() []string { return append([]string(nil), r.registered...) }

// ServeHTTP dispatches a request. Unmatched paths reach the not-found
// fallback registered in NewRouter, so an unknown method is always reported
// rather than absorbed.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

// WriteError renders an error in the Google JSON envelope.
func WriteError(w http.ResponseWriter, err error) {
	apierror.WriteJSON(w, err)
}

// WriteJSON renders a successful response.
func WriteJSON(w http.ResponseWriter, status int, body any) error {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	if body == nil {
		return nil
	}
	return json.NewEncoder(w).Encode(body)
}

// DecodeJSON reads a JSON request body.
//
// Unknown fields are rejected: a client sending a field we silently drop would
// believe it took effect.
func DecodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return apierror.InvalidArgument("request body is required")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if ok := asMaxBytes(err, &maxErr); ok {
			return apierror.InvalidArgument("request body exceeds %d bytes", MaxRequestBytes)
		}
		return apierror.InvalidArgument("malformed request body: %v", err)
	}
	// A second value in the stream means the caller sent something we would
	// otherwise ignore.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return apierror.InvalidArgument("request body must contain a single JSON object")
	}
	return nil
}

func asMaxBytes(err error, target **http.MaxBytesError) bool {
	for err != nil {
		if e, ok := err.(*http.MaxBytesError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// PathValue returns a required path variable.
func PathValue(r *http.Request, name string) (string, error) {
	v := r.PathValue(name)
	if v == "" {
		return "", apierror.InvalidArgument("missing path parameter %q", name)
	}
	return v, nil
}

// QueryInt reads an optional integer query parameter.
func QueryInt(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, apierror.InvalidArgument("%s must be an integer, got %q", name, raw)
	}
	return n, nil
}

// ResourceName builds a full resource name from path variables.
func ResourceName(r *http.Request, collection string) (string, error) {
	project, err := PathValue(r, "project")
	if err != nil {
		return "", err
	}
	location, err := PathValue(r, "location")
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("projects/%s/locations/%s", project, location)
	if collection == "" {
		return name, nil
	}
	id := r.PathValue(collection)
	if id == "" {
		return name + "/" + collection, nil
	}
	return strings.Join([]string{name, collection, id}, "/"), nil
}
