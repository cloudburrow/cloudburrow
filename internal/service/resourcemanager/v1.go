package resourcemanager

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	crmv1 "google.golang.org/api/cloudresourcemanager/v1"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Resource Manager v1 REST (#301), for `gcloud projects` and Terraform's
// google_project, which call v1 rather than v3.
//
// Only projects.list, get, create and delete, and operations.get for the
// operations those return, over the same Registry the v3 API and the console
// use. Every other v1 method — update, undelete, the IAM methods, org
// policies, liens, folders, organizations — answers UNIMPLEMENTED.
//
// Also served, because google_project reads it on every refresh: a Cloud
// Billing projects.getBillingInfo that says billing is disabled. There is no
// billing locally, and "disabled" is the honest answer to the question the
// provider asks; nothing else of Cloud Billing is served.

// v1Ops remembers the completed operations v1 create returned, so a client
// that polls operations.get finds them.
type v1Ops struct {
	mu  sync.Mutex
	ops map[string]*crmv1.Operation
}

func (o *v1Ops) put(op *crmv1.Operation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ops == nil {
		o.ops = map[string]*crmv1.Operation{}
	}
	// Bounded: an instance creating projects in a loop should not grow this
	// without limit, and a client polls an operation within seconds.
	if len(o.ops) > 1000 {
		o.ops = map[string]*crmv1.Operation{}
	}
	o.ops[op.Name] = op
}

func (o *v1Ops) get(name string) (*crmv1.Operation, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	op, ok := o.ops[name]
	return op, ok
}

// projectNumber is a stable number for a project ID. v1 carries one and
// gcloud prints it; the registry has none, so it is derived, the same every
// time for the same ID, and never claimed to match a real project's.
func projectNumber(id string) int64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(id); i++ {
		h ^= uint64(id[i])
		h *= 1099511628211
	}
	return int64(h%900000000000) + 100000000000
}

func toV1(p Project) *crmv1.Project {
	state := "ACTIVE"
	if p.State == StateDeleteRequested {
		state = "DELETE_REQUESTED"
	}
	return &crmv1.Project{
		ProjectId:      p.ProjectID,
		ProjectNumber:  projectNumber(p.ProjectID),
		Name:           p.DisplayName,
		Labels:         p.Labels,
		LifecycleState: state,
		CreateTime:     p.CreateTime,
	}
}

// v1Routes registers the v1 paths on the same router as v3.
func (s *ProjectsServer) v1Routes(r *rest.Router, ops *v1Ops) {
	r.Handle("GET /v1/projects", s.v1List)
	r.Handle("POST /v1/projects", func(w http.ResponseWriter, req *http.Request) error { return s.v1Create(w, req, ops) })
	r.Handle("GET /v1/projects/{project}", s.v1Get)
	r.Handle("DELETE /v1/projects/{project}", s.v1Delete)
	r.Handle("PUT /v1/projects/{project}", func(http.ResponseWriter, *http.Request) error {
		return ErrNotImplemented("v1 projects.update")
	})
	r.Handle("POST /v1/projects/{project}", s.v1Verb)
	r.Handle("GET /v1/operations/{operation...}", func(w http.ResponseWriter, req *http.Request) error {
		name := "operations/" + req.PathValue("operation")
		op, ok := ops.get(name)
		if !ok {
			return apierror.NotFound("operation %s not found", name)
		}
		return rest.WriteJSON(w, http.StatusOK, op)
	})
	// Cloud Billing's read of a project's billing, for google_project.
	r.Handle("GET /v1/projects/{project}/billingInfo", s.billingInfo)
	r.Handle("GET /v1/{rest...}", func(http.ResponseWriter, *http.Request) error {
		return apierror.Unimplemented("this v1 method is not implemented: only projects.list, get, create and delete are served")
	})
}

func (s *ProjectsServer) v1List(w http.ResponseWriter, r *http.Request) error {
	match, err := parseV1Filter(r.URL.Query().Get("filter"))
	if err != nil {
		return err
	}
	all, err := s.reg.List()
	if err != nil {
		return err
	}
	byID := map[string]Project{}
	var ids []string
	for _, p := range all {
		if match(p) {
			byID[p.ProjectID] = p
			ids = append(ids, p.ProjectID)
		}
	}
	size, err := rest.QueryInt(r, "pageSize", 0)
	if err != nil {
		return err
	}
	page, next, err := paging.Page("projects.v1:"+r.URL.Query().Get("filter"), ids, r.URL.Query().Get("pageToken"), size)
	if err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	resp := crmv1.ListProjectsResponse{NextPageToken: next, Projects: []*crmv1.Project{}}
	for _, id := range page {
		resp.Projects = append(resp.Projects, toV1(byID[id]))
	}
	return rest.WriteJSON(w, http.StatusOK, resp)
}

