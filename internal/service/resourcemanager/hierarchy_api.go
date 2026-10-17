package resourcemanager

// The v3 Folders and Organizations services, v2 folders (gcloud
// resource-manager folders) and v1 organizations and getAncestry (gcloud
// organizations, gcloud projects get-ancestors), over the hierarchy in
// hierarchy.go (#1294). Every change completes before the call returns, so
// the long-running operations Google returns come back done; they are also
// kept for operations.get.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	crmv2 "google.golang.org/api/cloudresourcemanager/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// v3Ops keeps the operations the v3 methods return, so operations.get finds
// them. Bounded, as v1Ops is.
type v3Ops struct {
	mu  sync.Mutex
	ops map[string]*longrunningpb.Operation
}

var v3Operations v3Ops

func (o *v3Ops) put(op *longrunningpb.Operation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ops == nil || len(o.ops) > 1000 {
		o.ops = map[string]*longrunningpb.Operation{}
	}
	o.ops[op.Name] = op
}

func (o *v3Ops) get(name string) (*longrunningpb.Operation, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	op, ok := o.ops[name]
	return op, ok
}

// OperationsServer serves google.longrunning.Operations.GetOperation for the
// operations Resource Manager returned.
type OperationsServer struct {
	longrunningpb.UnimplementedOperationsServer
}

func (OperationsServer) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	op, ok := v3Operations.get(req.GetName())
	if !ok {
		return nil, apierror.Wrap(apierror.NotFound("operation %s not found", req.GetName()))
	}
	return op, nil
}

func ts(s string) *timestamppb.Timestamp {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return timestamppb.New(t)
	}
	return nil
}

func folderProto(f Folder) *rmpb.Folder {
	out := &rmpb.Folder{Name: f.Name, Parent: f.Parent, DisplayName: f.DisplayName, State: rmpb.Folder_ACTIVE,
		CreateTime: ts(f.CreateTime), UpdateTime: ts(f.UpdateTime), Etag: f.Etag()}
	if f.State == StateDeleteRequested {
		out.State = rmpb.Folder_DELETE_REQUESTED
		out.DeleteTime = ts(f.DeleteTime)
	}
	return out
}

// OrganizationProto is the local organization.
func OrganizationProto() *rmpb.Organization {
	return &rmpb.Organization{Name: OrganizationName, DisplayName: OrganizationDomain,
		Owner: &rmpb.Organization_DirectoryCustomerId{DirectoryCustomerId: OrganizationCustomerID},
		State: rmpb.Organization_ACTIVE, CreateTime: ts(OrganizationCreateTime), UpdateTime: ts(OrganizationCreateTime),
		Etag: policyEtag([]byte(OrganizationName))}
}

// FoldersServer serves google.cloud.resourcemanager.v3.Folders.
type FoldersServer struct {
	rmpb.UnimplementedFoldersServer
	reg *Registry
}

// NewFoldersServer returns the Folders service over reg.
func NewFoldersServer(reg *Registry) *FoldersServer { return &FoldersServer{reg: reg} }

