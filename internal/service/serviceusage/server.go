// Package serviceusage serves the Service Usage API v1
// (serviceusage.googleapis.com, #1352): which APIs a project has enabled.
//
// Terraform's google_project_service and `gcloud services` call it before
// almost anything else, so a module that enables its APIs first could not
// apply locally without it.
//
// The catalogue is what this instance serves, each API under its
// googleapis.com name. Every API in it starts ENABLED in every project, as
// a local instance serves all of them to every project. A project's changes
// from that are stored in the instance's store, so they persist exactly as
// the instance's other metadata does (in memory, or in the state directory).
//
// Enabling an API outside the catalogue is refused with the reason: either
// the instance was not started with the service that serves it, or
// CloudBurrow serves no such API. Answering ENABLED for an API nothing
// serves would let a module apply and then fail at its first call.
//
// Disabling an API in the catalogue is stored and reported (state DISABLED)
// but not enforced: the API keeps answering, as IAM policies are stored and
// not enforced (ADR-0006). docs/serviceusage.md says so.
//
// Every change happens before the call returns, so each operation is
// returned done, and operations.get answers it afterwards; operations.list
// lists them, cancel answers Empty and leaves a done one done, and delete
// forgets one (#1530).
package serviceusage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// API is one entry of the catalogue.
type API struct {
	// Name is the googleapis.com name, such as "run.googleapis.com".
	Name string
	// Title is the API's display name, as Google titles it.
	Title string
}

// Unserved is an API CloudBurrow can serve but this instance does not: the
// reason enabling it is refused, such as "start the instance with
// --services spanner".
type Unserved struct {
	Name   string
	Reason string
}

// Projects resolves the project a request names, by ID or by number.
type Projects interface {
	// Resolve returns the project ID for "projects/{id}" or
	// "projects/{number}"'s last segment.
	Resolve(idOrNumber string) (string, error)
}

// Server serves the Service Usage API v1 over HTTP.
type Server struct {
	addr     string
	catalog  []API
	byName   map[string]API
	unserved map[string]string
	st       store.Store
	projects Projects

	mu  sync.Mutex
	ops map[string]map[string]any
	// opOrder is ops' names in the order they were made, which
	// operations.list answers in.
	opOrder []string

	srv *http.Server
	ln  net.Listener
	// Observe, if set, is called once per request with the method name and
	// the answer's HTTP status.
	Observe func(method string, code int)
}

// NewServer serves catalog at addr once started. unserved names the APIs
// CloudBurrow could serve with another configuration, and why it does not
// now. st keeps each project's changes.
func NewServer(addr string, catalog []API, unserved []Unserved, st store.Store, projects Projects) *Server {
	s := &Server{addr: addr, st: st, projects: projects, byName: map[string]API{},
		unserved: map[string]string{}, ops: map[string]map[string]any{}}
	for _, a := range catalog {
		if _, dup := s.byName[a.Name]; !dup {
			s.byName[a.Name] = a
			s.catalog = append(s.catalog, a)
		}
	}
	sort.Slice(s.catalog, func(i, j int) bool { return s.catalog[i].Name < s.catalog[j].Name })
	for _, u := range unserved {
		if _, served := s.byName[u.Name]; !served {
			s.unserved[u.Name] = u.Reason
		}
	}
	return s
}

func (s *Server) Name() string { return "serviceusage" }

