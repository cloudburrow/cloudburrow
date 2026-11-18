package resourcemanager

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	ci "google.golang.org/api/cloudidentity/v1"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The Cloud Identity v1 JSON surface of the groups (groups.go), served on
// Resource Manager's port beside the IAM API, at the paths
// cloudidentity.googleapis.com serves: /v1/groups... The shapes are those of
// the official discovery client, google.golang.org/api/cloudidentity/v1.
// Mutations answer a done Operation whose response is the resource, as
// Google's do. Served: groups create/get/list/lookup/patch/delete/search;
// memberships create/get/list/lookup/delete, checkTransitiveMembership,
// searchTransitiveGroups, searchDirectGroups, searchTransitiveMemberships,
// getMembershipGraph and modifyMembershipRoles (groups_search.go). The
// security settings are refused 501, and so are devices, customers,
// inbound SSO and the policies.

const (
	groupType      = "type.googleapis.com/google.apps.cloudidentity.groups.v1.Group"
	membershipType = "type.googleapis.com/google.apps.cloudidentity.groups.v1.Membership"
)

// IsCloudIdentityPath is whether path is Cloud Identity's.
func IsCloudIdentityPath(path string) bool {
	return path == "/v1/groups" || strings.HasPrefix(path, "/v1/groups/") || strings.HasPrefix(path, "/v1/groups:")
}

func ciTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func groupJSON(g *Group, full bool) *ci.Group {
	out := &ci.Group{Name: g.Name(), GroupKey: &ci.EntityKey{Id: g.Email}, Parent: CustomerName,
		DisplayName: g.DisplayName, Labels: g.Labels}
	if full {
		out.Description = g.Description
		out.CreateTime, out.UpdateTime = ciTime(g.CreateTime), ciTime(g.UpdateTime)
	}
	return out
}

func membershipJSON(g *Group, m GroupMember, full bool) *ci.Membership {
	out := &ci.Membership{Name: MembershipName(g, m), PreferredMemberKey: &ci.EntityKey{Id: m.Email}, Type: m.Type}
	for _, r := range m.Roles {
		role := &ci.MembershipRole{Name: r}
		if r == "MEMBER" && m.ExpireTime != nil {
			role.ExpiryDetail = &ci.ExpiryDetail{ExpireTime: ciTime(*m.ExpireTime)}
		}
		out.Roles = append(out.Roles, role)
	}
	if full {
		out.CreateTime, out.UpdateTime = ciTime(m.CreateTime), ciTime(m.UpdateTime)
		out.DeliverySetting = "ALL_MAIL"
	}
	return out
}

// doneOp is a finished Operation with v, of type typ, as its response.
func doneOp(w http.ResponseWriter, typ string, v any) error {
	var resp json.RawMessage
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return apierror.Internal(err, "encode response")
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		m["@type"] = typ
		resp, _ = json.Marshal(m)
	} else {
		resp = json.RawMessage(`{"@type":"type.googleapis.com/google.protobuf.Empty"}`)
	}
	return rest.WriteJSON(w, http.StatusOK, map[string]any{"done": true, "response": resp})
}

func view(r *http.Request) (bool, error) {
	switch v := r.URL.Query().Get("view"); v {
	case "", "VIEW_UNSPECIFIED", "BASIC":
		return false, nil
	case "FULL":
		return true, nil
	default:
		return false, apierror.InvalidArgument("Invalid value at 'view' (type.googleapis.com/google.apps.cloudidentity.groups.v1.View), %q", v)
	}
}

// pageOf pages n items of a listing by their index keys.
func pageOf(r *http.Request, scope string, keys []string, def, max int) ([]string, string, error) {
	size, err := rest.QueryInt(r, "pageSize", 0)
	if err != nil {
		return nil, "", err
	}
	if size < 0 {
		return nil, "", apierror.InvalidArgument("pageSize must not be negative")
	}
	if size == 0 {
		size = def
	}
	if size > max {
		return nil, "", apierror.InvalidArgument("pageSize must not be greater than %d", max)
	}
	page, next, err := paging.Page(scope, keys, r.URL.Query().Get("pageToken"), size)
	if err != nil {
		return nil, "", apierror.InvalidArgument("Invalid pageToken: %v", err)
	}
	return page, next, nil
}

// identityQuery reads a transitive search's CEL query:
// member_key_id == 'x' [&& member_key_namespace == 'y'] [&& 'label' in labels] [&& parent == 'customers/C...'].
type identityQuery struct {
	member, namespace, parent string
	labels                    []string
}

var (
	reEq    = regexp.MustCompile(`^(member_key_id|member_key_namespace|parent)\s*==\s*'([^']*)'$`)
	reLabel = regexp.MustCompile(`^'([^']+)'\s+in\s+labels$`)
)

