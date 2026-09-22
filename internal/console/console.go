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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	// Actions are the operations available on this resource.
	Actions []Action `json:"actions,omitempty"`
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
	// Prompt means the screen needs something from the user before it can
	// show anything — a project, most often.
	//
	// It is deliberately not Unavailable. "Choose a project" is a
	// precondition, not a failure, and rendering it as a red error taught the
	// user that a working instance was broken. The distinction is the same one
	// the empty state makes: nothing here yet is not the same as cannot read.
	Prompt string `json:"prompt,omitempty"`
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

// Field describes one input on a create form.
//
// The form is described by the backend rather than hard-coded in the client
// so that a field only ever appears when the service behind it can actually
// accept it — a form offering something the API refuses is the working-looking
// control the parity specification forbids.
type Field struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Help     string `json:"help,omitempty"`
	// Pattern is the constraint the API itself enforces, so the form refuses
	// what the API would refuse rather than letting a round trip do it.
	Pattern string `json:"pattern,omitempty"`
	Default string `json:"default,omitempty"`
}

// Creator is a provider whose resources can be created from the console.
//
// A provider that does not implement it gets no create button, which is how
// an unsupported operation stays absent rather than disabled-and-mysterious.
type Creator interface {
	// CreateForm describes the form, and its submit label. The label matches
	// the console's own wording — "Create", "Create topic", "Create queue".
	CreateForm() (label string, fields []Field)
	// Create makes the resource and returns its name.
	Create(ctx context.Context, project string, values map[string]string) (string, error)
}

// Deleter is a provider whose resources can be deleted from the console.
type Deleter interface {
	// Delete removes one resource by the name List reported.
	Delete(ctx context.Context, project, name string) error
}

// Actor is a provider with named per-resource actions, such as pausing a
// queue.
type Actor interface {
	// Actions returns the actions available on a resource, by id and label.
	// Returning none means the resource has no actions, not that the screen
	// should invent some.
	Actions(resource Resource) []Action
	// Act performs one.
	Act(ctx context.Context, project, name, action string) error
}

// Action is one named operation on a resource.
type Action struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Destructive marks an action that discards data, so the client can
	// confirm it and name what is about to be affected.
	Destructive bool `json:"destructive,omitempty"`
}

// Status is what the dashboard reports about the instance.
type Status struct {
	Instance string `json:"instance"`
	// DefaultProject is the project the console selects when the URL names
	// none.
	//
	// Without it the picker opened on "All projects", and the three services
	// that list per project answered with an error apiece — so a fresh
	// console's first screen was a wall of red on an instance that was
	// working perfectly. A console always has a project selected; that is
	// what the picker is for.
	DefaultProject string            `json:"defaultProject,omitempty"`
	Ready          bool              `json:"ready"`
	State          string            `json:"state"`
	Cluster        string            `json:"cluster"`
	Kubernetes     string            `json:"kubernetes,omitempty"`
	Namespace      string            `json:"namespace"`
	Mode           string            `json:"mode"`
	Services       []ServiceStatus   `json:"services"`
	Endpoints      map[string]string `json:"endpoints"`
	Components     map[string]bool   `json:"components,omitempty"`
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
	logs      *Recorder
	// playground is never nil; an unconfigured one reports that local AI is
	// off rather than making every call site check.
	playground *Playground

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	done chan struct{}
}

// New returns a console server bound to addr.
func New(addr string, status StatusSource, providers ...Provider) *Server {
	s := &Server{
		addr: addr, providers: map[string]Provider{}, status: status,
		logs:       NewRecorder(DefaultLogLimit, nil),
		playground: &Playground{},
	}
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

// Logs exposes the recorder, so the process can feed it what it observes.
func (s *Server) Logs() *Recorder { return s.logs }

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
	mux.HandleFunc("POST /api/resources/{service}", s.handleCreate)
	mux.HandleFunc("DELETE /api/resources/{service}", s.handleDelete)
	mux.HandleFunc("POST /api/actions/{service}", s.handleAction)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/operations", s.handleOperations)
	mux.HandleFunc("GET /api/ai/playground", s.handlePlayground)
	mux.HandleFunc("POST /api/ai/playground", s.handlePlaygroundGenerate)
	mux.HandleFunc("GET /api/stream", s.handleStream)

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
	out := make([]map[string]any, 0, len(s.order))
	for _, id := range s.order {
		p := s.providers[id]
		caps := s.capabilitiesOf(p)
		out = append(out, map[string]any{
			"id": p.ID(), "title": p.Title(),
			"create": caps.Create, "delete": caps.Delete,
		})
	}
	// The playground is advertised only when local AI is configured, so the
	// navigation never offers a screen that cannot work. It is not a
	// provider — it has no listing — so it is appended rather than being
	// forced into the provider interface.
	if s.playground.Configured() {
		out = append(out, map[string]any{
			"id": "playground", "title": "AI Playground", "create": false, "delete": false,
		})
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
		// showing an empty table. The collections are still empty arrays
		// rather than null, so a client that iterates before checking does
		// not fall over on top of the failure it was about to report.
		writeJSON(w, http.StatusOK, Listing{
			Unavailable: userMessage(err),
			Columns:     []string{}, Items: []Resource{},
		})
		return
	}
	if listing.Items == nil {
		listing.Items = []Resource{}
	}
	if listing.Columns == nil {
		listing.Columns = []string{}
	}
	// Per-resource actions are attached here rather than by the client, so
	// an action only appears when the provider actually offers it.
	if actor, ok := p.(Actor); ok {
		for i := range listing.Items {
			listing.Items[i].Actions = actor.Actions(listing.Items[i])
		}
	}
	writeJSON(w, http.StatusOK, listing)
}

