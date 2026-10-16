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
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Resource Manager v1 REST (#301), for `gcloud projects` and Terraform's
// google_project, which call v1 rather than v3.
//
// Only projects.list, get, create, update (#1265), delete, undelete and
// getAncestry (#1294), the org policy methods (#1296, orgpolicy.go),
// organizations.get and search (#1294, hierarchy_api.go), and operations.get
// for the operations create returns, over the same Registry the v3 API and
// the console use. Every other v1 method — the IAM methods, liens — answers
// UNIMPLEMENTED.
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

// projectNumber is resource.ProjectNumber: v1 carries one and gcloud prints
// it, and Cloud Storage's buckets report the same number.
func projectNumber(id string) int64 { return resource.ProjectNumber(id) }

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
		Parent:         v1ParentOf(p.Parent),
	}
}

// v1Routes registers the v1 paths on the same router as v3.
func (s *ProjectsServer) v1Routes(r *rest.Router, ops *v1Ops) {
	r.Handle("GET /v1/projects", s.v1List)
	r.Handle("POST /v1/projects", func(w http.ResponseWriter, req *http.Request) error { return s.v1Create(w, req, ops) })
	r.Handle("GET /v1/projects/{project}", s.v1Get)
	r.Handle("DELETE /v1/projects/{project}", s.v1Delete)
	r.Handle("PUT /v1/projects/{project}", s.v1Update)
	r.Handle("POST /v1/projects/{project}", s.v1Verb)
	r.Handle("GET /v1/operations/{operation...}", func(w http.ResponseWriter, req *http.Request) error {
		name := "operations/" + req.PathValue("operation")
		op, ok := ops.get(name)
		if !ok {
			// The v3 methods' operations, which google_folder polls here
			// through the v1 base path (#1294).
			if op3, ok := v3Operations.get(name); ok {
				return writeProto(w, op3)
			}
			return apierror.NotFound("operation %s not found", name)
		}
		return rest.WriteJSON(w, http.StatusOK, op)
	})
	// Cloud Billing's read of a project's billing, for google_project.
	r.Handle("GET /v1/projects/{project}/billingInfo", s.billingInfo)
	r.Handle("GET /v1/{rest...}", func(http.ResponseWriter, *http.Request) error {
		return apierror.Unimplemented("this v1 method is not implemented: only projects.list, get, create, update, delete and undelete are served")
	})
}

func (s *ProjectsServer) v1List(w http.ResponseWriter, r *http.Request) error {
	match, err := parseV1Filter(r.URL.Query().Get("filter"))
	if err != nil {
		return err
	}
	list := s.reg.Active
	if mentionsState(r.URL.Query().Get("filter")) {
		list = s.reg.List
	}
	all, err := list()
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
// display name, unlike v3), labels.KEY, lifecycleState, parent.type
// (folder or organization) and parent.id. A trailing
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
		case key == "parent.type":
			preds = append(preds, func(p Project) bool { r := v1ParentOf(p.Parent); return r != nil && eq(r.Type) })
		case key == "parent.id":
			preds = append(preds, func(p Project) bool { r := v1ParentOf(p.Parent); return r != nil && eq(r.Id) })
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
	parent, err := v1Parent(in.Parent)
	if err != nil {
		return err
	}
	p, err := s.reg.Create(Project{ProjectID: in.ProjectId, DisplayName: in.Name, Labels: in.Labels, Parent: parent})
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

// v1Update is projects.update (#1265), the call Terraform's google_project
// makes to change a project's name or labels. v1 documents name and labels as
// the fields it changes, and the body as the whole project: an absent name
// clears it (the Registry then shows the project ID, as v3 does) and absent
// labels remove them. projectId, projectNumber, lifecycleState and createTime
// are output here, so they are kept whatever the body says. A parent that
// differs from the project's moves it there, as v1 documents (#1294); an
// absent parent leaves it where it is.
func (s *ProjectsServer) v1Update(w http.ResponseWriter, r *http.Request) error {
	id, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	var in crmv1.Project
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		return apierror.InvalidArgument("decode project: %v", err)
	}
	if in.ProjectId != "" && in.ProjectId != id {
		// UNVERIFIED: Google documents no code for a body naming another project.
		return apierror.InvalidArgument("project %q in the body does not match %q in the URL; a project ID cannot be changed", in.ProjectId, id)
	}
	parent, err := v1Parent(in.Parent)
	if err != nil {
		return err
	}
	if parent != "" {
		cur, err := s.reg.Get(id)
		if err != nil {
			return err
		}
		if cur.Parent != parent {
			if _, err := s.reg.Move(id, parent); err != nil {
				return err
			}
		}
	}
	p, err := s.reg.Update(id, in.Name, in.Labels)
	if err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, toV1(p))
}

func (s *ProjectsServer) v1Delete(w http.ResponseWriter, r *http.Request) error {
	id, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	if _, err := s.reg.Delete(id); err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, struct{}{})
}

// v1Verb is POST /v1/projects/{project}:{verb}: undelete, the org policy
// verbs (#1296), getAncestry (#1294), and IAM, stored and not enforced
// (#1368, iam.go).
func (s *ProjectsServer) v1Verb(w http.ResponseWriter, r *http.Request) error {
	raw, err := rest.PathValue(r, "project")
	if err != nil {
		return err
	}
	id, verb, _ := strings.Cut(raw, ":")
	switch verb {
	case "":
		return apierror.InvalidArgument("POST to a project requires a custom method")
	case "undelete":
		// v1's undelete answers an empty body (#1216).
		if _, err := s.reg.Undelete(id); err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, struct{}{})
	case "getAncestry":
		return s.v1Ancestry(w, id)
	case "getIamPolicy", "setIamPolicy", "testIamPermissions":
		return serveIAM(w, r, s, "projects/"+id, verb)
	}
	return s.v1OrgPolicy(w, r, id, verb)
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