func (s *FoldersServer) GetFolder(_ context.Context, req *rmpb.GetFolderRequest) (*rmpb.Folder, error) {
	f, err := s.reg.Folder(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return folderProto(f), nil
}

// listFolders pages the folders that match.
func (s *FoldersServer) listFolders(scope string, match func(Folder) bool, token string, size int) ([]*rmpb.Folder, string, error) {
	all, err := s.reg.Folders()
	if err != nil {
		return nil, "", err
	}
	byName := map[string]Folder{}
	var names []string
	for _, f := range all {
		if match(f) {
			byName[f.Name] = f
			names = append(names, f.Name)
		}
	}
	page, next, err := paging.Page(scope, names, token, size)
	if err != nil {
		return nil, "", apierror.InvalidArgument("%v", err)
	}
	out := make([]*rmpb.Folder, 0, len(page))
	for _, n := range page {
		out = append(out, folderProto(byName[n]))
	}
	return out, next, nil
}

func (s *FoldersServer) ListFolders(_ context.Context, req *rmpb.ListFoldersRequest) (*rmpb.ListFoldersResponse, error) {
	parent := req.GetParent()
	if parent == "" {
		return nil, apierror.Wrap(apierror.InvalidArgument("parent is required: folders/{folder_number} or %s", OrganizationName))
	}
	if err := s.reg.CheckParentExists(parent); err != nil {
		return nil, apierror.Wrap(err)
	}
	show := req.GetShowDeleted()
	folders, next, err := s.listFolders(fmt.Sprintf("folders.list:%s:%v", parent, show), func(f Folder) bool {
		return f.Parent == parent && (show || f.State == StateActive)
	}, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &rmpb.ListFoldersResponse{Folders: folders, NextPageToken: next}, nil
}

// queryTerms splits a query at spaces outside double or single quotes, so
// displayName="Team A" is one term.
func queryTerms(q string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	for _, c := range q {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteRune(c)
		case c == '"' || c == '\'':
			quote = c
			cur.WriteRune(c)
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseFolderQuery compiles a SearchFolders query: space-separated
// key:value or key=value terms, all of which must match, over displayName
// (a trailing * matches a prefix), state and parent. Any other key, OR, or
// a parenthesis is refused rather than ignored.
func parseFolderQuery(q string) (func(Folder) bool, bool, error) {
	var preds []func(Folder) bool
	state := false
	for _, term := range queryTerms(q) {
		if term == "OR" || term == "AND" || strings.ContainsAny(term, "()") {
			return nil, false, apierror.InvalidArgument("query %q: only space-separated key:value terms are supported", q)
		}
		i := strings.IndexAny(term, ":=")
		if i <= 0 {
			return nil, false, apierror.InvalidArgument("query term %q must be key:value", term)
		}
		key, val := term[:i], strings.Trim(term[i+1:], `"'`)
		switch strings.ToLower(key) {
		case "displayname", "display_name":
			preds = append(preds, func(f Folder) bool {
				if p, ok := strings.CutSuffix(val, "*"); ok {
					return strings.HasPrefix(f.DisplayName, p)
				}
				return f.DisplayName == val
			})
		case "state", "lifecyclestate":
			state = true
			preds = append(preds, func(f Folder) bool { return strings.EqualFold(string(f.State), val) })
		case "parent":
			preds = append(preds, func(f Folder) bool { return f.Parent == val })
		default:
			return nil, false, apierror.InvalidArgument("query key %q is not supported: displayName, state and parent are", key)
		}
	}
	return func(f Folder) bool {
		if !state && f.State != StateActive {
			return false
		}
		for _, p := range preds {
			if !p(f) {
				return false
			}
		}
		return true
	}, state, nil
}

func (s *FoldersServer) SearchFolders(_ context.Context, req *rmpb.SearchFoldersRequest) (*rmpb.SearchFoldersResponse, error) {
	match, _, err := parseFolderQuery(req.GetQuery())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	folders, next, err := s.listFolders("folders.search:"+req.GetQuery(), match, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &rmpb.SearchFoldersResponse{Folders: folders, NextPageToken: next}, nil
}

func (s *FoldersServer) CreateFolder(_ context.Context, req *rmpb.CreateFolderRequest) (*longrunningpb.Operation, error) {
	in := req.GetFolder()
	if in == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("folder is required"))
	}
	f, err := s.reg.CreateFolder(in.GetParent(), in.GetDisplayName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("cf", f.ID(), folderProto(f))
}

func (s *FoldersServer) UpdateFolder(_ context.Context, req *rmpb.UpdateFolderRequest) (*longrunningpb.Operation, error) {
	in := req.GetFolder()
	if in == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("folder is required"))
	}
	// An absent mask updates display_name, the one field that can change:
	// Terraform's google_folder sends none, and Google accepts that.
	for _, p := range req.GetUpdateMask().GetPaths() {
		if p != "display_name" && p != "displayName" {
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask path %q cannot be updated; only display_name, and MoveFolder changes the parent", p))
		}
	}
	f, err := s.reg.RenameFolder(in.GetName(), in.GetDisplayName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("uf", f.ID(), folderProto(f))
}

func (s *FoldersServer) MoveFolder(_ context.Context, req *rmpb.MoveFolderRequest) (*longrunningpb.Operation, error) {
	if req.GetDestinationParent() == "" {
		return nil, apierror.Wrap(apierror.InvalidArgument("destination_parent is required"))
	}
	f, err := s.reg.MoveFolder(req.GetName(), req.GetDestinationParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("mf", f.ID(), folderProto(f))
}

func (s *FoldersServer) DeleteFolder(_ context.Context, req *rmpb.DeleteFolderRequest) (*longrunningpb.Operation, error) {
	f, err := s.reg.DeleteFolder(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("df", f.ID(), folderProto(f))
}

func (s *FoldersServer) UndeleteFolder(_ context.Context, req *rmpb.UndeleteFolderRequest) (*longrunningpb.Operation, error) {
	f, err := s.reg.UndeleteFolder(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("udf", f.ID(), folderProto(f))
}

// CheckParentExists is CheckParent for a listing: the parent must exist,
// and may be pending deletion.
func (r *Registry) CheckParentExists(parent string) error {
	if parent == OrganizationName {
		return nil
	}
	if strings.HasPrefix(parent, "folders/") {
		_, err := r.Folder(parent)
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.checkParentLocked(parent)
	return err
}

// OrganizationsServer serves google.cloud.resourcemanager.v3.Organizations.
type OrganizationsServer struct {
	rmpb.UnimplementedOrganizationsServer
	reg *Registry // where its IAM policy is kept (iam.go)
}

// NewOrganizationsServer returns the Organizations service, whose IAM policy
// is kept in reg.
func NewOrganizationsServer(reg *Registry) OrganizationsServer { return OrganizationsServer{reg: reg} }

func checkOrg(name string) error {
	if name == OrganizationName {
		return nil
	}
	if strings.HasPrefix(name, "organizations/") {
		return apierror.NotFound("organization %s not found; the local organization is %s", name, OrganizationName)
	}
	return apierror.InvalidArgument("name %q must be organizations/{organization_number}", name)
}

func (OrganizationsServer) GetOrganization(_ context.Context, req *rmpb.GetOrganizationRequest) (*rmpb.Organization, error) {
	if err := checkOrg(req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	return OrganizationProto(), nil
}

// orgMatches is a SearchOrganizations query: domain:, directoryCustomerId:
// or owner.directorycustomerid: terms, all of which must match.
func orgMatches(q string) (bool, error) {
	for _, term := range strings.Fields(q) {
		i := strings.IndexAny(term, ":=")
		if i <= 0 {
			return false, apierror.InvalidArgument("query term %q must be key:value", term)
		}
		val := strings.Trim(term[i+1:], `"'`)
		switch strings.ToLower(term[:i]) {
		case "domain":
			if val != OrganizationDomain {
				return false, nil
			}
		case "directorycustomerid", "owner.directorycustomerid":
			if val != OrganizationCustomerID {
				return false, nil
			}
		default:
			return false, apierror.InvalidArgument("query key %q is not supported: domain and directoryCustomerId are", term[:i])
		}
	}
	return true, nil
}

func (OrganizationsServer) SearchOrganizations(_ context.Context, req *rmpb.SearchOrganizationsRequest) (*rmpb.SearchOrganizationsResponse, error) {
	ok, err := orgMatches(req.GetQuery())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	out := &rmpb.SearchOrganizationsResponse{}
	if ok {
		out.Organizations = []*rmpb.Organization{OrganizationProto()}
	}
	return out, nil
}

// --- v3 REST -----------------------------------------------------------

func (s *FoldersServer) restRoutes(r *rest.Router, orgs OrganizationsServer, v1ops *v1Ops) {
	r.Handle("GET /v3/folders", func(w http.ResponseWriter, req *http.Request) error {
		size, err := rest.QueryInt(req, "pageSize", 0)
		if err != nil {
			return err
		}
		q := req.URL.Query()
		resp, err := s.ListFolders(req.Context(), &rmpb.ListFoldersRequest{Parent: q.Get("parent"), PageToken: q.Get("pageToken"),
			PageSize: int32(size), ShowDeleted: q.Get("showDeleted") == "true"})
		if err != nil {
			return err
		}
		return writeProto(w, resp)
	})
	r.Handle("GET /v3/folders:search", func(w http.ResponseWriter, req *http.Request) error {
		size, err := rest.QueryInt(req, "pageSize", 0)
		if err != nil {
			return err
		}
		q := req.URL.Query()
		resp, err := s.SearchFolders(req.Context(), &rmpb.SearchFoldersRequest{Query: q.Get("query"), PageToken: q.Get("pageToken"), PageSize: int32(size)})
		if err != nil {
			return err
		}
		return writeProto(w, resp)
	})
	r.Handle("POST /v3/folders", func(w http.ResponseWriter, req *http.Request) error {
		var f rmpb.Folder
		if err := readProto(req, &f); err != nil {
			return err
		}
		// The provider and gcloud send the parent in the body; a query
		// parameter is accepted too.
		if f.Parent == "" {
			f.Parent = req.URL.Query().Get("parent")
		}
		op, err := s.CreateFolder(req.Context(), &rmpb.CreateFolderRequest{Folder: &f})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	r.Handle("GET /v3/folders/{folder}", func(w http.ResponseWriter, req *http.Request) error {
		id, err := rest.PathValue(req, "folder")
		if err != nil {
			return err
		}
		f, err := s.GetFolder(req.Context(), &rmpb.GetFolderRequest{Name: "folders/" + id})
		if err != nil {
			return err
		}
		return writeProto(w, f)
	})
	r.Handle("PATCH /v3/folders/{folder}", func(w http.ResponseWriter, req *http.Request) error {
		id, err := rest.PathValue(req, "folder")
		if err != nil {
			return err
		}
		var f rmpb.Folder
		if err := readProto(req, &f); err != nil {
			return err
		}
		f.Name = "folders/" + id
		mask := splitMask(req.URL.Query().Get("updateMask"))
		op, err := s.UpdateFolder(req.Context(), &rmpb.UpdateFolderRequest{Folder: &f, UpdateMask: mask})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	r.Handle("DELETE /v3/folders/{folder}", func(w http.ResponseWriter, req *http.Request) error {
		id, err := rest.PathValue(req, "folder")
		if err != nil {
			return err
		}
		op, err := s.DeleteFolder(req.Context(), &rmpb.DeleteFolderRequest{Name: "folders/" + id})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	r.Handle("POST /v3/folders/{folder}", func(w http.ResponseWriter, req *http.Request) error {
		raw, err := rest.PathValue(req, "folder")
		if err != nil {
			return err
		}
		id, verb, _ := strings.Cut(raw, ":")
		name := "folders/" + id
		var op *longrunningpb.Operation
		switch verb {
		case "move":
			var in rmpb.MoveFolderRequest
			if err := readProto(req, &in); err != nil {
				return err
			}
			op, err = s.MoveFolder(req.Context(), &rmpb.MoveFolderRequest{Name: name, DestinationParent: in.GetDestinationParent()})
		case "undelete":
			op, err = s.UndeleteFolder(req.Context(), &rmpb.UndeleteFolderRequest{Name: name})
		case "getIamPolicy", "setIamPolicy", "testIamPermissions":
			return serveIAM(w, req, s, name, verb)
		default:
			return apierror.InvalidArgument("POST to a folder requires a custom method: move, undelete or an IAM method")
		}
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	r.Handle("GET /v3/organizations:search", func(w http.ResponseWriter, req *http.Request) error {
		resp, err := orgs.SearchOrganizations(req.Context(), &rmpb.SearchOrganizationsRequest{Query: req.URL.Query().Get("query")})
		if err != nil {
			return err
		}
		return writeProto(w, resp)
	})
	r.Handle("GET /v3/organizations/{org}", func(w http.ResponseWriter, req *http.Request) error {
		id, err := rest.PathValue(req, "org")
		if err != nil {
			return err
		}
		o, err := orgs.GetOrganization(req.Context(), &rmpb.GetOrganizationRequest{Name: "organizations/" + id})
		if err != nil {
			return err
		}
		return writeProto(w, o)
	})
	r.Handle("POST /v3/organizations/{org}", func(w http.ResponseWriter, req *http.Request) error {
		raw, err := rest.PathValue(req, "org")
		if err != nil {
			return err
		}
		id, verb, _ := strings.Cut(raw, ":")
		if isIAMVerb(verb) {
			return serveIAM(w, req, orgs, "organizations/"+id, verb)
		}
		return apierror.InvalidArgument("POST to an organization requires a custom method")
	})
	r.Handle("GET /v3/operations/{operation...}", func(w http.ResponseWriter, req *http.Request) error {
		name := "operations/" + req.PathValue("operation")
		op, ok := v3Operations.get(name)
		if !ok {
			if op1, ok := v1ops.get(name); ok {
				return rest.WriteJSON(w, http.StatusOK, op1)
			}
			return apierror.NotFound("operation %s not found", name)
		}
		return writeProto(w, op)
	})
}

// --- v2 REST (gcloud resource-manager folders) -------------------------

func v2Folder(f Folder) *crmv2.Folder {
	return &crmv2.Folder{Name: f.Name, Parent: f.Parent, DisplayName: f.DisplayName,
		LifecycleState: string(f.State), CreateTime: f.CreateTime}
}

func (s *FoldersServer) v2Routes(r *rest.Router, ops *v1Ops) {
	op := func(w http.ResponseWriter, kind string, f Folder) error {
		resp, _ := json.Marshal(struct {
			Type string `json:"@type"`
			*crmv2.Folder
		}{"type.googleapis.com/google.cloud.resourcemanager.v2.Folder", v2Folder(f)})
		o := &crmv1.Operation{Name: fmt.Sprintf("operations/%s.%s.%d", kind, f.ID(), time.Now().UnixNano()), Done: true, Response: resp}
		ops.put(o)
		return rest.WriteJSON(w, http.StatusOK, o)
	}
	list := func(w http.ResponseWriter, scope string, match func(Folder) bool, token string, size int) error {
		all, err := s.reg.Folders()
		if err != nil {
			return err
		}
		byName := map[string]Folder{}
		var names []string
		for _, f := range all {
			if match(f) {
				byName[f.Name] = f
				names = append(names, f.Name)
			}
		}
		page, next, err := paging.Page(scope, names, token, size)
		if err != nil {
			return apierror.InvalidArgument("%v", err)
		}
		resp := struct {
			Folders       []*crmv2.Folder `json:"folders"`
			NextPageToken string          `json:"nextPageToken,omitempty"`
		}{Folders: []*crmv2.Folder{}, NextPageToken: next}
		for _, n := range page {
			resp.Folders = append(resp.Folders, v2Folder(byName[n]))
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	}
	r.Handle("GET /v2/folders", func(w http.ResponseWriter, req *http.Request) error {
		q := req.URL.Query()
		parent, show := q.Get("parent"), q.Get("showDeleted") == "true"
		if parent == "" {
			return apierror.InvalidArgument("parent is required: folders/{folder_number} or %s", OrganizationName)
		}
		if err := s.reg.CheckParentExists(parent); err != nil {
			return err
		}
		size, err := rest.QueryInt(req, "pageSize", 0)
		if err != nil {
			return err
		}
		return list(w, fmt.Sprintf("folders.v2.list:%s:%v", parent, show), func(f Folder) bool {
			return f.Parent == parent && (show || f.State == StateActive)
		}, q.Get("pageToken"), size)
	})
	r.Handle("POST /v2/folders:search", func(w http.ResponseWriter, req *http.Request) error {
		var in crmv2.SearchFoldersRequest
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		match, _, err := parseFolderQuery(in.Query)
		if err != nil {
			return err
		}
		return list(w, "folders.v2.search:"+in.Query, match, in.PageToken, int(in.PageSize))
	})
	r.Handle("POST /v2/folders", func(w http.ResponseWriter, req *http.Request) error {
		var in crmv2.Folder
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		f, err := s.reg.CreateFolder(req.URL.Query().Get("parent"), in.DisplayName)
		if err != nil {
			return err
		}
		return op(w, "cf", f)
	})
	r.Handle("GET /v2/folders/{folder}", func(w http.ResponseWriter, req *http.Request) error {
		id, err := rest.PathValue(req, "folder")
		if err != nil {
			return err
		}
		f, err := s.reg.Folder(id)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, v2Folder(f))
	})
	r.Handle("PATCH /v2/folders/{folder}", func(w http.ResponseWriter, req *http.Request) error {
		id, err := rest.PathValue(req, "folder")
		if err != nil {
			return err
		}
		var in crmv2.Folder
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		for _, p := range strings.Split(req.URL.Query().Get("updateMask"), ",") {
			if p != "" && p != "displayName" && p != "display_name" {
				return apierror.InvalidArgument("updateMask path %q cannot be updated; only displayName", p)
			}
		}
		f, err := s.reg.RenameFolder(id, in.DisplayName)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, v2Folder(f))
	})
	r.Handle("DELETE /v2/folders/{folder}", func(w http.ResponseWriter, req *http.Request) error {
		id, err := rest.PathValue(req, "folder")
		if err != nil {
			return err
		}
		f, err := s.reg.DeleteFolder(id)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, v2Folder(f))
	})
	r.Handle("POST /v2/folders/{folder}", func(w http.ResponseWriter, req *http.Request) error {
		raw, err := rest.PathValue(req, "folder")
		if err != nil {
			return err
		}
		id, verb, _ := strings.Cut(raw, ":")
		switch verb {
		case "move":
			var in crmv2.MoveFolderRequest
			if err := rest.DecodeJSON(req, &in); err != nil {
				return err
			}
			f, err := s.reg.MoveFolder(id, in.DestinationParent)
			if err != nil {
				return err
			}
			return op(w, "mf", f)
		case "undelete":
			f, err := s.reg.UndeleteFolder(id)
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, v2Folder(f))
		case "getIamPolicy", "setIamPolicy", "testIamPermissions":
			return serveIAM(w, req, s, "folders/"+strings.TrimPrefix(id, "folders/"), verb)
		}
		return apierror.InvalidArgument("POST to a folder requires a custom method: move, undelete or an IAM method")
	})
}

// --- v1 organizations and getAncestry ---------------------------------

func v1Organization() *crmv1.Organization {
	return &crmv1.Organization{Name: OrganizationName, DisplayName: OrganizationDomain,
		Owner:          &crmv1.OrganizationOwner{DirectoryCustomerId: OrganizationCustomerID},
		LifecycleState: "ACTIVE", CreationTime: OrganizationCreateTime}
}

func (s *ProjectsServer) v1OrgRoutes(r *rest.Router, orgs OrganizationsServer) {
	r.Handle("GET /v1/organizations/{org}", func(w http.ResponseWriter, req *http.Request) error {
		id, err := rest.PathValue(req, "org")
		if err != nil {
			return err
		}
		if err := checkOrg("organizations/" + id); err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, v1Organization())
	})
	r.Handle("POST /v1/organizations:search", func(w http.ResponseWriter, req *http.Request) error {
		var in crmv1.SearchOrganizationsRequest
		if req.ContentLength != 0 {
			if err := rest.DecodeJSON(req, &in); err != nil {
				return err
			}
		}
		// v1's filter is the v3 query's terms: domain: and
		// owner.directoryCustomerId:.
		ok, err := orgMatches(in.Filter)
		if err != nil {
			return err
		}
		resp := crmv1.SearchOrganizationsResponse{Organizations: []*crmv1.Organization{}}
		if ok {
			resp.Organizations = append(resp.Organizations, v1Organization())
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	r.Handle("POST /v1/organizations/{org}", func(w http.ResponseWriter, req *http.Request) error {
		raw, err := rest.PathValue(req, "org")
		if err != nil {
			return err
		}
		id, verb, _ := strings.Cut(raw, ":")
		if isIAMVerb(verb) {
			return serveIAM(w, req, orgs, "organizations/"+id, verb)
		}
		return s.v1OrgPolicy(w, req, "organizations/"+id, verb)
	})
	// v1 has six folder methods, all org policy (#1534, orgpolicy.go).
	r.Handle("POST /v1/folders/{folder}", func(w http.ResponseWriter, req *http.Request) error {
		raw, err := rest.PathValue(req, "folder")
		if err != nil {
			return err
		}
		id, verb, _ := strings.Cut(raw, ":")
		return s.v1OrgPolicy(w, req, "folders/"+id, verb)
	})
}

// v1Ancestry is projects.getAncestry: the project, its folders, and the
// organization, nearest first.
func (s *ProjectsServer) v1Ancestry(w http.ResponseWriter, id string) error {
	chain, err := s.reg.Ancestry(id)
	if err != nil {
		return err
	}
	resp := crmv1.GetAncestryResponse{}
	for _, a := range chain {
		resp.Ancestor = append(resp.Ancestor, &crmv1.Ancestor{ResourceId: &crmv1.ResourceId{Type: a.Type, Id: a.ID}})
	}
	return rest.WriteJSON(w, http.StatusOK, resp)
}

// v1Parent is a v1 ResourceId parent as a v3 name.
func v1Parent(p *crmv1.ResourceId) (string, error) {
	if p == nil || p.Id == "" {
		return "", nil
	}
	switch p.Type {
	case "folder":
		return "folders/" + p.Id, nil
	case "organization":
		return "organizations/" + p.Id, nil
	}
	return "", apierror.InvalidArgument("parent type %q must be folder or organization", p.Type)
}

// v1ParentOf is a v3 parent name as v1's ResourceId.
func v1ParentOf(parent string) *crmv1.ResourceId {
	if id, ok := strings.CutPrefix(parent, "folders/"); ok {
		return &crmv1.ResourceId{Type: "folder", Id: id}
	}
	if id, ok := strings.CutPrefix(parent, "organizations/"); ok {
		return &crmv1.ResourceId{Type: "organization", Id: id}
	}
	return nil
}
