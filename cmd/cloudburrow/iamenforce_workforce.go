package main

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// IAM enforcement of Workforce Identity Federation (#2056), on Resource
// Manager's port: the IAM API's workforce pools, providers, keys and
// subjects, by the IAM API's permissions (internal/iameval's
// workforce.go). A pool's methods are checked on the pool, under its
// organization; list and create on the organization the request names.
func useIAMEnforcementWorkforce(e *iameval.Enforcer, rm *resourcemanager.Server) {
	if e == nil || rm == nil || e.On == nil || !e.On() {
		return
	}
	rm.WrapJSON(func(next http.Handler) http.Handler { return workforceEnforcer(e, next) })
}

func workforceEnforcer(e *iameval.Enforcer, next http.Handler) http.Handler {
	methods := append(append([]iameval.JSONMethod{}, iameval.WorkforceIdentityJSON...), iameval.WorkforceScimJSON...)
	items := e.HTTPPaths(iameval.WorkforceIdentity, methods, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// testIamPermissions on a pool or a SCIM tenant (#2065): the
			// permissions asked that the caller holds there.
			for _, rx := range []*regexp.Regexp{iameval.WorkforcePoolTestIAM, iameval.WorkforceScimTestIAM} {
				if m := rx.FindStringSubmatch(r.URL.Path); m != nil {
					e.TestPermissionsJSON(w, r, iameval.WorkforceIdentity, m[1], next)
					return
				}
			}
		}
		if !iameval.WorkforcePoolsCollection.MatchString(r.URL.Path) {
			items.ServeHTTP(w, r)
			return
		}
		permission, parent := "", ""
		switch r.Method {
		case http.MethodGet:
			permission, parent = "iam.workforcePools.list", r.URL.Query().Get("parent")
		case http.MethodPost:
			var body struct {
				Parent string `json:"parent"`
			}
			_ = peekBody(r, &body)
			permission, parent = "iam.workforcePools.create", body.Parent
		}
		if permission == "" || !strings.HasPrefix(parent, "organizations/") {
			// No parent: the API's own INVALID_ARGUMENT.
			next.ServeHTTP(w, r)
			return
		}
		if err := e.Authorize(r.Context(), iameval.WorkforceIdentity, iameval.Bearer(r.Header.Get("Authorization")), permission, parent); err != nil {
			iameval.WriteJSONStatus(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// workforceScope is a workforce pool resource's scope: a SCIM tenant's own
// policy and its pool's (#2065), deepest first, and the organization's
// ancestry above them.
func workforceScope(reg *resourcemanager.Registry, resource string) (iameval.Scope, bool) {
	rest, ok := strings.CutPrefix(resource, "locations/global/workforcePools/")
	if !ok {
		return iameval.Scope{}, false
	}
	pool, below, _ := strings.Cut(rest, "/")
	own := []string{}
	if parts := strings.Split(below, "/"); len(parts) >= 4 && parts[0] == "providers" && parts[2] == "scimTenants" {
		own = append(own, "locations/global/workforcePools/"+pool+"/providers/"+parts[1]+"/scimTenants/"+parts[3])
	}
	own = append(own, "locations/global/workforcePools/"+pool)
	parent := ""
	if reg != nil {
		parent = reg.WorkforcePoolParent(pool)
	}
	if parent == "" {
		// No such pool: the instance's organization, so a caller who
		// holds the permission there learns it does not exist.
		parent = resourcemanager.OrganizationName
	}
	return iameval.Scope{Own: own, Parent: parent}, true
}
