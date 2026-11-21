package main

// The IAM page (#1864), as Google's console draws it: the selected
// project's principals with their roles, the roles inherited from its
// folders and organization beside them, View by principals and View by
// roles, Grant access (several principals, several roles, each with an
// optional IAM condition) in a side panel, Edit principal (add, change and
// remove a principal's roles in one save) and Remove access. The screen
// sends these through console.Studio calls, each a read or one
// SetIamPolicy of Resource Manager's own Projects server with the etag the
// page read, so a policy changed since is refused with the API's message
// rather than overwritten.
//
//	policy  the project's policy (version 3), its ancestors' bindings, the
//	        display names of the project's service accounts
//	roles   every role the role picker offers: the project's and the
//	        organization's custom roles, then Google's predefined roles,
//	        each with its product, Select a role's group (#2013)
//	change  {etag, members, add: [{role, condition}], remove: [...]}

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// iamPageInherited is one ancestor's bindings.
type iamPageInherited struct {
	// Resource is folders/N or organizations/N; Display is its name.
	Resource string            `json:"resource"`
	Display  string            `json:"display"`
	Bindings []console.Binding `json:"bindings"`
}

type iamPagePolicy struct {
	Project  string `json:"project"`
	Resource string `json:"resource"`
	// Kind and Display name a folder or the organization chosen in the
	// toolbar (#2023): "folder" or "organization", and its name.
	Kind      string             `json:"kind,omitempty"`
	Display   string             `json:"display,omitempty"`
	Etag      string             `json:"etag"`
	Bindings  []console.Binding  `json:"bindings"`
	Inherited []iamPageInherited `json:"inherited"`
	// Names are the display names Google's Name column shows: the
	// project's service accounts'.
	Names map[string]string `json:"names"`
	Note  string            `json:"note"`
}

type iamPageRole struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Stage       string `json:"stage,omitempty"`
	// Type is Predefined, Custom or Basic.
	Type string `json:"type"`
	// Product is Select a role's group (#2013): the product Google's
	// docs list a predefined role under, "Basic" for a basic role and
	// "Custom" for a custom role.
	Product string `json:"product"`
}

// customRoleProduct is the custom roles' group in Select a role.
const customRoleProduct = "Custom"

type iamPageRoleChange struct {
	Role      string             `json:"role"`
	Condition *console.Condition `json:"condition"`
}

type iamPageChange struct {
	Etag    string              `json:"etag"`
	Members []string            `json:"members"`
	Add     []iamPageRoleChange `json:"add"`
	Remove  []iamPageRoleChange `json:"remove"`
}

var basicRoles = map[string]bool{"roles/owner": true, "roles/editor": true, "roles/viewer": true, "roles/browser": true}

func (p projectIAMProvider) StudioCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	// A folder or the organization chosen in the toolbar's resource
	// selector is the page's resource (#2023).
	sel, scoped, err := consoleResourceScope(ctx, p.registry)
	if err != nil {
		return nil, err
	}
	var selp *selectedResource
	if scoped {
		selp = &sel
	}
	if project == "" && !scoped {
		return nil, errors.New("choose a project in the toolbar")
	}
	switch call {
	case "policy":
		if scoped {
			return p.pagePolicyOf(ctx, sel)
		}
		return p.pagePolicy(ctx, project)
	case "roles":
		return p.pageRoles(project)
	case "deny-list", "deny-get", "deny-create", "deny-update", "deny-delete":
		return p.denyCall(project, selp, call, body)
	case "troubleshoot", "analyze", "enforcement", "analyze-export", "analyze-operation", "saved-list", "saved-create", "saved-delete":
		return p.analysisCall(ctx, project, call, body)
	case "change":
		var c iamPageChange
		if err := json.Unmarshal(body, &c); err != nil {
			return nil, fmt.Errorf("malformed change: %v", err)
		}
		if scoped {
			return p.pageChangeOn(ctx, iamServerOf(p.registry, sel.Name), sel.Name, c)
		}
		return p.pageChange(ctx, project, c)
	}
	return nil, fmt.Errorf("no such call %q", call)
}

func (p projectIAMProvider) pagePolicy(ctx context.Context, project string) (iamPagePolicy, error) {
	pol, err := getProtoPolicy(ctx, p.api(), resourcemanager.V3Name(project))
	if err != nil {
		return iamPagePolicy{}, err
	}
	out := iamPagePolicy{Project: project, Resource: "//cloudresourcemanager.googleapis.com/projects/" + project,
		Etag: pol.Etag, Bindings: pol.Bindings, Inherited: []iamPageInherited{}, Names: map[string]string{}, Note: projectIAMNote}
	if chain, err := p.registry.Ancestry(project); err == nil {
		for _, a := range chain[1:] {
			var in iamPageInherited
			var api iamServer
			switch a.Type {
			case "folder":
				in.Resource, in.Display = "folders/"+a.ID, "folders/"+a.ID
				if f, err := p.registry.Folder(in.Resource); err == nil && f.DisplayName != "" {
					in.Display = f.DisplayName
				}
				api = resourcemanager.NewFoldersServer(p.registry)
			case "organization":
				in.Resource, in.Display = resourcemanager.OrganizationName, resourcemanager.OrganizationName
				api = resourcemanager.NewOrganizationsServer(p.registry)
			default:
				continue
			}
			ap, err := getProtoPolicy(ctx, api, in.Resource)
			if err != nil {
				continue
			}
			in.Bindings = ap.Bindings
			out.Inherited = append(out.Inherited, in)
		}
	}
	resp, err := p.registry.ListServiceAccounts(&adminpb.ListServiceAccountsRequest{Name: "projects/" + project, PageSize: 100})
	for err == nil {
		for _, a := range resp.GetAccounts() {
			if a.GetDisplayName() != "" {
				out.Names["serviceAccount:"+a.GetEmail()] = a.GetDisplayName()
			}
		}
		if resp.GetNextPageToken() == "" {
			break
		}
		resp, err = p.registry.ListServiceAccounts(&adminpb.ListServiceAccountsRequest{Name: "projects/" + project, PageSize: 100, PageToken: resp.GetNextPageToken()})
	}
	return out, nil
}

