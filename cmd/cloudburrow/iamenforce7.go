package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/service/apikeys"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
	"github.com/cloudburrow/cloudburrow/internal/service/privateca"
	"github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
)

// The seventh round's services (#1879): Certificate Authority Service,
// Kubernetes Engine, Compute Engine, API Keys and Service Usage, and the
// Cloud Run port's UploadSource and Operations and Locations mixins.
type iamEnforced7 struct {
	privateCA    *privateCAService
	gke          *gkeService
	compute      *computeService
	apiKeys      *apikeys.Server
	serviceUsage *serviceusage.Server
	run          *runService
}

func (s iamEnforced7) use(e *iameval.Enforcer) {
	if s.privateCA != nil {
		s.privateCA.interpose = append(s.privateCA.interpose, e.Interceptor(iameval.PrivateCA))
		s.privateCA.wrapJSON = jsonEnforcer(e, iameval.PrivateCA, privateca.Mixins())
		// CA pools, certificate templates and CRLs keep their own policies.
		e.Policies[iameval.PrivateCA.Name] = iameval.GRPCPolicyGetter(func() func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			api := s.privateCA.API()
			if api == nil {
				return nil
			}
			return api.IAMPolicy().GetIamPolicy
		})
		e.Resources[iameval.PrivateCA.Name] = func(_ context.Context, project string) []iameval.ChildResource {
			api := s.privateCA.API()
			if api == nil {
				return nil
			}
			var out []iameval.ChildResource
			for _, n := range api.PolicyResourceNames(project) {
				c := iameval.ChildResource{Name: n}
				if pool, _, ok := strings.Cut(n, "/certificateAuthorities/"); ok {
					// A CRL, under its CA's pool: a CA keeps no policy.
					c.Parent = pool
				}
				out = append(out, c)
			}
			return out
		}
	}
	if s.gke != nil {
		s.gke.interpose = append(s.gke.interpose, e.Interceptor(iameval.GKE))
		s.gke.wrapJSON = jsonEnforcer(e, iameval.GKE, nil)
	}
	if s.compute != nil {
		// Instances, disks, snapshots and subnetworks keep allow policies
		// (#2112), which apply beside the project's, and Cloud Asset's
		// analysis under a scope lists them.
		e.Policies[iameval.Compute.Name] = func(_ context.Context, resource string) *iampolicy.Stored {
			api := s.compute.API()
			if api == nil {
				return nil
			}
			return api.Policy(resource)
		}
		e.Resources[iameval.Compute.Name] = func(_ context.Context, project string) []iameval.ChildResource {
			api := s.compute.API()
			if api == nil {
				return nil
			}
			var out []iameval.ChildResource
			for _, n := range api.PolicyResourceNames(project) {
				out = append(out, iameval.ChildResource{Name: n})
			}
			return out
		}
		s.compute.wrapJSON = func(next http.Handler) http.Handler {
			checked := e.HTTPPaths(iameval.Compute, iameval.ComputeJSON, next)
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if publicImageProject(r.URL.Path) {
					next.ServeHTTP(w, r)
					return
				}
				if m := iameval.ComputeIAMTest.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodPost && e.On != nil && e.On() {
					e.TestPermissionsJSON(w, r, iameval.Compute, m[1], next)
					return
				}
				// An INTERNAL address's insert is createInternal, in place
				// of create (round 13).
				if perm, project, ok := computeInternalInsert(r); ok {
					if err := e.Authorize(r.Context(), iameval.Compute, iameval.Bearer(r.Header.Get("Authorization")), perm, project); err != nil {
						iameval.WriteJSONStatus(w, err)
						return
					}
					next.ServeHTTP(w, r)
					return
				}
				checked.ServeHTTP(w, r)
			})
		}
	}
	if s.apiKeys != nil {
		s.apiKeys.Wrap = func(next http.Handler) http.Handler { return e.HTTPPaths(iameval.APIKeys, iameval.APIKeysJSON, next) }
	}
	if s.serviceUsage != nil {
		s.serviceUsage.Wrap = func(next http.Handler) http.Handler {
			return e.HTTPPaths(iameval.ServiceUsage, iameval.ServiceUsageJSON, next)
		}
	}
	if s.run != nil {
		lookup := func(name string) string {
			a := s.run.adapter.Load()
			if a == nil {
				return ""
			}
			op, err := a.Operation(name)
			if err != nil {
				return ""
			}
			return op.GetMetadata().GetTypeUrl()
		}
		s.run.interpose = append(s.run.interpose, runPortMixins(e, lookup))
		prev := s.run.wrapJSON
		s.run.wrapJSON = func(next http.Handler) http.Handler {
			h := e.HTTPPaths(iameval.Run, iameval.RunUploadSourceJSON,
				e.HTTPPaths(iameval.ArtifactRegistry, iameval.ArtifactRegistryLocationsJSON, runPortMixinsJSON(e, lookup, next)))
			if prev != nil {
				h = prev(h)
			}
			return h
		}
	}
}

