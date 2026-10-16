package run

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The Cloud Run v1 Admin API (#1293): Knative Serving's own shape, which
// `gcloud run services list`, `describe` and `revisions list` call.
//
// v1 is served from the same Knative objects v2 reads, through the v2
// mapping (FromKnative, FromKnativeRevision) and, for writes, the v2 server
// itself (CreateService, UpdateService, DeleteService, DeleteRevision), so
// the two versions cannot disagree about what a service is or refuse
// different configuration. Both of Google's path styles are served:
//
//	/apis/serving.knative.dev/v1/namespaces/{project}/{collection}[/{name}]
//	/v1/projects/{project}/locations/{location}/{collection}[/{name}]
//
// for services (list, get, create, replace, delete), revisions (list, get,
// delete), routes and configurations (list, get); and, in v1global.go,
// projects.locations.list and the services' IAM methods (#1349), and, in
// v1jobs.go, jobs (list, get, create, delete), and in v1domains.go domain
// mappings and authorized domains. Everything else under /apis/ and /v1/
// (replaceJob, run, executions, tasks) answers 501 UNIMPLEMENTED.

// v1LocationLabel is the label Cloud Run v1 reports a resource's region in.
const v1LocationLabel = "cloud.googleapis.com/location"

// v1Autoscaling are the template annotations v1 carries scaling in.
const (
	v1MinScale = "autoscaling.knative.dev/minScale"
	// The tool and version that wrote a service, which gcloud sets on the
	// service and on each revision; v2's client and client_version.
	v1ClientName    = "run.googleapis.com/client-name"
	v1ClientVersion = "run.googleapis.com/client-version"
	// v2's container resources.startup_cpu_boost, which gcloud run deploy
	// sets. Locally no CPU is throttled at startup, so the boost holds.
	v1StartupCPUBoost = "run.googleapis.com/startup-cpu-boost"
	v1MaxScale        = "autoscaling.knative.dev/maxScale"
	// The source build `gcloud run deploy --source` deployed (#1347).
	v1BuildImageURI       = "run.googleapis.com/build-image-uri"
	v1BuildName           = "run.googleapis.com/build-name"
	v1BuildSourceLocation = "run.googleapis.com/build-source-location"
	v1BuildAutoUpdates    = "run.googleapis.com/build-enable-automatic-updates"
	v1BuildID             = "run.googleapis.com/build-id"
	v1BuildFunctionTarget = "run.googleapis.com/build-function-target"
	v1BuildBaseImage      = "run.googleapis.com/build-base-image"
)

// regionalHostRE is a regional endpoint's host, {region}-{host}: gcloud
// derives it from the run endpoint override for a command given --region.
var regionalHostRE = regexp.MustCompile(`^([a-z]+-[a-z]+[0-9]+)-`)

// V1Handler wraps the v2 JSON API with the v1 API: requests under /apis/
// and /v1/ are v1's, and everything else goes to next.
func (s *Server) V1Handler(next http.Handler) http.Handler {
	v1 := http.NewServeMux()
	for _, p := range []string{
		"/apis/serving.knative.dev/v1/namespaces/{ns}/",
		"/v1/projects/{ns}/locations/{loc}/",
	} {
		v1.HandleFunc("GET "+p+"services", s.v1ListServices)
		v1.HandleFunc("POST "+p+"services", s.v1CreateService)
		v1.HandleFunc("GET "+p+"services/{name}", s.v1GetService)
		v1.HandleFunc("PUT "+p+"services/{name}", s.v1ReplaceService)
		v1.HandleFunc("DELETE "+p+"services/{name}", s.v1DeleteService)
		v1.HandleFunc("GET "+p+"revisions", s.v1ListRevisions)
		v1.HandleFunc("GET "+p+"revisions/{name}", s.v1GetRevision)
		v1.HandleFunc("DELETE "+p+"revisions/{name}", s.v1DeleteRevision)
		v1.HandleFunc("GET "+p+"routes", s.v1ListRoutes)
		v1.HandleFunc("GET "+p+"routes/{name}", s.v1GetRoute)
		v1.HandleFunc("GET "+p+"configurations", s.v1ListConfigurations)
		v1.HandleFunc("GET "+p+"configurations/{name}", s.v1GetConfiguration)
	}
	for _, p := range []string{
		"/apis/domains.cloudrun.com/v1/namespaces/{ns}/domainmappings",
		"/v1/projects/{ns}/locations/{loc}/domainmappings",
	} {
		v1.HandleFunc("GET "+p, s.v1ListDomainMappings)
		v1.HandleFunc("POST "+p, s.v1CreateDomainMapping)
		v1.HandleFunc("GET "+p+"/{name}", s.v1GetDomainMapping)
		v1.HandleFunc("DELETE "+p+"/{name}", s.v1DeleteDomainMapping)
	}
	v1.HandleFunc("GET /v1/projects/{ns}/locations", s.v1Locations)
	v1.HandleFunc("GET /v1/projects/{ns}/authorizeddomains", s.v1AuthorizedDomains)
	v1.HandleFunc("GET /apis/domains.cloudrun.com/v1/namespaces/{ns}/authorizeddomains", s.v1AuthorizedDomains)
	jobs := "/apis/run.googleapis.com/v1/namespaces/{ns}/jobs"
	v1.HandleFunc("GET "+jobs, s.v1ListJobs)
	v1.HandleFunc("POST "+jobs, s.v1CreateJob)
	v1.HandleFunc("GET "+jobs+"/{name}", s.v1GetJob)
	v1.HandleFunc("DELETE "+jobs+"/{name}", s.v1DeleteJob)
	v1.HandleFunc("PUT "+jobs+"/{name}", s.v1ReplaceJob)
	v1.HandleFunc("POST "+jobs+"/{name}", s.v1RunJob)
	execs := "/apis/run.googleapis.com/v1/namespaces/{ns}/executions"
	v1.HandleFunc("GET "+execs, s.v1ListExecutions)
	v1.HandleFunc("GET "+execs+"/{name}", s.v1GetExecution)
	v1.HandleFunc("DELETE "+execs+"/{name}", s.v1DeleteExecution)
	v1.HandleFunc("POST "+execs+"/{name}", s.v1CancelExecution)
	tasks := "/apis/run.googleapis.com/v1/namespaces/{ns}/tasks"
	v1.HandleFunc("GET "+tasks, s.v1ListTasks)
	v1.HandleFunc("GET "+tasks+"/{name}", s.v1GetTask)
	v1.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rest.WriteError(w, apierror.Unimplemented("%s %s is not served by the Cloud Run v1 API locally", r.Method, r.URL.Path))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A custom method is part of its {name} segment, which a pattern
		// cannot split, so the IAM methods are matched before the mux.
		if m := v1IAMRE.FindStringSubmatch(r.URL.Path); m != nil {
			s.v1IAM(w, r, m)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/apis/") || strings.HasPrefix(r.URL.Path, "/v1/") {
			v1.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// v1Scope is where a v1 request is addressed: its project, and its location
// when the path or a regional host names one.
type v1Scope struct {
	project, location string
	// knative is the /apis/serving.knative.dev/v1 path style.
	knative bool
}

func scopeOf(r *http.Request) v1Scope {
	sc := v1Scope{project: r.PathValue("ns"), location: r.PathValue("loc"),
		knative: strings.HasPrefix(r.URL.Path, "/apis/")}
	if sc.location == "-" {
		sc.location = ""
	}
	if sc.location == "" {
		if m := regionalHostRE.FindStringSubmatch(r.Host); m != nil {
			sc.location = m[1]
		}
	}
	return sc
}

// matches reports whether a v2 resource name is in the scope.
func (sc v1Scope) matches(name string) bool {
	project, location := projectOf(name), locationOf(name)
	return project == sc.project && (sc.location == "" || location == sc.location)
}

// resourceName checks a {name} path segment: a custom method such as
// :getIamPolicy is not one of the v1 methods served.
func resourceName(r *http.Request) (string, error) {
	n := r.PathValue("name")
	if strings.Contains(n, ":") {
		return "", apierror.Unimplemented("%s %s is not served by the Cloud Run v1 API locally", r.Method, r.URL.Path)
	}
	return n, nil
}

func locationOf(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) >= 4 && parts[2] == "locations" {
		return parts[3]
	}
	return ""
}

// v1Services are the services in a scope, as v2 reads them, with the
// Knative Service each was read from.
func (s *Server) v1Services(ctx context.Context, sc v1Scope) ([]*runpb.Service, map[string]ksvc, error) {
	items, err := s.kn.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out []*runpb.Service
	raw := map[string]ksvc{}
	for _, k := range items {
		svc := FromKnative(k, fmt.Sprintf("projects/%s/locations/%s", sc.project, sc.location))
		if !sc.matches(svc.GetName()) {
			continue
		}
		out = append(out, svc)
		raw[svc.GetName()] = k
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, raw, nil
}

// v1Service reads one service in a scope.
//
// With a region its Knative Service is found as v2 finds it, scoped or under
// an older version's name (#1400); without one (the global endpoint) it is
// the scope's service of that ID in whichever region.
func (s *Server) v1Service(ctx context.Context, sc v1Scope, id string) (*runpb.Service, ksvc, error) {
	parent := fmt.Sprintf("projects/%s/locations/%s", sc.project, sc.location)
	if sc.location != "" {
		if k, err := s.service(ctx, parent+"/services/"+id, id); err == nil {
			if svc := FromKnative(k, parent); sc.matches(svc.GetName()) {
				return svc, k, nil
			}
		}
		return nil, ksvc{}, apierror.NotFound("services.serving.knative.dev %q not found", id)
	}
	svcs, raw, err := s.v1Services(ctx, sc)
	if err != nil {
		return nil, ksvc{}, err
	}
	for _, svc := range svcs {
		if sid, _ := ServiceID(svc.GetName()); sid == id {
			return svc, raw[svc.GetName()], nil
		}
	}
	return nil, ksvc{}, apierror.NotFound("services.serving.knative.dev %q not found", id)
}

// v1Page pages names with v1's limit and continue parameters.
func v1Page(r *http.Request, scope string, names []string) ([]string, string, error) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, next, err := paging.Page(scope, names, r.URL.Query().Get("continue"), limit)
	if err != nil {
		return nil, "", apierror.InvalidArgument("%v", err)
	}
	return page, next, nil
}

// labelSelector is v1's labelSelector query parameter: comma-separated
// key=value (or key==value) equalities, spaces allowed around the sign, which
// is what gcloud sends. Set-based
// and inequality selectors are refused rather than ignored.
func labelSelector(r *http.Request) (map[string]string, error) {
	sel := r.URL.Query().Get("labelSelector")
	out := map[string]string{}
	if sel == "" {
		return out, nil
	}
	for _, term := range strings.Split(sel, ",") {
		// gcloud sends "key = value".
		k, v, ok := strings.Cut(term, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(strings.TrimPrefix(v, "="))
		if !ok || k == "" || strings.HasSuffix(k, "!") || strings.ContainsAny(k+v, " ()!") {
			return nil, apierror.Unimplemented("labelSelector %q: only key=value equalities are supported", sel)
		}
		out[k] = v
	}
	if r.URL.Query().Get("fieldSelector") != "" {
		return nil, apierror.Unimplemented("fieldSelector is not supported")
	}
	return out, nil
}

func selected(labels, sel map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func v1Write(w http.ResponseWriter, err error, body any) {
	if err != nil {
		rest.WriteError(w, err)
		return
	}
	_ = rest.WriteJSON(w, http.StatusOK, body)
}

func (s *Server) v1ListServices(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	sel, err := labelSelector(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	svcs, raw, err := s.v1Services(r.Context(), sc)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	byName := map[string]*runv1.Service{}
	var names []string
	for _, svc := range svcs {
		v := toV1Service(svc, raw[svc.GetName()])
		if selected(v.Metadata.Labels, sel) {
			byName[svc.GetName()] = v
			names = append(names, svc.GetName())
		}
	}
	page, next, err := v1Page(r, "run-v1-services:"+sc.project+"/"+sc.location, names)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	resp := &runv1.ListServicesResponse{ApiVersion: "serving.knative.dev/v1", Kind: "ServiceList",
		Metadata: &runv1.ListMeta{Continue: next}, Items: []*runv1.Service{}}
	for _, n := range page {
		resp.Items = append(resp.Items, byName[n])
	}
	v1Write(w, nil, resp)
}

func (s *Server) v1GetService(w http.ResponseWriter, r *http.Request) {
	id, err := resourceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	svc, k, err := s.v1Service(r.Context(), scopeOf(r), id)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	v1Write(w, nil, toV1Service(svc, k))
}

// v1CreateService creates a service through v2's CreateService and answers
// with the service as it now reads, not ready yet: v1 returns the object,
// and a client watches status.conditions, as gcloud does, rather than
// polling an operation.
func (s *Server) v1CreateService(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	var in runv1.Service
	if err := v1DecodeService(r, &in); err != nil {
		v1Write(w, err, nil)
		return
	}
	id := ""
	if in.Metadata != nil {
		id = in.Metadata.Name
	}
	if err := ValidateServiceID(id); err != nil {
		v1Write(w, apierror.InvalidArgument("metadata.name: %v", err), nil)
		return
	}
	loc, err := sc.writeLocation(&in)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	parent := fmt.Sprintf("projects/%s/locations/%s", sc.project, loc)
	svc, err := fromV1Service(&in, parent+"/services/"+id)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	validateOnly := r.URL.Query().Get("dryRun") == "all"
	if _, err := s.CreateService(r.Context(), &runpb.CreateServiceRequest{Parent: parent, ServiceId: id, Service: svc,
		ValidateOnly: validateOnly}); err != nil {
		v1Write(w, err, nil)
		return
	}
	if validateOnly {
		svc.Name = parent + "/services/" + id
		v1Write(w, nil, toV1Service(svc, ksvc{}))
		return
	}
	s.v1Respond(w, r.Context(), v1Scope{project: sc.project, location: loc}, id)
}

// v1Respond answers a write with the service as it now reads.
func (s *Server) v1Respond(w http.ResponseWriter, ctx context.Context, sc v1Scope, id string) {
	svc, k, err := s.v1Service(ctx, sc, id)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	v1Write(w, nil, toV1Service(svc, k))
}

// writeLocation is the region a write is for: the path's or the regional
// host's, or else the object's cloud.googleapis.com/location label, which
// must agree with the path when both are given.
func (sc v1Scope) writeLocation(in *runv1.Service) (string, error) {
	label := ""
	if in.Metadata != nil {
		label = in.Metadata.Labels[v1LocationLabel]
		if ns := in.Metadata.Namespace; ns != "" && ns != sc.project {
			return "", apierror.InvalidArgument("metadata.namespace %q does not match the request's namespace %q", ns, sc.project)
		}
	}
	switch {
	case sc.location != "" && label != "" && label != sc.location:
		return "", apierror.InvalidArgument("label %s=%s does not match the request's region %s", v1LocationLabel, label, sc.location)
	case sc.location != "":
		return sc.location, nil
	case label != "":
		return label, nil
	}
	return "", apierror.InvalidArgument("no region: call a regional endpoint or the projects.locations path, " +
		"or set the " + v1LocationLabel + " label")
}

// v1ReplaceService replaces a service through v2's UpdateService with no
// mask, so the whole template is replaced, as v1's PUT is. A
// metadata.resourceVersion is the etag, and a stale one is refused.
func (s *Server) v1ReplaceService(w http.ResponseWriter, r *http.Request) {
	id, err := resourceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	sc := scopeOf(r)
	existing, _, err := s.v1Service(r.Context(), sc, id)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	var in runv1.Service
	if err := v1DecodeService(r, &in); err != nil {
		v1Write(w, err, nil)
		return
	}
	if in.Metadata == nil || in.Metadata.Name != id {
		v1Write(w, apierror.InvalidArgument("metadata.name must be %q, the service being replaced", id), nil)
		return
	}
	if l := in.Metadata.Labels[v1LocationLabel]; l != "" && l != locationOf(existing.GetName()) {
		v1Write(w, apierror.InvalidArgument("label %s=%s: a service cannot change region", v1LocationLabel, l), nil)
		return
	}
	svc, err := fromV1Service(&in, existing.GetName())
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	keepCloudSQLLayout(svc.GetTemplate(), existing.GetTemplate())
	svc.Etag = in.Metadata.ResourceVersion
	validateOnly := r.URL.Query().Get("dryRun") == "all"
	if _, err := s.UpdateService(r.Context(), &runpb.UpdateServiceRequest{Service: svc, ValidateOnly: validateOnly}); err != nil {
		v1Write(w, err, nil)
		return
	}
	s.v1Respond(w, r.Context(), sc, id)
}

func (s *Server) v1DeleteService(w http.ResponseWriter, r *http.Request) {
	id, err := resourceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	svc, k, err := s.v1Service(r.Context(), scopeOf(r), id)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	if _, err := s.DeleteService(r.Context(), &runpb.DeleteServiceRequest{Name: svc.GetName(),
		ValidateOnly: r.URL.Query().Get("dryRun") == "all"}); err != nil {
		v1Write(w, err, nil)
		return
	}
	v1Write(w, nil, &runv1.Status{Status: "Success",
		Details: &runv1.StatusDetails{Name: id, Kind: "services", Group: "serving.knative.dev", Uid: k.Metadata.UID}})
}

// v1Revisions are the revisions of every service in a scope.
func (s *Server) v1Revisions(ctx context.Context, sc v1Scope) ([]*runv1.Revision, error) {
	svcs, raw, err := s.v1Services(ctx, sc)
	if err != nil {
		return nil, err
	}
	var out []*runv1.Revision
	for _, svc := range svcs {
		items, err := s.kn.Revisions(ctx, raw[svc.GetName()].Metadata.Name)
		if err != nil {
			return nil, err
		}
		for _, kr := range items {
			out = append(out, toV1Revision(FromKnativeRevision(kr, svc.GetName()), kr))
		}
	}
	return out, nil
}

func (s *Server) v1ListRevisions(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	sel, err := labelSelector(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	revs, err := s.v1Revisions(r.Context(), sc)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	byKey := map[string]*runv1.Revision{}
	var keys []string
	for _, rev := range revs {
		if !selected(rev.Metadata.Labels, sel) {
			continue
		}
		key := rev.Metadata.Name
		byKey[key] = rev
		keys = append(keys, key)
	}
	page, next, err := v1Page(r, "run-v1-revisions:"+sc.project+"/"+sc.location, keys)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	resp := &runv1.ListRevisionsResponse{ApiVersion: "serving.knative.dev/v1", Kind: "RevisionList",
		Metadata: &runv1.ListMeta{Continue: next}, Items: []*runv1.Revision{}}
	for _, k := range page {
		resp.Items = append(resp.Items, byKey[k])
	}
	v1Write(w, nil, resp)
}

// v1Revision finds a revision by name among the scope's services' revisions.
func (s *Server) v1Revision(ctx context.Context, sc v1Scope, name string) (*runv1.Revision, string, error) {
	svcs, raw, err := s.v1Services(ctx, sc)
	if err != nil {
		return nil, "", err
	}
	for _, svc := range svcs {
		kname := raw[svc.GetName()].Metadata.Name
		if kr, err := s.kn.Revision(ctx, kname, kRevisionName(svc.GetName(), kname, name)); err == nil {
			return toV1Revision(FromKnativeRevision(kr, svc.GetName()), kr), svc.GetName(), nil
		}
	}
	return nil, "", apierror.NotFound("revisions.serving.knative.dev %q not found", name)
}

func (s *Server) v1GetRevision(w http.ResponseWriter, r *http.Request) {
	name, err := resourceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	rev, _, err := s.v1Revision(r.Context(), scopeOf(r), name)
	v1Write(w, err, rev)
}

func (s *Server) v1DeleteRevision(w http.ResponseWriter, r *http.Request) {
	name, err := resourceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	rev, service, err := s.v1Revision(r.Context(), scopeOf(r), name)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	if _, err := s.Revisions().DeleteRevision(r.Context(), &runpb.DeleteRevisionRequest{
		Name: service + "/revisions/" + name, ValidateOnly: r.URL.Query().Get("dryRun") == "all"}); err != nil {
		v1Write(w, err, nil)
		return
	}
	v1Write(w, nil, &runv1.Status{Status: "Success",
		Details: &runv1.StatusDetails{Name: name, Kind: "revisions", Group: "serving.knative.dev", Uid: rev.Metadata.Uid}})
}

// A Knative Service owns one Route and one Configuration of its own name,
// and v1 reads them as such.

func (s *Server) v1ListRoutes(w http.ResponseWriter, r *http.Request) {
	s.v1ListOwned(w, r, "RouteList", func(svc *runpb.Service, k ksvc) any { return toV1Route(svc, k) })
}

func (s *Server) v1ListConfigurations(w http.ResponseWriter, r *http.Request) {
	s.v1ListOwned(w, r, "ConfigurationList", func(svc *runpb.Service, k ksvc) any { return toV1Configuration(svc, k) })
}

func (s *Server) v1GetRoute(w http.ResponseWriter, r *http.Request) {
	s.v1GetOwned(w, r, "routes", func(svc *runpb.Service, k ksvc) any { return toV1Route(svc, k) })
}

func (s *Server) v1GetConfiguration(w http.ResponseWriter, r *http.Request) {
	s.v1GetOwned(w, r, "configurations", func(svc *runpb.Service, k ksvc) any { return toV1Configuration(svc, k) })
}

func (s *Server) v1GetOwned(w http.ResponseWriter, r *http.Request, kind string, conv func(*runpb.Service, ksvc) any) {
	id, err := resourceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	svc, k, err := s.v1Service(r.Context(), scopeOf(r), id)
	if err != nil {
		v1Write(w, apierror.NotFound("%s.serving.knative.dev %q not found", kind, id), nil)
		return
	}
	v1Write(w, nil, conv(svc, k))
}

// v1List is the list envelope routes and configurations share; their
// generated response types differ only in the item type.
type v1List struct {
	ApiVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   *runv1.ListMeta `json:"metadata,omitempty"`
	Items      []any           `json:"items"`
}

func (s *Server) v1ListOwned(w http.ResponseWriter, r *http.Request, kind string, conv func(*runpb.Service, ksvc) any) {
	sc := scopeOf(r)
	sel, err := labelSelector(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	svcs, raw, err := s.v1Services(r.Context(), sc)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	byName := map[string]any{}
	var names []string
	for _, svc := range svcs {
		if !selected(v1Labels(svc), sel) {
			continue
		}
		byName[svc.GetName()] = conv(svc, raw[svc.GetName()])
		names = append(names, svc.GetName())
	}
	page, next, err := v1Page(r, "run-v1-"+kind+":"+sc.project+"/"+sc.location, names)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	resp := &v1List{ApiVersion: "serving.knative.dev/v1", Kind: kind, Metadata: &runv1.ListMeta{Continue: next}, Items: []any{}}
	for _, n := range page {
		resp.Items = append(resp.Items, byName[n])
	}
	v1Write(w, nil, resp)
}

// v1Labels are a service's labels as v1 reports them: the caller's, and its
// region.
func v1Labels(svc *runpb.Service) map[string]string {
	out := map[string]string{v1LocationLabel: locationOf(svc.GetName())}
	for k, v := range svc.GetLabels() {
		out[k] = v
	}
	return out
}

// v1Meta is the metadata v1 reports for an object a service owns or is.
func v1Meta(svc *runpb.Service, k ksvc, collection string) *runv1.ObjectMeta {
	id, _ := ServiceID(svc.GetName())
	project := projectOf(svc.GetName())
	m := &runv1.ObjectMeta{
		Name: id, Namespace: project, Labels: v1Labels(svc), Annotations: svc.GetAnnotations(),
		Generation: svc.GetGeneration(), ResourceVersion: svc.GetEtag(), Uid: svc.GetUid(),
		SelfLink: fmt.Sprintf("/apis/serving.knative.dev/v1/namespaces/%s/%s/%s", project, collection, id),
	}
	if !k.Metadata.CreationTimestamp.IsZero() {
		m.CreationTimestamp = k.Metadata.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	if collection != "services" {
		m.Labels = map[string]string{v1LocationLabel: locationOf(svc.GetName()), labelService: id}
		m.Annotations = nil
	}
	return m
}

// v1Conditions are Knative's conditions as v1 reports them, which is
// Knative's own shape. A service not yet reconciled reports Ready Unknown.
func v1Conditions(cs []ksvcCondition) []*runv1.GoogleCloudRunV1Condition {
	var out []*runv1.GoogleCloudRunV1Condition
	for _, c := range cs {
		v := &runv1.GoogleCloudRunV1Condition{Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message}
		if !c.LastTransitionTime.IsZero() {
			v.LastTransitionTime = c.LastTransitionTime.UTC().Format(time.RFC3339)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		out = []*runv1.GoogleCloudRunV1Condition{{Type: "Ready", Status: "Unknown"}}
	}
	return out
}

func toV1Template(svc *runpb.Service) *runv1.RevisionTemplate {
	tmpl := svc.GetTemplate()
	ann := map[string]string{}
	for k, v := range tmpl.GetAnnotations() {
		ann[k] = v
	}
	if sc := tmpl.GetScaling(); sc != nil {
		if sc.GetMinInstanceCount() > 0 {
			ann[v1MinScale] = strconv.Itoa(int(sc.GetMinInstanceCount()))
		}
		if sc.GetMaxInstanceCount() > 0 {
			ann[v1MaxScale] = strconv.Itoa(int(sc.GetMaxInstanceCount()))
		}
	}
	ann = toV1CloudSQL(ann, tmpl.GetVolumes())
	if len(ann) == 0 {
		ann = nil
	}
	return &runv1.RevisionTemplate{
		Metadata: &runv1.ObjectMeta{Labels: tmpl.GetLabels(), Annotations: ann},
		Spec: &runv1.RevisionSpec{
			ContainerConcurrency: int64(tmpl.GetMaxInstanceRequestConcurrency()),
			TimeoutSeconds:       tmpl.GetTimeout().GetSeconds(),
			Containers:           toV1Containers(tmpl.GetContainers(), tmpl.GetVolumes()),
			ServiceAccountName:   tmpl.GetServiceAccount(),
			Volumes:              toV1Volumes(tmpl.GetVolumes()),
		},
	}
}

func toV1Containers(cs []*runpb.Container, volumes []*runpb.Volume) []*runv1.Container {
	var out []*runv1.Container
	for _, c := range cs {
		v := &runv1.Container{Name: c.GetName(), Image: c.GetImage(), Command: c.GetCommand(), Args: c.GetArgs(),
			WorkingDir: c.GetWorkingDir(), StartupProbe: toV1Probe(c.GetStartupProbe(), servingPort(c)), LivenessProbe: toV1Probe(c.GetLivenessProbe(), servingPort(c)),
			VolumeMounts: toV1Mounts(c.GetVolumeMounts(), volumes)}
		if l := c.GetResources().GetLimits(); len(l) > 0 {
			v.Resources = &runv1.ResourceRequirements{Limits: l}
		}
		for _, p := range c.GetPorts() {
			v.Ports = append(v.Ports, &runv1.ContainerPort{ContainerPort: int64(p.GetContainerPort()), Name: p.GetName()})
		}
		for _, e := range c.GetEnv() {
			ev := &runv1.EnvVar{Name: e.GetName(), Value: e.GetValue()}
			if ref := e.GetValueSource().GetSecretKeyRef(); ref != nil {
				// Cloud Run v1 names a Secret Manager secret as name and
				// its version as key.
				ev.Value = ""
				ev.ValueFrom = &runv1.EnvVarSource{SecretKeyRef: &runv1.SecretKeySelector{Name: ref.GetSecret(), Key: ref.GetVersion()}}
			}
			v.Env = append(v.Env, ev)
		}
		out = append(out, v)
	}
	return out
}

func toV1Traffic(ts []*runpb.TrafficTarget) []*runv1.TrafficTarget {
	var out []*runv1.TrafficTarget
	for _, t := range ts {
		v := &runv1.TrafficTarget{Percent: int64(t.GetPercent()), Tag: t.GetTag()}
		if trafficIsLatest(t) {
			v.LatestRevision = true
		} else {
			v.RevisionName = trafficRevision(t)
		}
		out = append(out, v)
	}
	return out
}

func toV1TrafficStatus(svc *runpb.Service) []*runv1.TrafficTarget {
	var out []*runv1.TrafficTarget
	for _, t := range svc.GetTrafficStatuses() {
		v := &runv1.TrafficTarget{Percent: int64(t.GetPercent()), Tag: t.GetTag(), Url: t.GetUri(), RevisionName: t.GetRevision(),
			LatestRevision: t.GetType() == runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST}
		out = append(out, v)
	}
	return out
}

func toV1Service(svc *runpb.Service, k ksvc) *runv1.Service {
	return &runv1.Service{
		ApiVersion: "serving.knative.dev/v1", Kind: "Service",
		Metadata: v1Meta(svc, k, "services"),
		Spec:     &runv1.ServiceSpec{Template: toV1Template(svc), Traffic: toV1Traffic(svc.GetTraffic())},
		Status: &runv1.ServiceStatus{
			ObservedGeneration: svc.GetObservedGeneration(), Url: svc.GetUri(), Address: &runv1.Addressable{Url: svc.GetUri()},
			LatestReadyRevisionName: svc.GetLatestReadyRevision(), LatestCreatedRevisionName: svc.GetLatestCreatedRevision(),
			Conditions: v1Conditions(k.Status.Conditions), Traffic: toV1TrafficStatus(svc),
		},
	}
}

func toV1Route(svc *runpb.Service, k ksvc) *runv1.Route {
	return &runv1.Route{
		ApiVersion: "serving.knative.dev/v1", Kind: "Route",
		Metadata: v1Meta(svc, k, "routes"),
		Spec:     &runv1.RouteSpec{Traffic: toV1Traffic(svc.GetTraffic())},
		Status: &runv1.RouteStatus{ObservedGeneration: svc.GetObservedGeneration(), Url: svc.GetUri(),
			Address: &runv1.Addressable{Url: svc.GetUri()}, Traffic: toV1TrafficStatus(svc),
			Conditions: v1Conditions(pick(k.Status.Conditions, "Ready", "RoutesReady"))},
	}
}

func toV1Configuration(svc *runpb.Service, k ksvc) *runv1.Configuration {
	return &runv1.Configuration{
		ApiVersion: "serving.knative.dev/v1", Kind: "Configuration",
		Metadata: v1Meta(svc, k, "configurations"),
		Spec:     &runv1.ConfigurationSpec{Template: toV1Template(svc)},
		Status: &runv1.ConfigurationStatus{ObservedGeneration: svc.GetObservedGeneration(),
			LatestReadyRevisionName: svc.GetLatestReadyRevision(), LatestCreatedRevisionName: svc.GetLatestCreatedRevision(),
			Conditions: v1Conditions(pick(k.Status.Conditions, "Ready", "ConfigurationsReady"))},
	}
}

// pick is a Service's condition from, read as the owned object's own
// condition: a Route's Ready is the Service's RoutesReady.
func pick(cs []ksvcCondition, as, from string) []ksvcCondition {
	for _, c := range cs {
		if c.Type == from {
			c.Type = as
			return []ksvcCondition{c}
		}
	}
	return nil
}

func toV1Revision(rev *runpb.Revision, kr krev) *runv1.Revision {
	id, _ := ServiceID(rev.GetService())
	// Its Cloud Run ID, as v2 names it (#1400), not the Knative name.
	revID := rev.GetName()[strings.LastIndex(rev.GetName(), "/")+1:]
	m := &runv1.ObjectMeta{
		Name: revID, Namespace: projectOf(rev.GetService()), Uid: rev.GetUid(), Generation: kr.Metadata.Generation,
		Labels: map[string]string{v1LocationLabel: locationOf(rev.GetService()), labelService: id,
			"serving.knative.dev/configuration": id, labelConfigGen: strconv.FormatInt(rev.GetGeneration(), 10)},
		SelfLink:    fmt.Sprintf("/apis/serving.knative.dev/v1/namespaces/%s/revisions/%s", projectOf(rev.GetService()), revID),
		Annotations: toV1CloudSQL(nil, rev.GetVolumes()),
	}
	if !kr.Metadata.CreationTimestamp.IsZero() {
		m.CreationTimestamp = kr.Metadata.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	return &runv1.Revision{
		ApiVersion: "serving.knative.dev/v1", Kind: "Revision", Metadata: m,
		Spec: &runv1.RevisionSpec{ContainerConcurrency: int64(rev.GetMaxInstanceRequestConcurrency()),
			Containers: toV1Containers(rev.GetContainers(), rev.GetVolumes()), ServiceAccountName: rev.GetServiceAccount(),
			Volumes: toV1Volumes(rev.GetVolumes())},
		Status: &runv1.RevisionStatus{ObservedGeneration: rev.GetObservedGeneration(), ServiceName: id,
			Conditions: v1Conditions(kr.Status.Conditions)},
	}
}

// fromV1Service reads a v1 Service as the v2 Service it describes, for v2's
// CreateService and UpdateService to check and apply. A v1 field v2 has no
// counterpart for, or that the v2 adapter does not map, is refused here
// rather than dropped.
func fromV1Service(in *runv1.Service, name string) (*runpb.Service, error) {
	var gaps []string
	gap := func(set bool, field string) {
		if set {
			gaps = append(gaps, field)
		}
	}
	svc := &runpb.Service{Name: name, Template: &runpb.RevisionTemplate{}}
	if m := in.Metadata; m != nil {
		labels := map[string]string{}
		for k, v := range m.Labels {
			if k != v1LocationLabel {
				labels[k] = v
			}
		}
		if len(labels) > 0 {
			svc.Labels = labels
		}
		ann := map[string]string{}
		for k, v := range m.Annotations {
			switch k {
			case v1ClientName:
				svc.Client = v
			case v1ClientVersion:
				svc.ClientVersion = v
			case v1BuildImageURI, v1BuildName, v1BuildSourceLocation, v1BuildAutoUpdates, v1BuildID,
				v1BuildFunctionTarget, v1BuildBaseImage:
				// Which build produced the image, which `gcloud run deploy
				// --source` records (#1347): v2's build_config. Accepted
				// and not read back; the build is in Cloud Build's history.
			default:
				gap(strings.HasPrefix(k, "run.googleapis.com/"), "metadata.annotations["+k+"]")
				ann[k] = v
			}
		}
		if len(ann) > 0 {
			svc.Annotations = ann
		}
		gap(m.GenerateName != "", "metadata.generateName")
		gap(len(m.Finalizers) > 0, "metadata.finalizers")
		gap(len(m.OwnerReferences) > 0, "metadata.ownerReferences")
	}
	spec := in.Spec
	if spec == nil || spec.Template == nil || spec.Template.Spec == nil {
		return nil, apierror.InvalidArgument("spec.template.spec.containers is required")
	}
	boost := false
	sqlConns := ""
	if tm := spec.Template.Metadata; tm != nil {
		svc.Template.Labels = tm.Labels
		svc.Template.Revision = tm.Name
		ann := map[string]string{}
		var scaling runpb.RevisionScaling
		for k, v := range tm.Annotations {
			switch k {
			case v1MinScale, v1MaxScale:
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 {
					return nil, apierror.InvalidArgument("spec.template.metadata.annotations[%s]: %q is not a count", k, v)
				}
				if k == v1MinScale {
					scaling.MinInstanceCount = int32(n)
				} else {
					scaling.MaxInstanceCount = int32(n)
				}
			case v1ClientName, v1ClientVersion:
				// Which tool deployed the revision: informational.
			case v1StartupCPUBoost:
				boost = v == "true" || v == "True"
			case v1CloudSQLInstances:
				sqlConns = v
			default:
				gap(strings.HasPrefix(k, "run.googleapis.com/"), "spec.template.metadata.annotations["+k+"]")
				ann[k] = v
			}
		}
		if scaling.MinInstanceCount > 0 || scaling.MaxInstanceCount > 0 {
			svc.Template.Scaling = &scaling
		}
		if len(ann) > 0 {
			svc.Template.Annotations = ann
		}
	}
	ts := spec.Template.Spec
	svc.Template.MaxInstanceRequestConcurrency = int32(ts.ContainerConcurrency)
	if ts.TimeoutSeconds > 0 {
		svc.Template.Timeout = durationpb.New(time.Duration(ts.TimeoutSeconds) * time.Second)
	}
	svc.Template.ServiceAccount = ts.ServiceAccountName
	svc.Template.Volumes = fromV1Volumes(ts.Volumes, gap)
	gap(len(ts.ImagePullSecrets) > 0, "spec.template.spec.imagePullSecrets")
	gap(len(ts.NodeSelector) > 0, "spec.template.spec.nodeSelector")
	gap(ts.RuntimeClassName != "", "spec.template.spec.runtimeClassName")
	gap(ts.EnableServiceLinks, "spec.template.spec.enableServiceLinks")
	for _, c := range ts.Containers {
		out := &runpb.Container{Name: c.Name, Image: c.Image, Command: c.Command, Args: c.Args, WorkingDir: c.WorkingDir}
		if c.Resources != nil {
			gap(len(c.Resources.Requests) > 0, "container.resources.requests")
			if len(c.Resources.Limits) > 0 {
				out.Resources = &runpb.ResourceRequirements{Limits: c.Resources.Limits}
			}
		}
		if boost {
			if out.Resources == nil {
				out.Resources = &runpb.ResourceRequirements{}
			}
			out.Resources.StartupCpuBoost = true
		}
		for _, p := range c.Ports {
			gap(p.Protocol != "" && p.Protocol != "TCP", "container.ports.protocol")
			out.Ports = append(out.Ports, &runpb.ContainerPort{Name: p.Name, ContainerPort: int32(p.ContainerPort)})
		}
		for _, e := range c.Env {
			out.Env = append(out.Env, fromV1Env(e, gap))
		}
		gap(len(c.EnvFrom) > 0, "container.envFrom")
		out.LivenessProbe = fromV1Probe(c.LivenessProbe, "container.livenessProbe", gap)
		out.StartupProbe = fromV1Probe(c.StartupProbe, "container.startupProbe", gap)
		out.VolumeMounts = fromV1Mounts(c.VolumeMounts)
		gap(c.ReadinessProbe != nil, "container.readinessProbe")
		gap(c.SecurityContext != nil && c.SecurityContext.RunAsUser != 0, "container.securityContext.runAsUser")
		gap(c.ImagePullPolicy != "", "container.imagePullPolicy")
		gap(c.TerminationMessagePath != "" || c.TerminationMessagePolicy != "", "container.terminationMessage")
		svc.Template.Containers = append(svc.Template.Containers, out)
	}
	for _, t := range spec.Traffic {
		gap(t.ConfigurationName != "", "spec.traffic.configurationName")
		out := &runpb.TrafficTarget{Percent: int32(t.Percent), Tag: t.Tag}
		if t.LatestRevision || t.RevisionName == "" {
			out.Type = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST
		} else {
			out.Type = runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION
			out.Revision = t.RevisionName
		}
		svc.Traffic = append(svc.Traffic, out)
	}
	vols, err := fromV1CloudSQL("spec.template.metadata.annotations["+v1CloudSQLInstances+"]", sqlConns,
		svc.Template.Volumes, svc.Template.Containers)
	if err != nil {
		return nil, err
	}
	svc.Template.Volumes = vols
	if len(gaps) > 0 {
		return nil, apierror.Unimplemented("unsupported Cloud Run v1 configuration: %s: not mapped", strings.Join(gaps, ", "))
	}
	return svc, nil
}

// servingPort is a container's first port, or 0.
// fromV1Env converts a v1 environment variable: a value, or a Secret
// Manager reference; a ConfigMap reference is a gap.
func fromV1Env(e *runv1.EnvVar, gap func(bool, string)) *runpb.EnvVar {
	ev := &runpb.EnvVar{Name: e.Name, Values: &runpb.EnvVar_Value{Value: e.Value}}
	if src := e.ValueFrom; src != nil {
		gap(src.ConfigMapKeyRef != nil, "container.env.valueFrom.configMapKeyRef")
		if ref := src.SecretKeyRef; ref != nil {
			ev.Values = &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
				SecretKeyRef: &runpb.SecretKeySelector{Secret: ref.Name, Version: ref.Key}}}
		}
	}
	return ev
}

func servingPort(c *runpb.Container) int32 {
	if ps := c.GetPorts(); len(ps) > 0 {
		return ps[0].GetContainerPort()
	}
	return 0
}