// Start binds the listener and serves in the background.
func (s *Server) Start(context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("serviceusage: listen on %s: %w", s.addr, err)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Stop shuts the server down.
func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// Addr is the bound address, empty before Start.
func (s *Server) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// apiError is an error with the HTTP status and Google status it answers.
type apiError struct {
	code   int
	status string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errorf(code int, status, format string, args ...any) error {
	return &apiError{code: code, status: status, msg: fmt.Sprintf(format, args...)}
}

// PermissionDenied is the 403 Google answers for a project it does not
// know, or one the caller cannot see.
func PermissionDenied(format string, args ...any) error {
	return errorf(http.StatusForbidden, "PERMISSION_DENIED", format, args...)
}

func invalid(format string, args ...any) error {
	return errorf(http.StatusBadRequest, "INVALID_ARGUMENT", format, args...)
}

// ServeHTTP routes /v1/....
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, out, err := s.route(r)
	if err != nil {
		s.fail(w, method, err)
		return
	}
	if s.Observe != nil {
		s.Observe(method, http.StatusOK)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) fail(w http.ResponseWriter, method string, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = &apiError{code: http.StatusInternalServerError, status: "INTERNAL", msg: err.Error()}
	}
	if s.Observe != nil {
		s.Observe(method, ae.code)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(ae.code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": ae.code, "message": ae.msg, "status": ae.status,
	}})
}

func (s *Server) route(r *http.Request) (string, any, error) {
	path, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok {
		return "", nil, errorf(http.StatusNotFound, "NOT_FOUND", "%s is not a Service Usage API v1 path", r.URL.Path)
	}
	// The custom verb follows a colon in the last segment.
	verb := ""
	if i := strings.LastIndex(path, ":"); i > strings.LastIndex(path, "/") {
		path, verb = path[:i], path[i+1:]
	}
	p := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(p) >= 2 && p[0] == "operations" && verb == "" && r.Method == http.MethodGet:
		out, err := s.operation(strings.Join(p, "/"))
		return "serviceusage.operations.get", out, err
	case len(p) == 1 && p[0] == "operations" && verb == "" && r.Method == http.MethodGet:
		out, err := s.listOperations(r.URL.Query())
		return "serviceusage.operations.list", out, err
	case len(p) >= 2 && p[0] == "operations" && verb == "cancel" && r.Method == http.MethodPost:
		// Every operation is done before it is returned, and cancelling a
		// done operation changes nothing (google.longrunning): the answer
		// is Empty, and operations.get still reports it done.
		if err := decode(r, &struct{}{}); err != nil {
			return "serviceusage.operations.cancel", nil, err
		}
		_, err := s.operation(strings.Join(p, "/"))
		if err != nil {
			return "serviceusage.operations.cancel", nil, err
		}
		return "serviceusage.operations.cancel", map[string]any{}, nil
	case len(p) >= 2 && p[0] == "operations" && verb == "" && r.Method == http.MethodDelete:
		err := s.DeleteOperation(strings.Join(p, "/"))
		if err != nil {
			return "serviceusage.operations.delete", nil, err
		}
		return "serviceusage.operations.delete", map[string]any{}, nil
	}
	// {projects|folders|organizations}/{id}/services[/{service}]
	if len(p) < 3 || p[2] != "services" || len(p) > 4 {
		return "", nil, errorf(http.StatusNotFound, "NOT_FOUND", "%s is not a Service Usage API v1 path", r.URL.Path)
	}
	if p[0] != "projects" {
		// Google's v1 reference documents only projects/{project} as a
		// consumer (Service.parent, ListServicesRequest.parent); the route
		// takes {parent=*/*}, but what it answers for a folder or an
		// organization is undocumented, so it is not guessed (#1395).
		return "", nil, errorf(http.StatusNotImplemented, "UNIMPLEMENTED",
			"Service Usage v1 documents only projects/{project} as a parent; %s is not one, and CloudBurrow does not guess what Google answers for it", p[0]+"/"+p[1])
	}
	project, err := s.project(p[1])
	if err != nil {
		return "", nil, err
	}
	if len(p) == 3 {
		switch {
		case verb == "" && r.Method == http.MethodGet:
			out, err := s.list(project, r.URL.Query())
			return "serviceusage.services.list", out, err
		case verb == "batchGet" && r.Method == http.MethodGet:
			out, err := s.batchGet(project, r.URL.Query()["names"])
			return "serviceusage.services.batchGet", out, err
		case verb == "batchEnable" && r.Method == http.MethodPost:
			var req struct {
				ServiceIDs []string `json:"serviceIds"`
			}
			if err := decode(r, &req); err != nil {
				return "serviceusage.services.batchEnable", nil, err
			}
			out, err := s.batchEnable(project, req.ServiceIDs)
			return "serviceusage.services.batchEnable", out, err
		}
		return "", nil, errorf(http.StatusNotFound, "NOT_FOUND", "%s %s is not a Service Usage API v1 method", r.Method, r.URL.Path)
	}
	svc := p[3]
	switch {
	case verb == "" && r.Method == http.MethodGet:
		out, err := s.get(project, svc)
		return "serviceusage.services.get", out, err
	case verb == "enable" && r.Method == http.MethodPost:
		if err := decode(r, &struct{}{}); err != nil {
			return "serviceusage.services.enable", nil, err
		}
		out, err := s.enable(project, svc)
		return "serviceusage.services.enable", out, err
	case verb == "disable" && r.Method == http.MethodPost:
		var req struct {
			DisableDependentServices bool   `json:"disableDependentServices"`
			CheckIfServiceHasUsage   string `json:"checkIfServiceHasUsage"`
		}
		if err := decode(r, &req); err != nil {
			return "serviceusage.services.disable", nil, err
		}
		switch req.CheckIfServiceHasUsage {
		case "", "CHECK_IF_SERVICE_HAS_USAGE_UNSPECIFIED", "SKIP", "CHECK":
		default:
			return "serviceusage.services.disable", nil, invalid("Invalid value at 'check_if_service_has_usage' (%q)", req.CheckIfServiceHasUsage)
		}
		out, err := s.disable(project, svc)
		return "serviceusage.services.disable", out, err
	}
	return "", nil, errorf(http.StatusNotFound, "NOT_FOUND", "%s %s is not a Service Usage API v1 method", r.Method, r.URL.Path)
}

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return invalid("Invalid JSON payload received: %v", err)
	}
	return nil
}