// publicImageProject is whether a Compute path is a public image
// project's (debian-cloud, ...), which every caller may read.
func publicImageProject(path string) bool {
	rest, ok := strings.CutPrefix(path, "/compute/v1/projects/")
	if !ok {
		return false
	}
	project, _, _ := strings.Cut(rest, "/")
	for _, img := range compute.PublicImages {
		if img.Project == project {
			return true
		}
	}
	return false
}

// runPortMixins checks the Cloud Run port's Operations and Locations
// mixins over gRPC (iameval.RunPortOperation): an operation by its own
// API's permission, a listing by any of the APIs' and then each operation
// by its own.
func runPortMixins(e *iameval.Enforcer, typeOf func(string) string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if e == nil || e.On == nil || !e.On() {
			return handler(ctx, req)
		}
		bearer := iameval.IncomingBearer(ctx)
		switch in := req.(type) {
		case *longrunningpb.GetOperationRequest:
			if err := runPortOperation(ctx, e, bearer, typeOf, in.GetName(), false); err != nil {
				return nil, err
			}
		case *longrunningpb.WaitOperationRequest:
			if err := runPortOperation(ctx, e, bearer, typeOf, in.GetName(), false); err != nil {
				return nil, err
			}
		case *longrunningpb.DeleteOperationRequest:
			if err := runPortOperation(ctx, e, bearer, typeOf, in.GetName(), true); err != nil {
				return nil, err
			}
		case *longrunningpb.ListOperationsRequest:
			if err := runPortList(ctx, e, bearer, iameval.RunPortListOperations, in.GetName()); err != nil {
				return nil, err
			}
			resp, err := handler(ctx, req)
			out, ok := resp.(*longrunningpb.ListOperationsResponse)
			if err != nil || !ok {
				return resp, err
			}
			kept := out.Operations[:0]
			for _, op := range out.GetOperations() {
				if runPortListed(ctx, e, bearer, op.GetMetadata().GetTypeUrl(), op.GetName()) {
					kept = append(kept, op)
				}
			}
			out.Operations = kept
			return out, nil
		case *location.ListLocationsRequest:
			if err := runPortList(ctx, e, bearer, iameval.RunPortListLocations, in.GetName()); err != nil {
				return nil, err
			}
		}
		return handler(ctx, req)
	}
}

// runPortOperation checks a call on one operation by its API's get (or
// delete) permission; an operation not found, or of an API not checked,
// passes to the handler.
func runPortOperation(ctx context.Context, e *iameval.Enforcer, bearer string, typeOf func(string) string, name string, del bool) error {
	svc, get, _, d := iameval.RunPortOperation(typeOf(name))
	perm := get
	if del {
		perm = d
	}
	if svc == nil || perm == "" {
		return nil
	}
	return e.Authorize(ctx, svc, bearer, perm, name)
}

// runPortList refuses a listing unless the caller holds one of perms on
// parent; the denial names the first (Cloud Run's).
func runPortList(ctx context.Context, e *iameval.Enforcer, bearer string, perms []string, parent string) error {
	ok, err := e.HoldsAny(ctx, iameval.Run, bearer, perms, parent)
	if err != nil {
		return err
	}
	if !ok {
		return iameval.Run.Deny(perms[0], parent)
	}
	return nil
}

