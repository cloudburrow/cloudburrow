package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
)

// Shared VPC's Individual subnets (#2140): a subnet's own policy
// (subnetworks.setIamPolicy) is read by enforcement, so
// roles/compute.networkUser granted on one host subnet lets a service
// project's principals use that subnet, as compute.subnetworks.use is
// checked on it (iameval.ComputeFieldChecks) against the subnet's policy
// and the host project's. A subnet's testIamPermissions answers what the
// caller holds there, and listUsable lists the subnets on which the
// caller holds compute.subnetworks.use (vpc/docs/shared-vpc, "Individual
// subnets"). With enforcement off every call is served as it is.

var (
	computeSubnetTestIAMPath = regexp.MustCompile(`^/compute/v1/(projects/[^/]+/regions/[^/]+/subnetworks/[^/]+)/testIamPermissions$`)
	computeListUsablePath    = regexp.MustCompile(`^/compute/v1/projects/[^/]+/aggregated/subnetworks/listUsable$`)
)

// useComputeSubnetIAM wraps the API's testIamPermissions and listUsable
// answers. The subnet policies' reader is the one every Compute resource
// with a policy shares (#2112, iamenforce7.go).
func useComputeSubnetIAM(e *iameval.Enforcer, svc *computeService) {
	prev := svc.wrapJSON
	svc.wrapJSON = func(next http.Handler) http.Handler {
		h := computeSubnetAnswers(e, next)
		if prev != nil {
			h = prev(h)
		}
		return h
	}
}

// computeSubnetAnswers narrows a subnet's testIamPermissions to what the
// caller holds there, and listUsable to the subnets it may use.
func computeSubnetAnswers(e *iameval.Enforcer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() {
			next.ServeHTTP(w, r)
			return
		}
		var resource string
		testIAM := r.Method == http.MethodPost && computeSubnetTestIAMPath.MatchString(r.URL.Path)
		if testIAM {
			resource = computeSubnetTestIAMPath.FindStringSubmatch(r.URL.Path)[1]
		}
		usable := r.Method == http.MethodGet && computeListUsablePath.MatchString(r.URL.Path)
		if !testIAM && !usable {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		var body map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			copyRecorded(w, rec, rec.Body.Bytes())
			return
		}
		bearer := iameval.Bearer(r.Header.Get("Authorization"))
		holds := func(permission, resource string) bool {
			return e.Authorize(r.Context(), iameval.Compute, bearer, permission, resource) == nil
		}
		if testIAM {
			var held []any
			for _, p := range asList(body["permissions"]) {
				if s, _ := p.(string); s != "" && holds(s, resource) {
					held = append(held, s)
				}
			}
			delete(body, "permissions")
			if len(held) > 0 {
				body["permissions"] = held
			}
		} else {
			kept := []any{}
			for _, it := range asList(body["items"]) {
				m, _ := it.(map[string]any)
				link, _ := m["subnetwork"].(string)
				if i := strings.Index(link, "projects/"); i >= 0 && holds("compute.subnetworks.use", link[i:]) {
					kept = append(kept, it)
				}
			}
			body["items"] = kept
		}
		out, err := json.Marshal(body)
		if err != nil {
			copyRecorded(w, rec, rec.Body.Bytes())
			return
		}
		copyRecorded(w, rec, out)
	})
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// copyRecorded writes rec's status and headers with body.
func copyRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder, body []byte) {
	for k, vs := range rec.Header() {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(body)
}