func (s *Server) project(idOrNumber string) (string, error) {
	if s.projects == nil {
		return idOrNumber, nil
	}
	return s.projects.Resolve(idOrNumber)
}

// states are a project's stored changes from the default, by API name.
type states map[string]string

const keyPrefix = "serviceusage/projects/"

func (s *Server) load(project string) (states, error) {
	out := states{}
	if s.st == nil {
		return out, nil
	}
	b, err := s.st.Get(keyPrefix + project)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("serviceusage: read %s: %w", project, err)
	}
	return out, nil
}

func (s *Server) save(project string, st states) error {
	if s.st == nil {
		return nil
	}
	if !store.SafeKey(keyPrefix + project) {
		return invalid("project %q cannot be stored", project)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.st.Put(keyPrefix+project, b)
}

// State is an API's state in a project: ENABLED unless it was disabled.
func (s *Server) state(st states, name string) string {
	if v := st[name]; v != "" {
		return v
	}
	return "ENABLED"
}

func parent(project string) string {
	return "projects/" + strconv.FormatInt(resource.ProjectNumber(project), 10)
}

func (s *Server) serviceJSON(project string, a API, state string) map[string]any {
	return map[string]any{
		"name":   parent(project) + "/services/" + a.Name,
		"parent": parent(project),
		"config": map[string]any{"name": a.Name, "title": a.Title},
		"state":  state,
	}
}

// lookup returns the catalogued API a request names, or why it cannot be
// served.
func (s *Server) lookup(name string) (API, error) {
	if a, ok := s.byName[name]; ok {
		return a, nil
	}
	if name == "" {
		return API{}, invalid("a service name is required")
	}
	if why, ok := s.unserved[name]; ok {
		return API{}, errorf(http.StatusBadRequest, "FAILED_PRECONDITION",
			"service %s is not served by this CloudBurrow instance: %s", name, why)
	}
	return API{}, errorf(http.StatusBadRequest, "FAILED_PRECONDITION",
		"service %s is not served by CloudBurrow, so it cannot be enabled locally (docs/serviceusage.md lists what is)", name)
}

// A service named in a get or batchGet is "projects/{p}/services/{s}".
func (s *Server) serviceFromName(project, name string) (string, error) {
	p := strings.Split(name, "/")
	if len(p) != 4 || p[0] != "projects" || p[2] != "services" {
		return "", invalid("name %q must be projects/{project}/services/{service}", name)
	}
	pr, err := s.project(p[1])
	if err != nil {
		return "", err
	}
	if pr != project {
		return "", invalid("service %s is not in the parent project %s", name, project)
	}
	return p[3], nil
}

func (s *Server) get(project, svc string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	a, ok := s.byName[svc]
	if !ok {
		// Google answers any real API as DISABLED; one nothing here serves
		// is never ENABLED, and is described as it is named.
		if _, known := s.unserved[svc]; known {
			return s.serviceJSON(project, API{Name: svc}, "DISABLED"), nil
		}
		return nil, errorf(http.StatusForbidden, "PERMISSION_DENIED",
			"Not found or permission denied for service(s): %s (CloudBurrow does not serve it)", svc)
	}
	return s.serviceJSON(project, a, s.state(st, a.Name)), nil
}

const (
	defaultPageSize = 50
	maxPageSize     = 200
	maxBatchGet     = 30
	maxBatchEnable  = 20
)

func (s *Server) list(project string, q url.Values) (any, error) {
	want := ""
	switch f := strings.TrimSpace(q.Get("filter")); f {
	case "":
	case "state:ENABLED", "state:DISABLED":
		want = strings.TrimPrefix(f, "state:")
	default:
		return nil, invalid("Invalid filter %q: only state:ENABLED and state:DISABLED are supported", f)
	}
	size := defaultPageSize
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, invalid("Invalid pageSize %q", v)
		}
		if n > 0 {
			size = min(n, maxPageSize)
		}
	}
	start := 0
	if tok := q.Get("pageToken"); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 0 {
			return nil, invalid("Invalid pageToken %q", tok)
		}
		start = n
	}
	s.mu.Lock()
	st, err := s.load(project)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var all []any
	for _, a := range s.catalog {
		state := s.state(st, a.Name)
		if want == "" || state == want {
			all = append(all, s.serviceJSON(project, a, state))
		}
	}
	out := map[string]any{}
	if start < len(all) {
		end := min(start+size, len(all))
		out["services"] = all[start:end]
		if end < len(all) {
			out["nextPageToken"] = strconv.Itoa(end)
		}
	}
	return out, nil
}