// runPortListed is whether a listed operation is kept: its API's list
// permission held on its location, or an API not checked.
func runPortListed(ctx context.Context, e *iameval.Enforcer, bearer, typeURL, name string) bool {
	svc, _, list, _ := iameval.RunPortOperation(typeURL)
	if svc == nil {
		return true
	}
	parent := name
	if i := strings.Index(name, "/operations/"); i >= 0 {
		parent = name[:i]
	}
	ok, err := e.HoldsAny(ctx, svc, bearer, []string{list}, parent)
	return err == nil && ok
}

var (
	// Eventarc's JSON API is under /eventarc on the port (round 13).
	runPortOpPath  = regexp.MustCompile(`^(?:/eventarc)?/v[12]/(projects/[^/]+/locations/[^/]+/operations/[^/:]+)(:wait)?$`)
	runPortOpsPath = regexp.MustCompile(`^(?:/eventarc)?/v[12]/(projects/[^/]+/locations/[^/]+)/operations$`)
	// Cloud Run v1's and Cloud Functions v1's list is /v1/ (round 13).
	runPortLocsPath  = regexp.MustCompile(`^/v[12]/(projects/[^/]+)/locations$`)
	runPortV1OpPath  = regexp.MustCompile(`^/v1/(operations/[^/:]+)$`)
	runPortV1OpsPath = regexp.MustCompile(`^/v1/operations$`)
)

// runPortMixinsJSON is runPortMixins over JSON: the operations' paths of
// Cloud Run (/v2/) and of the v1 APIs on the port (/v1/), and Cloud
// Functions v2's ListLocations.
func runPortMixinsJSON(e *iameval.Enforcer, typeOf func(string) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() {
			next.ServeHTTP(w, r)
			return
		}
		ctx, bearer := r.Context(), iameval.Bearer(r.Header.Get("Authorization"))
		path := r.URL.Path
		var err error
		switch {
		case runPortOpPath.MatchString(path) && (r.Method == http.MethodGet || r.Method == http.MethodPost || r.Method == http.MethodDelete):
			err = runPortOperation(ctx, e, bearer, typeOf, runPortOpPath.FindStringSubmatch(path)[1], r.Method == http.MethodDelete)
		case runPortV1OpPath.MatchString(path) && r.Method == http.MethodGet:
			err = runPortOperation(ctx, e, bearer, typeOf, runPortV1OpPath.FindStringSubmatch(path)[1], false)
		case runPortLocsPath.MatchString(path) && r.Method == http.MethodGet:
			err = runPortList(ctx, e, bearer, iameval.RunPortListLocations, runPortLocsPath.FindStringSubmatch(path)[1])
		case (runPortOpsPath.MatchString(path) || runPortV1OpsPath.MatchString(path)) && r.Method == http.MethodGet:
			if g := runPortOpsPath.FindStringSubmatch(path); g != nil {
				if err := runPortList(ctx, e, bearer, iameval.RunPortListOperations, g[1]); err != nil {
					iameval.WriteJSONStatus(w, err)
					return
				}
			}
			filterOperationsJSON(w, r, func(typeURL, name string) bool { return runPortListed(ctx, e, bearer, typeURL, name) }, next)
			return
		}
		if err != nil {
			iameval.WriteJSONStatus(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// filterOperationsJSON serves a JSON ListOperations, keeping the
// operations keep accepts by their metadata's @type and name.
func filterOperationsJSON(w http.ResponseWriter, r *http.Request, keep func(typeURL, name string) bool, next http.Handler) {
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	var out map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	if ops, ok := out["operations"].([]any); ok {
		kept := []any{}
		for _, o := range ops {
			m, _ := o.(map[string]any)
			md, _ := m["metadata"].(map[string]any)
			t, _ := md["@type"].(string)
			n, _ := m["name"].(string)
			if keep(t, n) {
				kept = append(kept, o)
			}
		}
		if len(kept) == 0 {
			delete(out, "operations")
		} else {
			out["operations"] = kept
		}
	}
	b, _ := json.Marshal(out)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