// parseV1Filter compiles a v1 projects.list filter: space-separated
// field:value terms, all of which must match. v1's fields are id, name (the
// display name, unlike v3), labels.KEY and lifecycleState; parent.type and
// parent.id match nothing, since no project here has a parent. A trailing
// wildcard in a value is honoured; OR and any other field are refused.
func parseV1Filter(q string) (func(Project) bool, error) {
	var preds []func(Project) bool
	for _, term := range strings.Fields(q) {
		if term == "OR" || term == "AND" || strings.ContainsAny(term, "()") {
			return nil, apierror.InvalidArgument("filter %q: only space-separated field:value terms are supported", q)
		}
		i := strings.IndexAny(term, ":=")
		if i <= 0 {
			return nil, apierror.InvalidArgument("filter term %q must be field:value", term)
		}
		key, val := strings.ToLower(term[:i]), strings.Trim(term[i+1:], `"'`)
		eq := func(got string) bool {
			if prefix, ok := strings.CutSuffix(val, "*"); ok {
				return strings.HasPrefix(got, prefix)
			}
			return got == val
		}
		switch {
		case key == "id" || key == "projectid":
			preds = append(preds, func(p Project) bool { return eq(p.ProjectID) })
		case key == "name":
			preds = append(preds, func(p Project) bool { return eq(p.DisplayName) })
		case key == "lifecyclestate":
			preds = append(preds, func(p Project) bool { return strings.EqualFold(string(p.State), val) })
		case key == "parent.type" || key == "parent.id":
			preds = append(preds, func(Project) bool { return false })
		case strings.HasPrefix(key, "labels."):
			lk := term[len("labels."):i]
			preds = append(preds, func(p Project) bool { v, ok := p.Labels[lk]; return ok && eq(v) })
		default:
			return nil, apierror.InvalidArgument("filter field %q is not supported", term[:i])
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

func (s *ProjectsServer) v1Get(w http.ResponseWriter, r *http.Request) error {
	raw, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	if strings.Contains(raw, ":") {
		return apierror.Unimplemented("v1 custom method on %q is not implemented", raw)
	}
	p, err := s.reg.Get(raw)
	if err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, toV1(p))
}

func (s *ProjectsServer) v1Create(w http.ResponseWriter, r *http.Request, ops *v1Ops) error {
	var in crmv1.Project
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		return apierror.InvalidArgument("decode project: %v", err)
	}
	if in.Parent != nil && in.Parent.Id != "" {
		return ErrNotImplemented("parent " + in.Parent.Type + "/" + in.Parent.Id + ": folders and organizations")
	}
	p, err := s.reg.Create(Project{ProjectID: in.ProjectId, DisplayName: in.Name, Labels: in.Labels})
	if err != nil {
		return err
	}
	resp, _ := json.Marshal(struct {
		Type string `json:"@type"`
		*crmv1.Project
	}{"type.googleapis.com/google.cloudresourcemanager.v1.Project", toV1(p)})
	op := &crmv1.Operation{
		Name:     fmt.Sprintf("operations/cp.%d", time.Now().UnixNano()),
		Done:     true,
		Response: resp,
	}
	ops.put(op)
	return rest.WriteJSON(w, http.StatusOK, op)
}

func (s *ProjectsServer) v1Delete(w http.ResponseWriter, r *http.Request) error {
	id, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	if err := s.reg.Delete(id); err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, struct{}{})
}

// v1Verb is POST /v1/projects/{project}:{verb}: IAM, undelete, org policy
// and ancestry, none of which is served.
func (s *ProjectsServer) v1Verb(w http.ResponseWriter, r *http.Request) error {
	raw, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	_, verb, _ := strings.Cut(raw, ":")
	if verb == "" {
		return apierror.InvalidArgument("POST to a project requires a custom method")
	}
	return ErrNotImplemented("v1 projects." + verb)
}

// billingInfo is Cloud Billing's projects.getBillingInfo. Billing is never
// enabled locally.
func (s *ProjectsServer) billingInfo(w http.ResponseWriter, r *http.Request) error {
	id, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	if _, err := s.reg.Get(id); err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, map[string]any{
		"name":           "projects/" + id + "/billingInfo",
		"projectId":      id,
		"billingEnabled": false,
	})
}