func (s *Server) batchGet(project string, names []string) (any, error) {
	if len(names) == 0 {
		return nil, invalid("names is required")
	}
	if len(names) > maxBatchGet {
		return nil, invalid("at most %d names may be requested at once, got %d", maxBatchGet, len(names))
	}
	out := []any{}
	for _, n := range names {
		svc, err := s.serviceFromName(project, n)
		if err != nil {
			return nil, err
		}
		v, err := s.get(project, svc)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return map[string]any{"services": out}, nil
}

// set changes the state of every API in names, or none of them: Google's
// batchEnable fails as a whole when any service in it would.
func (s *Server) set(project string, names []string, state string) ([]any, error) {
	apis := make([]API, 0, len(names))
	for _, n := range names {
		a, err := s.lookup(n)
		if err != nil {
			return nil, err
		}
		apis = append(apis, a)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	for _, a := range apis {
		if state == "ENABLED" {
			delete(st, a.Name)
		} else {
			st[a.Name] = state
		}
	}
	if err := s.save(project, st); err != nil {
		return nil, err
	}
	out := make([]any, len(apis))
	for i, a := range apis {
		out[i] = s.serviceJSON(project, a, state)
	}
	return out, nil
}

const typePrefix = "type.googleapis.com/google.api.serviceusage.v1."

func (s *Server) enable(project, svc string) (any, error) {
	out, err := s.set(project, []string{svc}, "ENABLED")
	if err != nil {
		return nil, err
	}
	return s.done(project, svc, "EnableServiceResponse", map[string]any{"service": out[0]}), nil
}

func (s *Server) batchEnable(project string, ids []string) (any, error) {
	if len(ids) == 0 {
		return nil, invalid("serviceIds is required")
	}
	if len(ids) > maxBatchEnable {
		return nil, invalid("at most %d services may be enabled at once, got %d", maxBatchEnable, len(ids))
	}
	out, err := s.set(project, ids, "ENABLED")
	if err != nil {
		return nil, err
	}
	return s.done(project, strings.Join(ids, ","), "BatchEnableServicesResponse", map[string]any{"services": out}), nil
}

func (s *Server) disable(project, svc string) (any, error) {
	if _, ok := s.byName[svc]; !ok {
		// Not enabled, as nothing outside the catalogue ever is: Google
		// answers a disable of a disabled service with a done operation.
		if _, known := s.unserved[svc]; known {
			return s.done(project, svc, "DisableServiceResponse",
				map[string]any{"service": s.serviceJSON(project, API{Name: svc}, "DISABLED")}), nil
		}
		_, err := s.lookup(svc)
		return nil, err
	}
	out, err := s.set(project, []string{svc}, "DISABLED")
	if err != nil {
		return nil, err
	}
	return s.done(project, svc, "DisableServiceResponse", map[string]any{"service": out[0]}), nil
}

// done records a finished operation and returns it.
func (s *Server) done(project, target, respType string, resp map[string]any) map[string]any {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	name := fmt.Sprintf("operations/acat.p2-%d-%s-%s-%s-%s-%s", resource.ProjectNumber(project),
		id[:8], id[8:12], id[12:16], id[16:20], id[20:])
	r := map[string]any{"@type": typePrefix + respType}
	for k, v := range resp {
		r[k] = v
	}
	op := map[string]any{
		"name": name,
		"metadata": map[string]any{"@type": typePrefix + "OperationMetadata",
			"resourceNames": []string{parent(project) + "/services/" + target}},
		"done":     true,
		"response": r,
	}
	s.mu.Lock()
	s.ops[name] = op
	s.opOrder = append(s.opOrder, name)
	s.mu.Unlock()
	return op
}

// listOperations is operations.list (#1530): every operation this instance
// returned and nobody deleted, oldest first, paged as services.list is.
// Google documents filter and name without a grammar for this API, so a
// filter, or a name other than "operations", is refused saying so rather
// than guessed.
func (s *Server) listOperations(q url.Values) (any, error) {
	if f := strings.TrimSpace(q.Get("filter")); f != "" {
		return nil, errorf(http.StatusNotImplemented, "UNIMPLEMENTED",
			"operations.list filter %q is not supported: Google documents no filter grammar for Service Usage operations; list without one", f)
	}
	if n := q.Get("name"); n != "" && n != "operations" {
		return nil, errorf(http.StatusNotImplemented, "UNIMPLEMENTED",
			"operations.list name %q is not supported: only \"operations\", every operation, is", n)
	}
	size := defaultPageSize
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, invalid("Invalid pageSize %q", v)
		}
		if n > 0 {
			size = min(n, maxPageSize)
		}
	}
	start := 0
	if tok := q.Get("pageToken"); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 0 {
			return nil, invalid("Invalid pageToken %q", tok)
		}
		start = n
	}
	all := s.Operations()
	out := map[string]any{}
	if start < len(all) {
		end := min(start+size, len(all))
		page := make([]any, 0, end-start)
		for _, op := range all[start:end] {
			page = append(page, op)
		}
		out["operations"] = page
		if end < len(all) {
			out["nextPageToken"] = strconv.Itoa(end)
		}
	}
	return out, nil
}