// userMessage renders an error for a screen.
//
// A gRPC error stringifies as `rpc error: code = AlreadyExists desc = Topic
// already exists`, which puts the transport in front of the thing the
// developer needs to read. The code still matters — it says whether this is
// their mistake or ours — so it is kept and the envelope is dropped.
func userMessage(err error) string {
	if err == nil {
		return ""
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown && st.Code() != codes.OK {
		return fmt.Sprintf("%s: %s", st.Code(), st.Message())
	}
	return err.Error()
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

// capabilities describes what a provider supports, so the client renders only
// controls that will work.
type capabilities struct {
	Create *createForm `json:"create,omitempty"`
	Delete bool        `json:"delete,omitempty"`
}

type createForm struct {
	Label  string  `json:"label"`
	Fields []Field `json:"fields"`
}

func (s *Server) capabilitiesOf(p Provider) capabilities {
	var c capabilities
	if creator, ok := p.(Creator); ok {
		label, fields := creator.CreateForm()
		c.Create = &createForm{Label: label, Fields: fields}
	}
	if _, ok := p.(Deleter); ok {
		c.Delete = true
	}
	return c
}

// handleCreate creates a resource through the provider's own API.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	creator, ok := p.(Creator)
	if !ok {
		// Unimplemented rather than a generic error: the service exists and
		// creation is simply not offered for it.
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " cannot be created from the console",
		})
		return
	}

	var values map[string]string
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&values); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "malformed request: " + err.Error(),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	project := r.URL.Query().Get("project")
	opID := s.logs.StartOperation("create", p.Title(), project)

	name, err := creator.Create(ctx, project, values)
	if err != nil {
		// The verdict comes from the backend, never from the console's own
		// optimism: an operation is not successful because a call returned.
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		s.logs.Log(Entry{
			Severity: SeverityError, Source: p.ID(), Project: project,
			OperationID: opID, Message: "create failed: " + userMessage(err),
		})
		// The API's own message reaches the screen. A generic "could not
		// create" hides the constraint the caller actually violated.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{
		Severity: SeverityInfo, Source: p.ID(), Project: project,
		Resource: name, OperationID: opID, Message: "created " + name,
	})
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "operation": opID})
}

// handleDelete removes a resource.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	deleter, ok := p.(Deleter)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " cannot be deleted from the console",
		})
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "name is required; refusing to delete without one",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	project := r.URL.Query().Get("project")
	opID := s.logs.StartOperation("delete", name, project)

	if err := deleter.Delete(ctx, project, name); err != nil {
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		s.logs.Log(Entry{
			Severity: SeverityError, Source: p.ID(), Project: project, Resource: name,
			OperationID: opID, Message: "delete failed: " + userMessage(err),
		})
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{
		Severity: SeverityInfo, Source: p.ID(), Project: project, Resource: name,
		OperationID: opID, Message: "deleted " + name,
	})
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name, "operation": opID})
}

// handleAction performs a named per-resource action.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	actor, ok := p.(Actor)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": p.Title() + " has no actions",
		})
		return
	}

	var req struct{ Name, Action string }
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Name == "" || req.Action == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "name and action are both required",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if err := actor.Act(ctx, r.URL.Query().Get("project"), req.Name, req.Action); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"applied": req.Action})
}

// SetPlayground configures the local AI playground.
//
// Absent configuration the screen is not offered at all, which is the
// requirement: missing AI must not break the console.
func (s *Server) SetPlayground(p *Playground) {
	if p == nil {
		p = &Playground{}
	}
	s.playground = p
}
