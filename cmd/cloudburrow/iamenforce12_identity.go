package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The twelfth round of IAM enforcement (#1879), on Resource Manager's
// port: Workload Identity Federation's pools, providers and keys, by the
// IAM API's permissions, and Cloud Identity, which keeps its own model.
//
// Cloud Identity's groups are not IAM resources: who may do what follows
// the group's own roles and the customer's admins, as Google documents
// them ("Assign roles to a group's members",
// https://knowledge.workspace.google.com/admin/groups/assign-roles-to-a-groups-members,
// read 2026-11-20): "members with the owner role have all permissions for
// a group"; managers can do everything owners can "except delete the
// group, make another member an owner, or change an owner's role";
// members' basic permissions include "viewing members"; "Only group
// owners and managers can change a group's settings"; and "Administrators
// are automatically assigned the owner role for all groups in an
// organization". The instance's own account is the customer's
// administrator. Any caller may create a group, as the Cloud Identity
// setup requires the domain to let users create groups
// (https://docs.cloud.google.com/identity/docs/how-to/setup); with
// WITH_INITIAL_OWNER the caller owns it. A list or search answers only
// the groups the caller may view, a member's (directly or through a
// nested group) or an admin's. Inbound SSO profiles and assignments need
// the Admin console's Security Settings administrator privilege ("Set up
// SSO via a third party Identity provider",
// https://support.google.com/a/answer/12032922), which only the instance's
// own account holds here. A refusal is Cloud Identity's own, as for a
// group that does not exist (Google does not tell the two apart).
func useIAMEnforcement12Identity(e *iameval.Enforcer, rm *resourcemanager.Server, reg func() *resourcemanager.Registry) {
	if e == nil || rm == nil || e.On == nil || !e.On() {
		return
	}
	rm.WrapJSON(func(next http.Handler) http.Handler {
		return e.HTTPPaths(iameval.WorkloadIdentity, iameval.WorkloadIdentityJSON, next)
	})
	rm.WrapJSON(func(next http.Handler) http.Handler { return identityEnforcer(e, reg, next) })
}

// identityCaller is who a Cloud Identity request comes from: admin for
// the instance's own account (or no token), else its email.
type identityCaller struct {
	email string
	admin bool
}

// groupDenied is Cloud Identity's refusal of resource, the answer for a
// group or membership that does not exist.
func groupDenied(resource string) error {
	return &apierror.Error{Code: codes.PermissionDenied, Reason: "forbidden",
		Message: fmt.Sprintf("Error(2028): Permission denied for resource %s (or it may not exist).", resource)}
}

// ssoDenied is the refusal of an inbound SSO method to a caller who is
// not the customer's admin: Google's PERMISSION_DENIED wording
// (UNVERIFIED against a page, 2026-11-20: the reference gives none).
func ssoDenied() error {
	return &apierror.Error{Code: codes.PermissionDenied, Reason: "forbidden", Message: "The caller does not have permission"}
}

var (
	ciGroupPath      = regexp.MustCompile(`^/v1/(groups/[^/:]+)(?:/securitySettings)?$`)
	ciMembershipsVia = regexp.MustCompile(`^/v1/(groups/[^/:]+)/memberships(?::([A-Za-z]+))?$`)
	ciMembershipPath = regexp.MustCompile(`^/v1/((groups/[^/:]+)/memberships/[^/:]+)(?::([A-Za-z]+))?$`)
	ciMemberKey      = regexp.MustCompile(`member_key_id\s*==\s*'([^']*)'`)
)

// identityEnforcer checks Cloud Identity's JSON requests.
func identityEnforcer(e *iameval.Enforcer, reg func() *resourcemanager.Registry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, "/v1/groups") && !strings.HasPrefix(p, "/v1/inboundS") && !strings.HasPrefix(p, "/v1/inboundOidc") {
			next.ServeHTTP(w, r)
			return
		}
		rg := reg()
		if rg == nil {
			next.ServeHTTP(w, r)
			return
		}
		c := identityCaller{admin: true}
		if b := iameval.Bearer(r.Header.Get("Authorization")); b != "" && e.CallerOf != nil {
			who := e.CallerOf(b)
			if who.Invalid {
				iameval.WriteJSONStatus(w, iameval.Unauthenticated())
				return
			}
			c = identityCaller{email: strings.ToLower(who.Email), admin: who.Owner}
		}
		if c.admin {
			next.ServeHTTP(w, r)
			return
		}
		if err := identityCheck(rg, c, r, w, next); err != nil {
			rest.WriteError(w, err)
		}
	})
}

