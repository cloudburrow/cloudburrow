package resourcemanager

import (
	"net/http"
	"sort"
	"strings"
	"time"

	ci "google.golang.org/api/cloudidentity/v1"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The JSON of groups_search.go's searches and role changes.

const graphType = "type.googleapis.com/google.apps.cloudidentity.groups.v1.GetMembershipGraphResponse"

// roleExpiry is a MembershipRole's expiryDetail.expireTime: "only
// supported for MEMBER MembershipRoles".
func roleExpiry(x *ci.MembershipRole) (*time.Time, error) {
	if x == nil || x.ExpiryDetail == nil || x.ExpiryDetail.ExpireTime == "" {
		return nil, nil
	}
	if x.Name != "MEMBER" {
		return nil, apierror.InvalidArgument("Expiry details are only supported for MEMBER MembershipRoles, not %q.", x.Name)
	}
	t, err := time.Parse(time.RFC3339Nano, x.ExpiryDetail.ExpireTime)
	if err != nil {
		return nil, apierror.InvalidArgument("Invalid value at 'expiry_detail.expire_time' (type.googleapis.com/google.protobuf.Timestamp), %q", x.ExpiryDetail.ExpireTime)
	}
	t = t.UTC()
	return &t, nil
}

func relationType(direct, indirect bool) string {
	switch {
	case direct && indirect:
		return "DIRECT_AND_INDIRECT"
	case direct:
		return "DIRECT"
	}
	return "INDIRECT"
}

// memberQuery reads a member search's query, which "MUST include member
// specification AND label(s)".
func memberQuery(q string) (identityQuery, error) {
	iq, err := parseIdentityQuery(q)
	if err != nil {
		return iq, err
	}
	if len(iq.labels) == 0 {
		return iq, apierror.InvalidArgument("Invalid query %q: it must include a label ('...' in labels).", q)
	}
	if iq.parent != "" && iq.parent != CustomerName {
		return iq, permissionDenied(iq.parent)
	}
	return iq, nil
}

func (r *Registry) searchDirectGroups(w http.ResponseWriter, req *http.Request, parent string) error {
	if parent != "groups/-" {
		return apierror.InvalidArgument("Invalid parent %q: searchDirectGroups searches every group, groups/-.", parent)
	}
	q := req.URL.Query()
	iq, err := memberQuery(q.Get("query"))
	if err != nil {
		return err
	}
	var gs []DirectGroup
	if iq.namespace == "" {
		if gs, err = r.DirectGroups(iq.member, iq.labels); err != nil {
			return err
		}
	}
	// orderBy: group_name or group_key, asc (the default) or desc.
	field, dir, _ := strings.Cut(strings.TrimSpace(q.Get("orderBy")), " ")
	dir = strings.TrimSpace(dir)
	switch {
	case field == "" && dir == "":
		field = "group_key"
	case (field == "group_name" || field == "group_key") && (dir == "" || dir == "asc" || dir == "desc"):
	default:
		return apierror.InvalidArgument("Invalid orderBy %q: it must be group_name or group_key, optionally followed by asc or desc.", q.Get("orderBy"))
	}
	sort.SliceStable(gs, func(i, j int) bool {
		a, b := gs[i].Group.Email, gs[j].Group.Email
		if field == "group_name" {
			a, b = gs[i].Group.DisplayName, gs[j].Group.DisplayName
		}
		if dir == "desc" {
			return a > b
		}
		return a < b
	})
	byKey := map[string]DirectGroup{}
	keys := make([]string, 0, len(gs))
	for _, x := range gs {
		byKey[x.Group.Email] = x
		keys = append(keys, x.Group.Email)
	}
	page, next, err := pageOf(req, "direct:"+q.Get("query")+":"+q.Get("orderBy"), keys, 200, 1000)
	if err != nil {
		return err
	}
	resp := ci.SearchDirectGroupsResponse{NextPageToken: next, Memberships: []*ci.MembershipRelation{}}
	for _, k := range page {
		x := byKey[k]
		mj := membershipJSON(x.Group, x.Member, false)
		resp.Memberships = append(resp.Memberships, &ci.MembershipRelation{Group: x.Group.Name(),
			GroupKey: &ci.EntityKey{Id: x.Group.Email}, DisplayName: x.Group.DisplayName, Description: x.Group.Description,
			Labels: x.Group.Labels, Membership: mj.Name, Roles: mj.Roles})
	}
	return rest.WriteJSON(w, http.StatusOK, resp)
}

func (r *Registry) searchTransitiveMemberships(w http.ResponseWriter, req *http.Request, parent string) error {
	rels, err := r.TransitiveMembers(parent)
	if err != nil {
		return err
	}
	byKey := map[string]MemberRelation{}
	keys := make([]string, 0, len(rels))
	for _, x := range rels {
		byKey[x.Email] = x
		keys = append(keys, x.Email)
	}
	page, next, err := pageOf(req, "members:"+parent, keys, 200, 1000)
	if err != nil {
		return err
	}
	resp := ci.SearchTransitiveMembershipsResponse{NextPageToken: next, Memberships: []*ci.MemberRelation{}}
	for _, k := range page {
		x := byKey[k]
		mr := &ci.MemberRelation{Member: MemberResource(x), PreferredMemberKey: []*ci.EntityKey{{Id: x.Email}},
			RelationType: relationType(x.Direct, x.Indirect)}
		roles := x.Roles
		if !x.Direct {
			// An indirect member is a member of the group.
			roles = []string{"MEMBER"}
		}
		for _, role := range roles {
			mr.Roles = append(mr.Roles, &ci.TransitiveMembershipRole{Role: role})
		}
		resp.Memberships = append(resp.Memberships, mr)
	}
	return rest.WriteJSON(w, http.StatusOK, resp)
}

func (r *Registry) getMembershipGraph(w http.ResponseWriter, req *http.Request, parent string) error {
	iq, err := memberQuery(req.URL.Query().Get("query"))
	if err != nil {
		return err
	}
	var edges []GraphEdge
	if iq.namespace == "" {
		if edges, err = r.MembershipGraph(parent, iq.member, iq.labels); err != nil {
			return err
		}
	} else if parent != "groups/-" {
		if _, err := r.GetGroup(parent); err != nil {
			return err
		}
	}
	resp := ci.GetMembershipGraphResponse{AdjacencyList: []*ci.MembershipAdjacencyList{}, Groups: []*ci.Group{}}
	for _, e := range edges {
		al := &ci.MembershipAdjacencyList{Group: e.Group.Name(), Edges: []*ci.Membership{}}
		for _, m := range e.Members {
			al.Edges = append(al.Edges, membershipJSON(e.Group, m, false))
		}
		resp.AdjacencyList = append(resp.AdjacencyList, al)
		resp.Groups = append(resp.Groups, groupJSON(e.Group, true))
	}
	return doneOp(w, graphType, resp)
}

func (r *Registry) modifyMembershipRoles(w http.ResponseWriter, req *http.Request, name string) error {
	var body ci.ModifyMembershipRolesRequest
	if err := rest.DecodeJSON(req, &body); err != nil {
		return err
	}
	var add, update []RoleChange
	for _, x := range body.AddRoles {
		if x == nil {
			continue
		}
		t, err := roleExpiry(x)
		if err != nil {
			return err
		}
		add = append(add, RoleChange{Name: x.Name, ExpireTime: t})
	}
	for _, p := range body.UpdateRolesParams {
		if p == nil || p.MembershipRole == nil {
			return apierror.InvalidArgument("Request contains an invalid argument: updateRolesParams.membershipRole is required.")
		}
		if p.FieldMask != "expiry_detail.expire_time" && p.FieldMask != "expiryDetail.expireTime" {
			return apierror.InvalidArgument("Invalid updateRolesParams.fieldMask %q: it may only contain expiry_detail.expire_time.", p.FieldMask)
		}
		t, err := roleExpiry(p.MembershipRole)
		if err != nil {
			return err
		}
		update = append(update, RoleChange{Name: p.MembershipRole.Name, ExpireTime: t})
	}
	g, m, err := r.ModifyMembershipRoles(name, add, body.RemoveRoles, update, len(body.UpdateRolesParams) > 0)
	if err != nil {
		return err
	}
	return rest.WriteJSON(w, http.StatusOK, ci.ModifyMembershipRolesResponse{Membership: r.evaluated(g, m, membershipJSON(g, m, true))})
}
