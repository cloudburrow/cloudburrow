package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// The eleventh round of IAM enforcement (#1879): Resource Manager v3, the
// IAM API v1 (roles, service accounts, keys and their policies), IAM v2
// deny policies and the Organization Policy API v2, all on Resource
// Manager's port, over gRPC and JSON. The instance's own account, the
// creator of every project, holds every permission no deny policy
// refuses it, whatever a policy it set says, so it can always repair a
// policy it broke; and none of iam.denypolicies.* can be denied, so it
// can always delete a deny policy that locks it out.
func useIAMEnforcement11(e *iameval.Enforcer, rm *resourcemanager.Server, reg func() *resourcemanager.Registry) {
	if e == nil || rm == nil || e.On == nil || !e.On() {
		return
	}
	src := registrySource{reg: reg}
	// Tag keys and values keep their own policies.
	e.Policies[iameval.ResourceManager.Name] = func(_ context.Context, resource string) *iampolicy.Stored {
		return src.Policy(resource)
	}
	if e.Scopes == nil {
		e.Scopes = map[string]func(string) iameval.Scope{}
	}
	e.Scopes[iameval.ResourceManager.Name] = func(resource string) iameval.Scope { return tagScope(reg(), resource) }
	e.Scopes[iameval.IAM.Name] = func(resource string) iameval.Scope { return iamScope(reg(), resource) }
	e.ParentOf = func(resource string) string { return parentOf(src, resource) }
	for _, svc := range []*iameval.Service{iameval.ResourceManager, iameval.IAM, iameval.DenyPolicies, iameval.OrgPolicy} {
		rm.Interpose(e.Interceptor(svc))
		rm.WrapJSON(jsonEnforcer(e, svc, nil))
	}
	rm.WrapJSON(rmV1Enforcer(e, reg))
}

// parentOf is a project's or folder's current parent, "" for none.
func parentOf(src registrySource, resource string) string {
	reg := src.registry()
	if reg == nil {
		return ""
	}
	switch {
	case strings.HasPrefix(resource, "folders/"):
		if f, err := reg.Folder(resource); err == nil {
			return f.Parent
		}
	case strings.HasPrefix(resource, "projects/"):
		if p, err := reg.Get(src.projectID(reg, strings.TrimPrefix(resource, "projects/"))); err == nil {
			return p.Parent
		}
	}
	return ""
}

// tagScope is what applies to a tag key or value (or a hold on one): its
// own policy, a value's key's, and the key's parent's ancestry. A
// namespaced name ("ORG/KEY", "PROJECT/KEY/VALUE") is the key or value it
// names.
func tagScope(reg *resourcemanager.Registry, resource string) iameval.Scope {
	if reg == nil {
		return iameval.Scope{}
	}
	name := resource
	if i := strings.Index(name, "/tagHolds/"); i >= 0 {
		name = name[:i]
	}
	collection, _, _ := strings.Cut(name, "/")
	switch collection {
	case "tagKeys", "tagValues":
	case "projects", "folders", "organizations", "tagBindings", "":
		return iameval.Scope{}
	default:
		// A namespaced name.
		if strings.Count(name, "/") == 2 {
			v, err := reg.NamespacedTagValue(name)
			if err != nil {
				return iameval.Scope{}
			}
			name = v.GetName()
		} else {
			k, err := reg.NamespacedTagKey(name)
			if err != nil {
				return iameval.Scope{}
			}
			name = k.GetName()
		}
	}
	sc := iameval.Scope{Resource: name}
	key := name
	if strings.HasPrefix(name, "tagValues/") {
		v, err := reg.TagValue(name)
		if err != nil {
			return iameval.Scope{}
		}
		key = v.GetParent()
		sc.Own = []string{name}
	}
	k, err := reg.TagKey(key)
	if err != nil {
		return iameval.Scope{}
	}
	sc.Own = append(sc.Own, key)
	sc.Parent = k.GetParent()
	return sc
}

// iamScope is what applies to a service account or one of its keys named
// by its project, "-" or its unique ID: its own policy and its project's
// ancestry; and to a deny policy, its attachment point.
func iamScope(reg *resourcemanager.Registry, resource string) iameval.Scope {
	if sc, ok := workforceScope(reg, resource); ok {
		// A workforce pool's (#2056, iamenforce_workforce.go).
		return sc
	}
	if strings.HasPrefix(resource, "policies/") {
		if a := iameval.DenyAttachment(resource); a != "" {
			return iameval.Scope{Resource: a, Own: []string{}, Parent: a}
		}
		return iameval.Scope{}
	}
	parts := strings.Split(resource, "/")
	if len(parts) < 4 || parts[0] != "projects" || parts[2] != "serviceAccounts" {
		return iameval.Scope{}
	}
	account := strings.Join(parts[:4], "/")
	suffix := strings.Join(parts[4:], "/")
	canonical, project := "", ""
	if reg != nil {
		if sa, err := reg.GetServiceAccount(account); err == nil {
			canonical, project = sa.GetName(), sa.GetProjectId()
		}
	}
	if canonical == "" {
		canonical = iameval.ServiceAccountResource(parts[1], parts[3])
		project = iameval.Project(canonical)
	}
	if project == "" || project == "-" {
		return iameval.Scope{}
	}
	sc := iameval.Scope{Resource: canonical, Own: []string{canonical}, Parent: "projects/" + project}
	if suffix != "" {
		sc.Resource = canonical + "/" + suffix
	}
	return sc
}