// Operations is every kept operation, oldest first, for operations.list
// and the console.
func (s *Server) Operations() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.opOrder))
	for _, n := range s.opOrder {
		out = append(out, s.ops[n])
	}
	return out
}

// ProjectOperations is the kept operations that changed project's APIs,
// oldest first, for the console.
func (s *Server) ProjectOperations(project string) []map[string]any {
	prefix := parent(project) + "/services/"
	var out []map[string]any
	for _, op := range s.Operations() {
		md, _ := op["metadata"].(map[string]any)
		names, _ := md["resourceNames"].([]string)
		if len(names) > 0 && strings.HasPrefix(names[0], prefix) {
			out = append(out, op)
		}
	}
	return out
}

// DeleteOperation is operations.delete (#1530): the operation is forgotten,
// so operations.get and operations.list no longer answer it. The change it
// made stays.
func (s *Server) DeleteOperation(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ops[name]; !ok {
		return errorf(http.StatusNotFound, "NOT_FOUND", "Operation %s not found", name)
	}
	delete(s.ops, name)
	for i, n := range s.opOrder {
		if n == name {
			s.opOrder = append(s.opOrder[:i], s.opOrder[i+1:]...)
			break
		}
	}
	return nil
}

func (s *Server) operation(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.ops[name]; ok {
		return op, nil
	}
	return nil, errorf(http.StatusNotFound, "NOT_FOUND", "Operation %s not found", name)
}

