// Package console serves CloudBurrow's local web console.
//
// The console is a **view**, not a second system. Every resource it shows is
// read through the same surfaces an SDK client uses, so a bucket created with
// the Go client appears here and one created here is visible to the client.
// Keeping a store of its own would give CloudBurrow two answers to the same
// question, and the one the developer saw would be whichever they happened to
// ask.
//
// The backend layer here is deliberately thin: it exists because a browser
// cannot speak gRPC or hold cluster credentials, not to add behaviour. Nothing
// in it decides anything a service would decide differently.
package console

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed assets
var assets embed.FS

// Resource is one item on a list screen.
//
// It is deliberately generic: the console renders tables, and a per-service
// shape for each would multiply the backend without changing what a table
// draws. Service-specific detail lives in Fields.
type Resource struct {
	// Name is the resource's own name, as its API reports it.
	Name string `json:"name"`
	// Status is a short state word, or empty when the resource has no state.
	Status string `json:"status,omitempty"`
	// Fields are additional columns, rendered in the order Columns gives.
	Fields map[string]string `json:"fields,omitempty"`
	// Link is the detail route, or empty when there is no detail screen.
	Link string `json:"link,omitempty"`
}

// Listing is a page of resources.
type Listing struct {
	// Columns names the Fields keys to render, in order.
	Columns []string   `json:"columns"`
	Items   []Resource `json:"items"`
	// Total is the number of resources before paging.
	Total int `json:"total"`
	// Unavailable, when set, means the service could not be reached. It is
	// distinct from an empty list: a developer shown an empty table for a
	// broken backend goes looking for a bug in their own code.
	Unavailable string `json:"unavailable,omitempty"`
	// Note carries a caveat about what the listing actually shows — for
	// instance that a filter the screen offers is not honoured by the
	// backend. A screen that silently ignores a filter is lying about what
	// the rows are.
	Note string `json:"note,omitempty"`
}

// Provider reads live state for one service.
//
// Implementations call the service's own API rather than reaching into a
// store, which is what keeps the console a view.
type Provider interface {
	// ID is the URL segment and navigation key, e.g. "storage".
	ID() string
	// Title is the screen name, matching the console page it mirrors.
	Title() string
	// List returns the resources in a project.
	List(ctx context.Context, project string) (Listing, error)
}

// Status is what the dashboard reports about the instance.
type Status struct {
	Instance   string            `json:"instance"`
	Ready      bool              `json:"ready"`
	State      string            `json:"state"`
	Cluster    string            `json:"cluster"`
	Kubernetes string            `json:"kubernetes,omitempty"`
	Namespace  string            `json:"namespace"`
	Mode       string            `json:"mode"`
	Services   []ServiceStatus   `json:"services"`
	Endpoints  map[string]string `json:"endpoints"`
	Components map[string]bool   `json:"components,omitempty"`
}

// ServiceStatus is one service's availability.
type ServiceStatus struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Enabled bool   `json:"enabled"`
	// Reason explains a disabled service, so a greyed-out entry is never
	// unexplained.
	Reason string `json:"reason,omitempty"`
}

// StatusSource supplies the instance status.
type StatusSource func(ctx context.Context) Status

// Server serves the console.
type Server struct {
	addr      string
	providers map[string]Provider
	order     []string
	status    StatusSource

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	done chan struct{}
}

// New returns a console server bound to addr.
func New(addr string, status StatusSource, providers ...Provider) *Server {
	s := &Server{addr: addr, providers: map[string]Provider{}, status: status}
	for _, p := range providers {
		if p == nil {
			continue
		}
		s.providers[p.ID()] = p
		s.order = append(s.order, p.ID())
	}
	return s
}

func (s *Server) Name() string { return "console" }

// Addr returns the resolved address, or "" before Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// URL returns the address a developer opens, or "" before Start.
func (s *Server) URL() string {
	if addr := s.Addr(); addr != "" {
		return "http://" + addr
	}
	return ""
}

// Handler builds the routes. Exported so tests drive it without a listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/services", s.handleServices)
	mux.HandleFunc("GET /api/resources/{service}", s.handleResources)

	ui, err := fs.Sub(assets, "assets")
	if err != nil {
		// The assets are embedded at build time; a failure here means the
		// binary is malformed, and serving a console with no UI would be
		// worse than saying so.
		panic("console assets are missing from the binary: " + err.Error())
	}
	files := http.FileServer(http.FS(ui))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Every non-asset path serves the shell, so a deep link opened
		// directly renders rather than 404ing. The client router then reads
		// the path.
		if isAsset(r.URL.Path) {
			files.ServeHTTP(w, r)
			return
		}
		s.serveShell(w, r)
	})

	return sameOriginOnly(noStore(mux))
}

func isAsset(path string) bool {
	for _, ext := range []string{".css", ".js", ".svg", ".png", ".ico", ".woff2", ".map"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func (s *Server) serveShell(w http.ResponseWriter, r *http.Request) {
	body, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "console assets are missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A console that loaded remote code would defeat the point of shipping
	// the assets: it would stop working offline and would widen what the page
	// can reach. The policy is enforced rather than merely intended.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
			"connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; "+
			"form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write(body)
}

// noStore keeps the browser from caching live state.
//
// A cached resource list is worse than a slow one: it shows a developer a
// bucket they deleted and lets them conclude the delete did not work.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// sameOriginOnly rejects cross-site requests to the API.
//
// The console binds loopback, but loopback is reachable from any page the
// developer's browser has open. Without this, a visited website could drive
// the API — including deletes — because the browser would send the request
// happily. Sec-Fetch-Site is checked because it cannot be set by script.
func sameOriginOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
			// Empty covers clients that send no fetch metadata at all, such
			// as curl, which is not a browser and cannot be driven by a
			// visited page.
		default:
			http.Error(w, "cross-site requests to the console API are refused",
				http.StatusForbidden)
			return
		}
		// A cross-origin browser request also carries Origin; anything other
		// than our own is refused even if fetch metadata was stripped.
		if origin := r.Header.Get("Origin"); origin != "" {
			if !sameHost(origin, r.Host) {
				http.Error(w, "cross-origin requests to the console API are refused",
					http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameHost(origin, host string) bool {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
	return trimmed == host
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.status == nil {
		writeJSON(w, http.StatusOK, Status{})
		return
	}
	writeJSON(w, http.StatusOK, s.status(r.Context()))
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]string, 0, len(s.order))
	for _, id := range s.order {
		p := s.providers[id]
		out = append(out, map[string]string{"id": p.ID(), "title": p.Title()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("service")
	p, ok := s.providers[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("no such service %q", id),
		})
		return
	}
	project := r.URL.Query().Get("project")

	// A bounded read: a hung backend must surface as an error the screen can
	// show, not as a request that never returns.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	listing, err := p.List(ctx, project)
	if err != nil {
		// Reported as an unavailable listing rather than an HTTP error, so
		// the screen can render its error state with the cause instead of
		// showing an empty table.
		writeJSON(w, http.StatusOK, Listing{Unavailable: err.Error()})
		return
	}
	if listing.Items == nil {
		listing.Items = []Resource{}
	}
	if listing.Columns == nil {
		listing.Columns = []string{}
	}
	writeJSON(w, http.StatusOK, listing)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Start binds the listener and serves. It does not block.
func (s *Server) Start(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("bind console address %s: %w", s.addr, err)
	}

	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})

	s.mu.Lock()
	s.ln, s.srv, s.done = ln, srv, done
	s.mu.Unlock()

	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err
		}
	}()
	return nil
}

// Stop shuts the console down.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv, done := s.srv, s.done
	s.srv = nil
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return err
}
