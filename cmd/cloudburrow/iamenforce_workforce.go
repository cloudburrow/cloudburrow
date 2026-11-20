package main

import (
	"net/http"
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
	items := e.HTTPPaths(iameval.WorkforceIdentity, iameval.WorkforceIdentityJSON, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// workforceScope is a workforce pool resource's scope: its own name, and
// its organization's ancestry above it.
func workforceScope(reg *resourcemanager.Registry, resource string) (iameval.Scope, bool) {
	rest, ok := strings.CutPrefix(resource, "locations/global/workforcePools/")
	if !ok {
		return iameval.Scope{}, false
	}
	pool, _, _ := strings.Cut(rest, "/")
	parent := ""
	if reg != nil {
		parent = reg.WorkforcePoolParent(pool)
	}
	if parent == "" {
		// No such pool: the instance's organization, so a caller who
		// holds the permission there learns it does not exist.
		parent = resourcemanager.OrganizationName
	}
	return iameval.Scope{Own: []string{}, Parent: parent}, true
}
