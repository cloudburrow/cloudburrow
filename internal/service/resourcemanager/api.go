package resourcemanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	grpcx "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The Resource Manager v3 Projects API (#298), served from the Registry.
//
// One store: the console's project list and this API read and write the same
// records, so a project created in one is in the other. Projects, Folders and
// Organizations (#1294, hierarchy_api.go), and Liens and the tag services
// (#1295, tags_api.go) are served; the IAM methods are UNIMPLEMENTED, each
// saying why. A
// deleted project is kept for 30 days, and UndeleteProject restores it.

// ProjectsServer serves google.cloud.resourcemanager.v3.Projects.
type ProjectsServer struct {
	rmpb.UnimplementedProjectsServer
	reg *Registry
}

// NewProjectsServer returns the gRPC service over reg.
func NewProjectsServer(reg *Registry) *ProjectsServer { return &ProjectsServer{reg: reg} }

// V3Name is a project's v3 resource name, "projects/{number}", as Google's
// v3 reports it (#1211). The number is resource.ProjectNumber, the one v1's
// projectNumber, the metadata server's numeric-project-id and the service
// agents' emails carry. Requests may name a project either way.
func V3Name(id string) string {
	return "projects/" + strconv.FormatInt(projectNumber(id), 10)
}

func toProto(p Project) *rmpb.Project {
	out := &rmpb.Project{
		Name:        "projects/" + strconv.FormatInt(numberOf(p), 10),
		ProjectId:   p.ProjectID,
		DisplayName: p.DisplayName,
		Labels:      p.Labels,
		// A folder, the organization, or empty for a top-level project.
		Parent: p.Parent,
		State:  rmpb.Project_ACTIVE,
	}
	if p.State == StateDeleteRequested {
		out.State = rmpb.Project_DELETE_REQUESTED
	}
	if t, err := time.Parse(time.RFC3339, p.CreateTime); err == nil {
		out.CreateTime = timestamppb.New(t)
		out.UpdateTime = out.CreateTime
	}
	return out
}

// idFromName reads the project ID from "projects/{id}" or
// "projects/{number}". A project ID starts with a letter, so a name of digits
// alone is a number, and it names the registered project whose number it is.
func (s *ProjectsServer) idFromName(name string) (string, error) {
	id, ok := strings.CutPrefix(name, "projects/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", apierror.InvalidArgument("name %q must be projects/{project_id} or projects/{project_number}", name)
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return id, nil
	}
	projects, err := s.reg.List()
	if err != nil {
		return "", err
	}
	for _, p := range projects {
		if numberOf(p) == n {
			return p.ProjectID, nil
		}
	}
	return "", apierror.NotFound("project %s", name)
}

// doneOperation is a completed long-running operation carrying resp. Every
// change here happens before the call returns, so an operation that said
// otherwise would make the client poll for nothing.
func doneOperation(kind, id string, resp proto.Message) (*longrunningpb.Operation, error) {
	a, err := anypb.New(resp)
	if err != nil {
		return nil, apierror.Internal(err, "encode operation response")
	}
	op := &longrunningpb.Operation{
		Name:   fmt.Sprintf("operations/%s.%s.%d", kind, id, time.Now().UnixNano()),
		Done:   true,
		Result: &longrunningpb.Operation_Response{Response: a},
	}
	v3Operations.put(op)
	return op, nil
}

// splitMask is a REST updateMask query parameter as a field mask.
func splitMask(m string) *fieldmaskpb.FieldMask {
	if m == "" {
		return nil
	}
	return &fieldmaskpb.FieldMask{Paths: strings.Split(m, ",")}
}