func (p projectIAMProvider) pageRoles(project string) ([]iamPageRole, error) {
	custom, err := iamRolesProvider{registry: p.registry}.roles(project)
	if err != nil {
		return nil, err
	}
	out := []iamPageRole{}
	for _, r := range custom {
		if r.GetDeleted() || r.GetStage() == adminpb.Role_DISABLED {
			continue
		}
		out = append(out, iamPageRole{Name: r.GetName(), Title: r.GetTitle(), Description: r.GetDescription(),
			Stage: r.GetStage().String(), Type: "Custom", Product: customRoleProduct})
	}
	for _, n := range iamroles.PredefinedNames() {
		r := iamroles.Predefined(n)
		kind, product := "Predefined", r.Product
		if basicRoles[n] {
			kind, product = "Basic", iamroles.BasicProduct
		}
		out = append(out, iamPageRole{Name: n, Title: r.Title, Description: r.Description, Stage: r.Stage, Type: kind, Product: product})
	}
	return out, nil
}

// pagePolicyOf is the policy of the folder or organization chosen in the
// toolbar (#2023), read through Folders' or Organizations' GetIamPolicy,
// with the bindings of the folders and organization above it.
func (p projectIAMProvider) pagePolicyOf(ctx context.Context, sel selectedResource) (iamPagePolicy, error) {
	pol, err := getProtoPolicy(ctx, iamServerOf(p.registry, sel.Name), sel.Name)
	if err != nil {
		return iamPagePolicy{}, err
	}
	out := iamPagePolicy{Resource: "//cloudresourcemanager.googleapis.com/" + sel.Name, Kind: strings.ToLower(sel.Kind), Display: sel.Display,
		Etag: pol.Etag, Bindings: pol.Bindings, Inherited: []iamPageInherited{}, Names: map[string]string{}, Note: projectIAMNote}
	for _, a := range ancestorsOf(p.registry, sel.Name) {
		ap, err := getProtoPolicy(ctx, iamServerOf(p.registry, a.Name), a.Name)
		if err != nil {
			continue
		}
		out.Inherited = append(out.Inherited, iamPageInherited{Resource: a.Name, Display: a.Display, Bindings: ap.Bindings})
	}
	return out, nil
}

// pageChange applies Grant access or Edit principal in one SetIamPolicy:
// each removal first, then each grant, for every member named.
func (p projectIAMProvider) pageChange(ctx context.Context, project string, c iamPageChange) (map[string]string, error) {
	return p.pageChangeOn(ctx, p.api(), resourcemanager.V3Name(project), c)
}

// pageChangeOn is pageChange on resource through api: a project's through
// Projects, a folder's or the organization's through theirs (#2023).
func (p projectIAMProvider) pageChangeOn(ctx context.Context, api iamServer, resource string, c iamPageChange) (map[string]string, error) {
	if c.Etag == "" {
		return nil, errors.New("etag is required: send the one the policy was read with")
	}
	members := []string{}
	for _, m := range c.Members {
		if m = strings.TrimSpace(m); m != "" {
			members = append(members, m)
		}
	}
	if len(members) == 0 {
		return nil, errors.New("name at least one principal")
	}
	if len(c.Add) == 0 && len(c.Remove) == 0 {
		return nil, errors.New("choose a role to grant or remove")
	}
	pol, err := getProtoPolicy(ctx, api, resource)
	if err != nil {
		return nil, err
	}
	for _, r := range c.Remove {
		for _, m := range members {
			if pol, err = console.RemoveConditional(pol, r.Role, m, r.Condition); err != nil {
				return nil, err
			}
		}
	}
	for _, a := range c.Add {
		if strings.TrimSpace(a.Role) == "" {
			return nil, errors.New("select a role")
		}
		if pol, err = console.GrantConditional(pol, strings.TrimSpace(a.Role), members, a.Condition); err != nil {
			return nil, err
		}
	}
	pol.Etag = c.Etag
	if err := setProtoPolicy(ctx, api, resource, pol); err != nil {
		return nil, err
	}
	return map[string]string{"applied": "change"}, nil
}

var _ console.Studio = projectIAMProvider{}