// Catalog returns the served APIs, sorted by name.
func (s *Server) Catalog() []API { return append([]API(nil), s.catalog...) }

// Services is what a project's APIs read, for the console: every
// catalogued API with its state.
func (s *Server) Services(project string) ([]map[string]any, error) {
	out, err := s.list(project, url.Values{"pageSize": {strconv.Itoa(maxPageSize)}})
	if err != nil {
		return nil, err
	}
	var list []map[string]any
	if v, ok := out.(map[string]any)["services"].([]any); ok {
		for _, e := range v {
			list = append(list, e.(map[string]any))
		}
	}
	return list, nil
}

// SetState enables or disables one API in a project, for the console.
func (s *Server) SetState(project, svc string, enabled bool) error {
	var err error
	if enabled {
		_, err = s.enable(project, svc)
	} else {
		_, err = s.disable(project, svc)
	}
	return err
}

// Method is one method of the v1 discovery document: its ID, HTTP method
// and path, and whether it is served.
type Method struct {
	ID          string
	HTTPMethod  string
	Path        string
	Implemented bool
}

// Methods is every method of serviceusage v1. The coverage registry reads
// it, and a unit test calls each one, so a method moves to implemented in
// the change that serves it.
var Methods = []Method{
	{"serviceusage.operations.cancel", "POST", "v1/operations/x:cancel", true},
	{"serviceusage.operations.delete", "DELETE", "v1/operations/x", true},
	{"serviceusage.operations.get", "GET", "v1/operations/x", true},
	{"serviceusage.operations.list", "GET", "v1/operations", true},
	{"serviceusage.services.batchEnable", "POST", "v1/projects/p/services:batchEnable", true},
	{"serviceusage.services.batchGet", "GET", "v1/projects/p/services:batchGet", true},
	{"serviceusage.services.disable", "POST", "v1/projects/p/services/s:disable", true},
	{"serviceusage.services.enable", "POST", "v1/projects/p/services/s:enable", true},
	{"serviceusage.services.get", "GET", "v1/projects/p/services/s", true},
	{"serviceusage.services.list", "GET", "v1/projects/p/services", true},
}

// MethodStatuses maps each method ID to whether it is implemented.
func MethodStatuses() map[string]bool {
	out := make(map[string]bool, len(Methods))
	for _, m := range Methods {
		out[m.ID] = m.Implemented
	}
	return out
}