func (s *ProjectsServer) GetProject(_ context.Context, req *rmpb.GetProjectRequest) (*rmpb.Project, error) {
	id, err := s.idFromName(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := s.reg.Get(id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProto(p), nil
}

func (s *ProjectsServer) ListProjects(_ context.Context, req *rmpb.ListProjectsRequest) (*rmpb.ListProjectsResponse, error) {
	// ListProjects requires a parent in Google's API. A top-level project
	// has none, so SearchProjects is how those are listed.
	parent := req.GetParent()
	if parent == "" {
		return nil, apierror.InvalidArgument("parent is required: folders/{id} or organizations/{id}; " +
			"top-level projects have no parent, so use SearchProjects to list them")
	}
	if err := s.reg.CheckParentExists(parent); err != nil {
		return nil, apierror.Wrap(err)
	}
	list := s.reg.Active
	if req.GetShowDeleted() {
		list = s.reg.List
	}
	all, err := list()
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	byID := map[string]Project{}
	var ids []string
	for _, p := range all {
		if p.Parent == parent {
			byID[p.ProjectID] = p
			ids = append(ids, p.ProjectID)
		}
	}
	page, next, err := paging.Page(fmt.Sprintf("projects.list:%s:%v", parent, req.GetShowDeleted()), ids, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	out := &rmpb.ListProjectsResponse{NextPageToken: next}
	for _, id := range page {
		out.Projects = append(out.Projects, toProto(byID[id]))
	}
	return out, nil
}

func (s *ProjectsServer) SearchProjects(_ context.Context, req *rmpb.SearchProjectsRequest) (*rmpb.SearchProjectsResponse, error) {
	match, err := parseQuery(req.GetQuery())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	// A project pending deletion is found only by a query naming its state,
	// as ListProjects returns one only with show_deleted (#1216).
	list := s.reg.Active
	if mentionsState(req.GetQuery()) {
		list = s.reg.List
	}
	all, err := list()
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	byID := map[string]Project{}
	var ids []string
	for _, p := range all {
		if match(p) {
			byID[p.ProjectID] = p
			ids = append(ids, p.ProjectID)
		}
	}
	// The query is the scope, so a token issued for one query is refused
	// for another rather than paging through a different result set.
	page, next, err := paging.Page("projects.search:"+req.GetQuery(), ids, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	out := &rmpb.SearchProjectsResponse{NextPageToken: next}
	for _, id := range page {
		out.Projects = append(out.Projects, toProto(byID[id]))
	}
	return out, nil
}

// mentionsState reports whether a query or filter has a state term.
func mentionsState(q string) bool {
	for _, term := range strings.Fields(q) {
		if i := strings.IndexAny(term, ":="); i > 0 {
			switch strings.ToLower(term[:i]) {
			case "state", "lifecyclestate":
				return true
			}
		}
	}
	return false
}

// parseQuery compiles a SearchProjects query: space-separated terms, each
// "key:value" or "key=value", all of which must match. Keys are id,
// projectId, name, displayName, state, parent and labels.KEY. A term with any
// other key, or OR, or a wildcard, is refused rather than ignored: ignoring
// part of a filter returns projects the caller excluded.
func parseQuery(q string) (func(Project) bool, error) {
	var preds []func(Project) bool
	for _, term := range strings.Fields(q) {
		if term == "OR" || term == "AND" || strings.ContainsAny(term, "*()") {
			return nil, apierror.InvalidArgument("query %q: only space-separated key:value terms are supported", q)
		}
		i := strings.IndexAny(term, ":=")
		if i <= 0 {
			return nil, apierror.InvalidArgument("query term %q must be key:value", term)
		}
		key, val := term[:i], strings.Trim(term[i+1:], `"`)
		switch {
		case key == "id" || key == "projectId":
			preds = append(preds, func(p Project) bool { return p.ProjectID == val })
		case key == "name":
			preds = append(preds, func(p Project) bool { return p.Name == val || p.ProjectID == val })
		case key == "displayName":
			preds = append(preds, func(p Project) bool { return p.DisplayName == val })
		case key == "state" || key == "lifecycleState":
			preds = append(preds, func(p Project) bool { return string(p.State) == strings.ToUpper(val) })
		case key == "parent":
			preds = append(preds, func(p Project) bool { return p.Parent == val })
		case key == "parent.type":
			preds = append(preds, func(p Project) bool { return strings.HasPrefix(p.Parent, val+"s/") })
		case key == "parent.id":
			preds = append(preds, func(p Project) bool { _, id, _ := strings.Cut(p.Parent, "/"); return p.Parent != "" && id == val })
		case strings.HasPrefix(key, "labels."):
			lk := strings.TrimPrefix(key, "labels.")
			preds = append(preds, func(p Project) bool { v, ok := p.Labels[lk]; return ok && v == val })
		default:
			return nil, apierror.InvalidArgument("query key %q is not supported", key)
		}
	}
	return func(p Project) bool {
		for _, f := range preds {
			if !f(p) {
				return false
			}
		}
		return true
	}, nil
}

func (s *ProjectsServer) CreateProject(_ context.Context, req *rmpb.CreateProjectRequest) (*longrunningpb.Operation, error) {
	in := req.GetProject()
	if in == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("project is required"))
	}
	p, err := s.reg.Create(Project{ProjectID: in.GetProjectId(), DisplayName: in.GetDisplayName(), Labels: in.GetLabels(), Parent: in.GetParent()})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("cp", p.ProjectID, toProto(p))
}

func (s *ProjectsServer) UpdateProject(_ context.Context, req *rmpb.UpdateProjectRequest) (*longrunningpb.Operation, error) {
	in := req.GetProject()
	if in == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("project is required"))
	}
	id, err := s.idFromName(in.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	cur, err := s.reg.Get(id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	displayName, labels, err := applyMask(cur, in, req.GetUpdateMask())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := s.reg.Update(id, displayName, labels)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("up", id, toProto(p))
}

// applyMask returns the display name and labels an update leaves. An empty
// mask means both, as Google documents; any other field is refused, because
// the rest of a project is identity or history, not settings.
func applyMask(cur Project, in *rmpb.Project, mask *fieldmaskpb.FieldMask) (string, map[string]string, error) {
	displayName, labels := cur.DisplayName, cur.Labels
	paths := mask.GetPaths()
	if len(paths) == 0 {
		paths = []string{"display_name", "labels"}
	}
	for _, p := range paths {
		switch p {
		case "display_name", "displayName":
			displayName = in.GetDisplayName()
		case "labels":
			labels = in.GetLabels()
		default:
			return "", nil, apierror.InvalidArgument("update_mask path %q cannot be updated; only display_name and labels", p)
		}
	}
	return displayName, labels, nil
}

func (s *ProjectsServer) DeleteProject(_ context.Context, req *rmpb.DeleteProjectRequest) (*longrunningpb.Operation, error) {
	id, err := s.idFromName(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	// Kept in DELETE_REQUESTED for RetentionPeriod, as Google keeps it, so
	// UndeleteProject can restore it (#1216).
	p, err := s.reg.Delete(id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("dp", id, toProto(p))
}

// UndeleteProject restores a project deleted within RetentionPeriod. One
// that is ACTIVE is FAILED_PRECONDITION; one purged is NOT_FOUND.
func (s *ProjectsServer) UndeleteProject(_ context.Context, req *rmpb.UndeleteProjectRequest) (*longrunningpb.Operation, error) {
	id, err := s.idFromName(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := s.reg.Undelete(id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("up", id, toProto(p))
}

// MoveProject places a project under a folder or the organization (#1294).
func (s *ProjectsServer) MoveProject(_ context.Context, req *rmpb.MoveProjectRequest) (*longrunningpb.Operation, error) {
	id, err := s.idFromName(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := s.reg.Move(id, req.GetDestinationParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("mp", id, toProto(p))
}

// --- REST --------------------------------------------------------------

// restRoutes registers the v3 JSON paths, each transcoded to the gRPC
// method, so the two surfaces cannot disagree.
func (s *ProjectsServer) restRoutes(r *rest.Router) {
	r.Handle("GET /v3/projects", s.restList)
	r.Handle("GET /v3/projects:search", func(w http.ResponseWriter, r *http.Request) error { return s.restSearch(w, r, "search") })
	r.Handle("POST /v3/projects", s.restCreate)
	r.Handle("GET /v3/projects/{project}", s.restGet)
	r.Handle("PATCH /v3/projects/{project}", s.restUpdate)
	r.Handle("DELETE /v3/projects/{project}", s.restDelete)
	r.Handle("POST /v3/projects/{project}", s.restVerb)
}

var pj = protojson.MarshalOptions{EmitUnpopulated: false}

func writeProto(w http.ResponseWriter, m proto.Message) error {
	b, err := pj.Marshal(m)
	if err != nil {
		return apierror.Internal(err, "encode response")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, err = w.Write(b)
	return err
}

func readProto(r *http.Request, m proto.Message) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return apierror.InvalidArgument("read body: %v", err)
	}
	if len(b) == 0 {
		return nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(b, m); err != nil {
		return apierror.InvalidArgument("decode body: %v", err)
	}
	return nil
}

// restList serves GET /v3/projects?parent=.
func (s *ProjectsServer) restList(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	size, err := rest.QueryInt(r, "pageSize", 0)
	if err != nil {
		return err
	}
	resp, err := s.ListProjects(r.Context(), &rmpb.ListProjectsRequest{Parent: q.Get("parent"), PageToken: q.Get("pageToken"),
		PageSize: int32(size), ShowDeleted: q.Get("showDeleted") == "true"})
	if err != nil {
		return err
	}
	return writeProto(w, resp)
}

func (s *ProjectsServer) restCreate(w http.ResponseWriter, r *http.Request) error {
	var p rmpb.Project
	if err := readProto(r, &p); err != nil {
		return err
	}
	op, err := s.CreateProject(r.Context(), &rmpb.CreateProjectRequest{Project: &p})
	if err != nil {
		return err
	}
	return writeProto(w, op)
}

func (s *ProjectsServer) restGet(w http.ResponseWriter, r *http.Request) error {
	raw, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	if strings.Contains(raw, ":") {
		return apierror.Unimplemented("custom method on %q is not implemented", raw)
	}
	p, err := s.GetProject(r.Context(), &rmpb.GetProjectRequest{Name: "projects/" + raw})
	if err != nil {
		return err
	}
	return writeProto(w, p)
}

func (s *ProjectsServer) restSearch(w http.ResponseWriter, r *http.Request, verb string) error {
	if verb != "search" {
		return apierror.Unimplemented("method %q is not implemented", verb)
	}
	q := r.URL.Query()
	size, err := rest.QueryInt(r, "pageSize", 0)
	if err != nil {
		return err
	}
	resp, err := s.SearchProjects(r.Context(), &rmpb.SearchProjectsRequest{
		Query: q.Get("query"), PageToken: q.Get("pageToken"), PageSize: int32(size)})
	if err != nil {
		return err
	}
	return writeProto(w, resp)
}

func (s *ProjectsServer) restUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	var p rmpb.Project
	if err := readProto(r, &p); err != nil {
		return err
	}
	p.Name = "projects/" + id
	mask := splitMask(r.URL.Query().Get("updateMask"))
	op, err := s.UpdateProject(r.Context(), &rmpb.UpdateProjectRequest{Project: &p, UpdateMask: mask})
	if err != nil {
		return err
	}
	return writeProto(w, op)
}

func (s *ProjectsServer) restDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	op, err := s.DeleteProject(r.Context(), &rmpb.DeleteProjectRequest{Name: "projects/" + id})
	if err != nil {
		return err
	}
	return writeProto(w, op)
}

// restVerb is POST /v3/projects/{project}:{verb}: undelete and move, and
// IAM, stored and not enforced (iam.go).
func (s *ProjectsServer) restVerb(w http.ResponseWriter, r *http.Request) error {
	raw, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	_, verb, _ := strings.Cut(raw, ":")
	switch verb {
	case "getIamPolicy", "setIamPolicy", "testIamPermissions":
		id, _, _ := strings.Cut(raw, ":")
		return serveIAM(w, r, s, "projects/"+id, verb)
	case "move":
		id, _, _ := strings.Cut(raw, ":")
		var in rmpb.MoveProjectRequest
		if err := readProto(r, &in); err != nil {
			return err
		}
		op, err := s.MoveProject(r.Context(), &rmpb.MoveProjectRequest{Name: "projects/" + id, DestinationParent: in.GetDestinationParent()})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	case "undelete":
		id, _, _ := strings.Cut(raw, ":")
		op, err := s.UndeleteProject(r.Context(), &rmpb.UndeleteProjectRequest{Name: "projects/" + id})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	default:
		return apierror.InvalidArgument("POST to a project requires a custom method")
	}
}

// --- server ------------------------------------------------------------

// Server serves the Projects API over gRPC and JSON on one port.
type Server struct {
	addr     string
	projects *ProjectsServer
	folders  *FoldersServer
	tags     *tagServers

	calls     grpcx.Observer
	requests  func(rest.Request)
	interpose []grpc.UnaryServerInterceptor
	options   []grpc.ServerOption

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	grpc *grpc.Server
	done chan struct{}
	// inflight counts gRPC calls being served. gRPC arrives over h2c, which
	// hijacks the connection, so http.Server.Shutdown does not wait for
	// them; Stop does, so a call (and its trace span, #1159) finishes
	// before the server is gone.
	inflight sync.WaitGroup
	stopping bool // set by Stop under mu; no call is counted after it
}

// NewServer returns a Resource Manager server over reg, bound to addr.
func NewServer(addr string, reg *Registry) *Server {
	return &Server{addr: addr, projects: NewProjectsServer(reg), folders: NewFoldersServer(reg), tags: newTagServers(reg)}
}

// Observe sets who is told about completed calls. It must be set before Start.
func (s *Server) Observe(calls grpcx.Observer, requests func(rest.Request)) {
	s.calls, s.requests = calls, requests
}

// Interpose adds a unary interceptor inside the call observer, such as the
// request log. It must be called before Start.
func (s *Server) Interpose(i grpc.UnaryServerInterceptor) {
	s.interpose = append(s.interpose, i)
}

// ServerOptions adds options to the gRPC server, such as tracing's stats
// handler. It must be called before Start.
func (s *Server) ServerOptions(opts ...grpc.ServerOption) {
	s.options = append(s.options, opts...)
}

func (s *Server) Name() string { return "resourcemanager" }

// Addr is the bound address, or "" before Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) Start(ctx context.Context) error {
	opts := append([]grpc.ServerOption(nil), s.options...)
	if s.calls != nil {
		opts = append(opts,
			grpc.ChainUnaryInterceptor(grpcx.UnaryObserver(s.calls)),
			grpc.ChainStreamInterceptor(grpcx.StreamObserver(s.calls)))
	}
	if len(s.interpose) > 0 {
		opts = append(opts, grpc.ChainUnaryInterceptor(s.interpose...))
	}
	g := grpc.NewServer(opts...)
	rmpb.RegisterProjectsServer(g, s.projects)
	rmpb.RegisterFoldersServer(g, s.folders)
	rmpb.RegisterOrganizationsServer(g, NewOrganizationsServer(s.projects.reg))
	rmpb.RegisterTagKeysServer(g, s.tags.keys)
	rmpb.RegisterTagValuesServer(g, s.tags.values)
	rmpb.RegisterTagBindingsServer(g, s.tags.bindings)
	rmpb.RegisterTagHoldsServer(g, s.tags.holds)
	longrunningpb.RegisterOperationsServer(g, OperationsServer{})

	router := rest.NewRouter()
	s.projects.restRoutes(router)
	ops := &v1Ops{}
	s.projects.v1Routes(router, ops)
	s.projects.v1OrgRoutes(router, NewOrganizationsServer(s.projects.reg))
	s.folders.restRoutes(router, NewOrganizationsServer(s.projects.reg), ops)
	s.folders.v2Routes(router, ops)
	s.tags.restRoutes(router)
	var jsonAPI http.Handler = router
	if s.requests != nil {
		jsonAPI = rest.Observe(router, s.requests)
	}
	both := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			s.mu.Lock()
			if s.stopping {
				s.mu.Unlock()
				http.Error(w, "Resource Manager is stopping", http.StatusServiceUnavailable)
				return
			}
			s.inflight.Add(1)
			s.mu.Unlock()
			defer s.inflight.Done()
			g.ServeHTTP(w, r)
			return
		}
		jsonAPI.ServeHTTP(w, r)
	})

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("bind Resource Manager address %s: %w", s.addr, err)
	}
	srv := &http.Server{Handler: h2c.NewHandler(hostguard.Wrap(both), &http2.Server{}), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	s.mu.Lock()
	s.ln, s.srv, s.grpc, s.done = ln, srv, g, done
	s.stopping = false
	s.mu.Unlock()
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err
		}
	}()
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv, g, done := s.srv, s.grpc, s.done
	s.srv = nil
	if srv != nil {
		s.stopping = true
	}
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	// Calls already being served finish first (see inflight), unless ctx
	// ends; then they are cut off with the rest.
	served := make(chan struct{})
	go func() { s.inflight.Wait(); close(served) }()
	select {
	case <-served:
	case <-ctx.Done():
	}
	if g != nil {
		g.Stop()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return err
}