// identityCheck serves r if c may make it; a list or search it serves
// itself, with only what c may view.
func identityCheck(rg *resourcemanager.Registry, c identityCaller, r *http.Request, w http.ResponseWriter, next http.Handler) error {
	p := r.URL.Path
	if strings.HasPrefix(p, "/v1/inboundS") || strings.HasPrefix(p, "/v1/inboundOidc") {
		return ssoDenied()
	}
	q := r.URL.Query()
	switch {
	case p == "/v1/groups" && r.Method == http.MethodPost:
		// Any caller may create a group.
	case (p == "/v1/groups" || p == "/v1/groups:search") && r.Method == http.MethodGet:
		filterIdentity(w, r, next, "groups", "name", func(name string) bool { return groupRole(rg, c, name) != "" })
		return nil
	case p == "/v1/groups:lookup":
		name, err := rg.LookupGroup(q.Get("groupKey.id"), q.Get("groupKey.namespace"))
		if err == nil && groupRole(rg, c, name) == "" {
			return groupDenied(q.Get("groupKey.id"))
		}
	case strings.HasPrefix(p, "/v1/groups/-/memberships:getMembershipGraph"):
		// Every group's graph of a member: the caller's own.
		if m := ciMemberKey.FindStringSubmatch(q.Get("query")); m == nil || !strings.EqualFold(m[1], c.email) {
			return groupDenied("groups/-")
		}
	case strings.HasPrefix(p, "/v1/groups/-/"):
		// A search of every group (searchTransitiveGroups,
		// searchDirectGroups, getMembershipGraph): the caller's own
		// groups, or those of a member of groups it may view.
		filterIdentity(w, r, next, "memberships", "group", func(name string) bool { return groupRole(rg, c, name) != "" })
		return nil
	case ciGroupPath.MatchString(p):
		g := ciGroupPath.FindStringSubmatch(p)[1]
		role := groupRole(rg, c, g)
		var ok bool
		switch r.Method {
		case http.MethodGet:
			ok = role != ""
		case http.MethodDelete:
			ok = role == "OWNER"
		default:
			ok = role == "OWNER" || role == "MANAGER"
		}
		if !ok {
			return groupDenied(g)
		}
	case ciMembershipsVia.MatchString(p):
		m := ciMembershipsVia.FindStringSubmatch(p)
		role := groupRole(rg, c, m[1])
		if r.Method == http.MethodGet {
			if role == "" {
				return groupDenied(m[1])
			}
			break
		}
		// memberships.create: an owner or manager, and only an owner
		// makes an owner.
		var body struct {
			Roles []struct{ Name string } `json:"roles"`
		}
		_ = peekBody(r, &body)
		makesOwner := slices.ContainsFunc(body.Roles, func(x struct{ Name string }) bool { return strings.EqualFold(x.Name, "OWNER") })
		if role == "" || role == "MEMBER" || (makesOwner && role != "OWNER") {
			return groupDenied(m[1])
		}
	case ciMembershipPath.MatchString(p):
		m := ciMembershipPath.FindStringSubmatch(p)
		name, group := m[1], m[2]
		role := groupRole(rg, c, group)
		if r.Method == http.MethodGet {
			if role == "" {
				return groupDenied(name)
			}
			break
		}
		// A delete or modifyMembershipRoles: an owner or manager; an
		// owner's membership, or making one an owner, an owner's alone.
		if role == "" || role == "MEMBER" {
			return groupDenied(name)
		}
		if role != "OWNER" {
			ownerTouched := false
			if _, mem, err := rg.GetMembership(name); err == nil && slices.Contains(mem.Roles, "OWNER") {
				ownerTouched = true
			}
			var body struct {
				AddRoles []struct{ Name string } `json:"addRoles"`
			}
			if r.Method == http.MethodPost && peekBody(r, &body) {
				for _, x := range body.AddRoles {
					if strings.EqualFold(x.Name, "OWNER") {
						ownerTouched = true
					}
				}
			}
			if ownerTouched {
				return groupDenied(name)
			}
		}
	}
	next.ServeHTTP(w, r)
	return nil
}

// groupRole is c's role in the group name names: OWNER or MANAGER of a
// direct membership, MEMBER of any other (a nested group's member
// included), "" for none or no such group.
func groupRole(rg *resourcemanager.Registry, c identityCaller, name string) string {
	g, err := rg.GetGroup(name)
	if err != nil || c.email == "" {
		return ""
	}
	best := ""
	for _, m := range g.Members {
		if !strings.EqualFold(m.Email, c.email) {
			continue
		}
		switch {
		case slices.Contains(m.Roles, "OWNER"):
			return "OWNER"
		case slices.Contains(m.Roles, "MANAGER"):
			best = "MANAGER"
		case best == "":
			best = "MEMBER"
		}
	}
	if best != "" {
		return best
	}
	if ok, err := rg.HasTransitiveMembership(g.Name(), c.email); err == nil && ok {
		return "MEMBER"
	}
	return ""
}

// filterIdentity serves r and keeps, of its answer's list, the items
// whose key keep accepts.
func filterIdentity(w http.ResponseWriter, r *http.Request, next http.Handler, list, key string, keep func(string) bool) {
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	body := rec.Body.Bytes()
	if rec.Code == http.StatusOK {
		var doc map[string]json.RawMessage
		var items []map[string]any
		if json.Unmarshal(body, &doc) == nil && json.Unmarshal(doc[list], &items) == nil {
			kept := []map[string]any{}
			for _, it := range items {
				if s, _ := it[key].(string); keep(s) {
					kept = append(kept, it)
				}
			}
			if b, err := json.Marshal(kept); err == nil {
				doc[list] = b
				if out, err := json.Marshal(doc); err == nil {
					body = out
				}
			}
		}
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(rec.Code)
	_, _ = w.Write(body)
}