// rmV1Resource is a Resource Manager v1 resource path and its custom verb.
var (
	rmV1Resource   = regexp.MustCompile(`^/v1/((?:projects|organizations|folders)/[^/:]+)(?::([A-Za-z]+))?$`)
	rmLienPath     = regexp.MustCompile(`^/v1/(liens/[^/:]+)$`)
	rmCapabilities = regexp.MustCompile(`^/v3/(folders/[^/]+/capabilities/[^/:]+)$`)
)

// rmV1Verbs are Resource Manager v1's custom methods and their permissions
// (https://docs.cloud.google.com/resource-manager/reference/rest/v1, read
// 2026-11-20); "%s" is the resource's kind (projects, organizations).
var rmV1Verbs = map[string]string{
	"undelete":                          "resourcemanager.projects.undelete",
	"getAncestry":                       "resourcemanager.projects.get",
	"getIamPolicy":                      "resourcemanager.%s.getIamPolicy",
	"setIamPolicy":                      "resourcemanager.%s.setIamPolicy",
	"getOrgPolicy":                      "orgpolicy.policy.get",
	"getEffectiveOrgPolicy":             "orgpolicy.policy.get",
	"listOrgPolicies":                   "orgpolicy.policy.get",
	"listAvailableOrgPolicyConstraints": "orgpolicy.policy.get",
	"setOrgPolicy":                      "orgpolicy.policy.set",
	"clearOrgPolicy":                    "orgpolicy.policy.set",
}

// rmV1Enforcer checks Resource Manager v1's JSON API (projects, org
// policies v1, liens) and the v3 folder capabilities, which no proto
// binds: projects.list and organizations.search answer only what the
// caller may get ("Search will only return organizations on which the user
// has the permission resourcemanager.organizations.get"); a lien's create
// and delete need resourcemanager.projects.updateLiens and its list and get
// resourcemanager.projects.get on its parent; a folder's capability
// resourcemanager.capabilities.get or .update.
func rmV1Enforcer(e *iameval.Enforcer, reg func() *resourcemanager.Registry) func(http.Handler) http.Handler {
	svc := iameval.ResourceManager
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := iameval.Bearer(r.Header.Get("Authorization"))
			check := func(permission, resource string) bool {
				if err := e.Authorize(r.Context(), svc, b, permission, resource); err != nil {
					iameval.WriteJSONStatus(w, err)
					return false
				}
				return true
			}
			p := r.URL.Path
			switch {
			case p == "/v1/projects" && r.Method == http.MethodGet:
				e.FilterJSON(w, r, svc, "resourcemanager.projects.get", "projects", next)
				return
			case p == "/v1/organizations:search" && r.Method == http.MethodPost:
				e.FilterJSON(w, r, svc, "resourcemanager.organizations.get", "organizations", next)
				return
			case p == "/v1/projects" && r.Method == http.MethodPost:
				if parent := v1Parent(r); parent != "" && !check("resourcemanager.projects.create", parent) {
					return
				}
			case p == "/v1/liens" && r.Method == http.MethodGet:
				if parent := r.URL.Query().Get("parent"); parent != "" && !check("resourcemanager.projects.get", parent) {
					return
				}
			case p == "/v1/liens" && r.Method == http.MethodPost:
				if parent := bodyField(r, "parent"); parent != "" && !check("resourcemanager.projects.updateLiens", parent) {
					return
				}
			case rmLienPath.MatchString(p):
				perm := "resourcemanager.projects.get"
				if r.Method == http.MethodDelete {
					perm = "resourcemanager.projects.updateLiens"
				}
				if rg := reg(); rg != nil {
					if l, err := rg.GetLien(rmLienPath.FindStringSubmatch(p)[1]); err == nil && !check(perm, l.Parent) {
						return
					}
				}
			case rmCapabilities.MatchString(p):
				perm := "resourcemanager.capabilities.get"
				if r.Method == http.MethodPatch {
					perm = "resourcemanager.capabilities.update"
				}
				if !check(perm, rmCapabilities.FindStringSubmatch(p)[1]) {
					return
				}
			default:
				g := rmV1Resource.FindStringSubmatch(p)
				if g == nil {
					break
				}
				res, verb := g[1], g[2]
				kind, _, _ := strings.Cut(res, "/")
				perm := ""
				switch {
				case verb == "testIamPermissions":
					e.TestPermissionsJSON(w, r, svc, res, next)
					return
				case verb != "":
					perm = rmV1Verbs[verb]
					if strings.Contains(perm, "%s") {
						perm = fmt.Sprintf(perm, kind)
					}
				case kind == "folders":
				case r.Method == http.MethodGet:
					perm = "resourcemanager." + kind + ".get"
				case r.Method == http.MethodPut && kind == "projects":
					perm = "resourcemanager.projects.update"
				case r.Method == http.MethodDelete && kind == "projects":
					perm = "resourcemanager.projects.delete"
				}
				if perm != "" && !check(perm, res) {
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// v1Parent is a v1 project's parent ({"type": "organization", "id": "1"})
// as a resource name, "" for none.
func v1Parent(r *http.Request) string {
	var body struct {
		Parent struct{ Type, ID string } `json:"parent"`
	}
	if !peekBody(r, &body) || body.Parent.ID == "" {
		return ""
	}
	switch body.Parent.Type {
	case "organization":
		return "organizations/" + body.Parent.ID
	case "folder":
		return "folders/" + body.Parent.ID
	}
	return ""
}

// bodyField is a top-level string field of a JSON request body.
func bodyField(r *http.Request, field string) string {
	var body map[string]any
	if !peekBody(r, &body) {
		return ""
	}
	s, _ := body[field].(string)
	return s
}

// peekBody decodes r's body into v and leaves it for the handler.
func peekBody(r *http.Request, v any) bool {
	if r.Body == nil {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(b))
	return err == nil && json.Unmarshal(b, v) == nil
}