func parseIdentityQuery(q string) (identityQuery, error) {
	var out identityQuery
	if strings.TrimSpace(q) == "" {
		return out, apierror.InvalidArgument("Request contains an invalid argument: query is required.")
	}
	for _, term := range strings.Split(q, "&&") {
		term = strings.TrimSpace(term)
		if m := reEq.FindStringSubmatch(term); m != nil {
			switch m[1] {
			case "member_key_id":
				out.member = m[2]
			case "member_key_namespace":
				out.namespace = m[2]
			case "parent":
				out.parent = m[2]
			}
			continue
		}
		if m := reLabel.FindStringSubmatch(term); m != nil {
			out.labels = append(out.labels, m[1])
			continue
		}
		return out, apierror.InvalidArgument("Invalid query %q: the term %q is not member_key_id == '...', member_key_namespace == '...', parent == '...' or '...' in labels.", q, term)
	}
	if out.member == "" {
		return out, apierror.InvalidArgument("Invalid query %q: it must include member_key_id.", q)
	}
	return out, nil
}

// identityRoutes registers Cloud Identity's group paths.
func (r *Registry) identityRoutes(rt *rest.Router) {
	rt.Handle("POST /v1/groups", func(w http.ResponseWriter, req *http.Request) error {
		switch c := req.URL.Query().Get("initialGroupConfig"); c {
		case "", "INITIAL_GROUP_CONFIG_UNSPECIFIED", "EMPTY", "WITH_INITIAL_OWNER":
		default:
			return apierror.InvalidArgument("Invalid value at 'initial_group_config', %q", c)
		}
		var g ci.Group
		if err := rest.DecodeJSON(req, &g); err != nil {
			return err
		}
		in := GroupInput{Parent: g.Parent, DisplayName: g.DisplayName, Description: g.Description, Labels: g.Labels,
			Dynamic: g.DynamicGroupMetadata != nil}
		if g.GroupKey != nil {
			in.Email, in.Namespace = g.GroupKey.Id, g.GroupKey.Namespace
		}
		if _, ok := g.Labels[labelDynamic]; ok {
			in.Dynamic = true
		}
		initial := req.URL.Query().Get("initialGroupConfig")
		out, err := r.CreateGroup(in)
		if err != nil {
			return err
		}
		if initial == "WITH_INITIAL_OWNER" {
			// "The end user making the request will be added as the
			// initial owner of the Group": the account the bearer token
			// was minted for. A token the instance minted for no account
			// (its admin token) names no end user, and adds no owner.
			bearer, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
			if caller, ok := r.Caller(strings.TrimSpace(bearer)); ok && caller != out.Email {
				if _, _, err := r.CreateMembership(out.Name(), caller, "", []string{"OWNER"}); err != nil {
					return err
				}
			}
		}
		return doneOp(w, groupType, groupJSON(out, true))
	})
	rt.Handle("GET /v1/groups", func(w http.ResponseWriter, req *http.Request) error {
		full, err := view(req)
		if err != nil {
			return err
		}
		parent := req.URL.Query().Get("parent")
		gs, err := r.ListGroups(parent)
		if err != nil {
			return err
		}
		max := 1000
		def := 200
		if full {
			max, def = 500, 50
		}
		byKey := map[string]*Group{}
		keys := make([]string, 0, len(gs))
		for _, g := range gs {
			byKey[g.Email] = g
			keys = append(keys, g.Email)
		}
		page, next, err := pageOf(req, "groups:"+parent, keys, def, max)
		if err != nil {
			return err
		}
		resp := ci.ListGroupsResponse{NextPageToken: next, Groups: []*ci.Group{}}
		for _, k := range page {
			resp.Groups = append(resp.Groups, groupJSON(byKey[k], full))
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("GET /v1/groups:lookup", func(w http.ResponseWriter, req *http.Request) error {
		q := req.URL.Query()
		name, err := r.LookupGroup(q.Get("groupKey.id"), q.Get("groupKey.namespace"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, ci.LookupGroupNameResponse{Name: name})
	})
	rt.Handle("GET /v1/groups:search", func(w http.ResponseWriter, req *http.Request) error {
		full, err := view(req)
		if err != nil {
			return err
		}
		gs, err := r.SearchGroups(req.URL.Query().Get("query"))
		if err != nil {
			return err
		}
		max, def := 1000, 200
		if full {
			max, def = 500, 50
		}
		byKey := map[string]*Group{}
		keys := make([]string, 0, len(gs))
		for _, g := range gs {
			byKey[g.Email] = g
			keys = append(keys, g.Email)
		}
		page, next, err := pageOf(req, "search:"+req.URL.Query().Get("query"), keys, def, max)
		if err != nil {
			return err
		}
		resp := ci.SearchGroupsResponse{NextPageToken: next, Groups: []*ci.Group{}}
		for _, k := range page {
			resp.Groups = append(resp.Groups, groupJSON(byKey[k], full))
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("GET /v1/groups/{group}", func(w http.ResponseWriter, req *http.Request) error {
		g, err := r.GetGroup("groups/" + req.PathValue("group"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, groupJSON(g, true))
	})
	rt.Handle("PATCH /v1/groups/{group}", func(w http.ResponseWriter, req *http.Request) error {
		var g ci.Group
		if err := rest.DecodeJSON(req, &g); err != nil {
			return err
		}
		var mask []string
		for _, f := range strings.Split(req.URL.Query().Get("updateMask"), ",") {
			if f = strings.TrimSpace(f); f != "" {
				mask = append(mask, f)
			}
		}
		out, err := r.UpdateGroup("groups/"+req.PathValue("group"),
			GroupInput{DisplayName: g.DisplayName, Description: g.Description, Labels: g.Labels}, mask)
		if err != nil {
			return err
		}
		return doneOp(w, groupType, groupJSON(out, true))
	})
	rt.Handle("DELETE /v1/groups/{group}", func(w http.ResponseWriter, req *http.Request) error {
		if err := r.DeleteGroup("groups/" + req.PathValue("group")); err != nil {
			return err
		}
		return doneOp(w, "", nil)
	})
	rt.Handle("GET /v1/groups/{group}/securitySettings", func(w http.ResponseWriter, req *http.Request) error {
		return apierror.Unimplemented("Not implemented here: a group's security settings (member restrictions).")
	})
	rt.Handle("PATCH /v1/groups/{group}/securitySettings", func(w http.ResponseWriter, req *http.Request) error {
		return apierror.Unimplemented("Not implemented here: a group's security settings (member restrictions).")
	})
	rt.Handle("POST /v1/groups/{group}/memberships", func(w http.ResponseWriter, req *http.Request) error {
		var m ci.Membership
		if err := rest.DecodeJSON(req, &m); err != nil {
			return err
		}
		var email, ns string
		if m.PreferredMemberKey != nil {
			email, ns = m.PreferredMemberKey.Id, m.PreferredMemberKey.Namespace
		}
		var roles []string
		var expire *time.Time
		for _, x := range m.Roles {
			if x.RestrictionEvaluations != nil {
				return apierror.InvalidArgument("restrictionEvaluations is output only.")
			}
			t, err := roleExpiry(x)
			if err != nil {
				return err
			}
			if t != nil {
				expire = t
			}
			roles = append(roles, x.Name)
		}
		g, out, err := r.CreateMembershipExpiring("groups/"+req.PathValue("group"), email, ns, roles, expire)
		if err != nil {
			return err
		}
		return doneOp(w, membershipType, membershipJSON(g, out, true))
	})
	rt.Handle("GET /v1/groups/{group}/{verb}", func(w http.ResponseWriter, req *http.Request) error {
		parent := "groups/" + req.PathValue("group")
		q := req.URL.Query()
		switch req.PathValue("verb") {
		case "memberships":
			full, err := view(req)
			if err != nil {
				return err
			}
			g, err := r.ListMemberships(parent)
			if err != nil {
				return err
			}
			max, def := 1000, 200
			if full {
				max, def = 500, 50
			}
			byKey := map[string]GroupMember{}
			keys := make([]string, 0, len(g.Members))
			for _, m := range g.Members {
				byKey[m.Email] = m
				keys = append(keys, m.Email)
			}
			page, next, err := pageOf(req, "memberships:"+parent, keys, def, max)
			if err != nil {
				return err
			}
			resp := ci.ListMembershipsResponse{NextPageToken: next, Memberships: []*ci.Membership{}}
			for _, k := range page {
				resp.Memberships = append(resp.Memberships, membershipJSON(g, byKey[k], full))
			}
			return rest.WriteJSON(w, http.StatusOK, resp)
		case "memberships:lookup":
			name, err := r.LookupMembership(parent, q.Get("memberKey.id"), q.Get("memberKey.namespace"))
			if err != nil {
				return err
			}
			return rest.WriteJSON(w, http.StatusOK, ci.LookupMembershipNameResponse{Name: name})
		case "memberships:checkTransitiveMembership":
			iq, err := parseIdentityQuery(q.Get("query"))
			if err != nil {
				return err
			}
			if iq.namespace != "" {
				return rest.WriteJSON(w, http.StatusOK, map[string]bool{"hasMembership": false})
			}
			has, err := r.HasTransitiveMembership(parent, iq.member)
			if err != nil {
				return err
			}
			// hasMembership is written even when false, as Google's JSON
			// carries it.
			return rest.WriteJSON(w, http.StatusOK, map[string]bool{"hasMembership": has})
		case "memberships:searchTransitiveGroups":
			if parent != "groups/-" {
				return apierror.InvalidArgument("Invalid parent %q: searchTransitiveGroups searches every group, groups/-.", parent)
			}
			iq, err := parseIdentityQuery(q.Get("query"))
			if err != nil {
				return err
			}
			if len(iq.labels) == 0 {
				return apierror.InvalidArgument("Invalid query %q: it must include a label ('...' in labels).", q.Get("query"))
			}
			if iq.parent != "" && iq.parent != CustomerName {
				return permissionDenied(iq.parent)
			}
			var rels []GroupRelation
			if iq.namespace == "" {
				if rels, err = r.TransitiveGroups(iq.member); err != nil {
					return err
				}
			}
			byKey := map[string]*ci.GroupRelation{}
			var keys []string
		next:
			for _, x := range rels {
				for _, l := range iq.labels {
					if _, ok := x.Group.Labels[l]; !ok {
						continue next
					}
				}
				rel := "INDIRECT"
				switch {
				case x.Direct && x.Indirect:
					rel = "DIRECT_AND_INDIRECT"
				case x.Direct:
					rel = "DIRECT"
				}
				gr := &ci.GroupRelation{Group: x.Group.Name(), GroupKey: &ci.EntityKey{Id: x.Group.Email},
					DisplayName: x.Group.DisplayName, Labels: x.Group.Labels, RelationType: rel}
				for _, role := range x.Roles {
					gr.Roles = append(gr.Roles, &ci.TransitiveMembershipRole{Role: role})
				}
				byKey[x.Group.Email] = gr
				keys = append(keys, x.Group.Email)
			}
			page, nextTok, err := pageOf(req, "transitive:"+q.Get("query"), keys, 200, 1000)
			if err != nil {
				return err
			}
			resp := ci.SearchTransitiveGroupsResponse{NextPageToken: nextTok, Memberships: []*ci.GroupRelation{}}
			for _, k := range page {
				resp.Memberships = append(resp.Memberships, byKey[k])
			}
			return rest.WriteJSON(w, http.StatusOK, resp)
		case "memberships:searchDirectGroups":
			return r.searchDirectGroups(w, req, parent)
		case "memberships:searchTransitiveMemberships":
			return r.searchTransitiveMemberships(w, req, parent)
		case "memberships:getMembershipGraph":
			return r.getMembershipGraph(w, req, parent)
		}
		return apierror.NotFound("no such method: GET %s", req.URL.Path)
	})
	rt.Handle("GET /v1/groups/{group}/memberships/{membership}", func(w http.ResponseWriter, req *http.Request) error {
		g, m, err := r.GetMembership("groups/" + req.PathValue("group") + "/memberships/" + req.PathValue("membership"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, membershipJSON(g, m, true))
	})
	rt.Handle("DELETE /v1/groups/{group}/memberships/{membership}", func(w http.ResponseWriter, req *http.Request) error {
		if err := r.DeleteMembership("groups/" + req.PathValue("group") + "/memberships/" + req.PathValue("membership")); err != nil {
			return err
		}
		return doneOp(w, "", nil)
	})
	for _, res := range []string{"allowlistedDomains", "customers", "devices", "inboundOidcSsoProfiles", "inboundSamlSsoProfiles",
		"inboundSsoAssignments", "policies"} {
		refuse := func(w http.ResponseWriter, req *http.Request) error {
			return apierror.Unimplemented("Not implemented here: Cloud Identity's %s; its groups and memberships are served.", res)
		}
		for _, m := range []string{"GET", "POST", "PATCH", "DELETE"} {
			rt.Handle(m+" /v1/"+res, refuse)
			rt.Handle(m+" /v1/"+res+"/{rest...}", refuse)
		}
	}
	rt.Handle("POST /v1/groups/{group}/memberships/{membership}", func(w http.ResponseWriter, req *http.Request) error {
		id, ok := strings.CutSuffix(req.PathValue("membership"), ":modifyMembershipRoles")
		if !ok {
			return apierror.NotFound("no such method: POST %s", req.URL.Path)
		}
		return r.modifyMembershipRoles(w, req, "groups/"+req.PathValue("group")+"/memberships/"+id)
	})
}

// IdentityHandler is Cloud Identity's JSON surface over r alone, for a
// test of a client of it.
func (r *Registry) IdentityHandler() http.Handler {
	rt := rest.NewRouter()
	r.identityRoutes(rt)
	return rt
}
