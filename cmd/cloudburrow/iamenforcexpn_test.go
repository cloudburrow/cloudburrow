package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// xpnSource is an evaluator Source of project policies only.
type xpnSource map[string]*iampolicy.Stored

func (s xpnSource) Ancestry(p string) []string                    { return []string{"projects/" + p} }
func (s xpnSource) Policy(r string) *iampolicy.Stored             { return s[r] }
func (s xpnSource) CustomRolePermissions(string) ([]string, bool) { return nil, false }
func (s xpnSource) Principals(e string) []string                  { return []string{iameval.Member(e)} }

// TestSharedVPCIndividualSubnetsEnforced (#2140): a service project's
// developer, an editor of the service project only, may not put an
// internal address on the host's subnet until roles/compute.networkUser is
// granted on that subnet; then that subnet (and no other host subnet) is
// usable, testIamPermissions on it answers compute.subnetworks.use, and
// listUsable lists it beside the service project's own subnets. With
// enforcement off every call is served.
func TestSharedVPCIndividualSubnetsEnforced(t *testing.T) {
	api, err := compute.New(compute.Options{})
	if err != nil {
		t.Fatal(err)
	}
	src := xpnSource{"projects/svc": {Bindings: []iampolicy.Binding{{Role: "roles/editor", Members: []string{"user:dev@example.com"}}}}}
	on := true
	e := &iameval.Enforcer{Eval: &iameval.Evaluator{Source: src}, On: func() bool { return on }, Policies: map[string]iameval.PolicyGetter{}, Resources: map[string]iameval.ResourceLister{},
		CallerOf: func(b string) iameval.Caller { return iameval.Caller{Email: b} }, OwnerEmail: "owner@example.com"}
	svc := &computeService{api: api}
	iamEnforced7{compute: svc}.use(e)
	iamEnforced8{compute: svc}.use(e)
	h := svc.wrapJSON(api)
	do := func(as, method, path string, body any) (int, map[string]any) {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		r := httptest.NewRequest(method, "/compute/v1/projects/"+path, bytes.NewReader(b))
		if as != "" {
			r.Header.Set("Authorization", "Bearer "+as)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	mustOK := func(code int, out map[string]any) map[string]any {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("%d %v", code, out)
		}
		return out
	}
	mustOK(do("", "POST", "host/global/networks", map[string]any{"name": "shared", "autoCreateSubnetworks": false}))
	for _, s := range []string{"a", "b"} {
		cidr := map[string]string{"a": "10.10.0.0/24", "b": "10.20.0.0/24"}[s]
		mustOK(do("", "POST", "host/regions/us-central1/subnetworks", map[string]any{"name": s, "network": "global/networks/shared", "ipCidrRange": cidr}))
	}
	mustOK(do("", "POST", "host/enableXpnHost", nil))
	mustOK(do("", "POST", "host/enableXpnResource", map[string]any{"xpnResource": map[string]any{"id": "svc", "type": "PROJECT"}}))
	addr := func(name, subnet string) (int, map[string]any) {
		return do("dev@example.com", "POST", "svc/regions/us-central1/addresses", map[string]any{"name": name, "addressType": "INTERNAL",
			"subnetwork": "projects/host/regions/us-central1/subnetworks/" + subnet})
	}
	if code, out := addr("x", "a"); code != http.StatusForbidden || !strings.Contains(asString(out), "compute.subnetworks.use") {
		t.Fatalf("before the grant: %d %v", code, out)
	}
	pol := mustOK(do("", "GET", "host/regions/us-central1/subnetworks/a/getIamPolicy", nil))
	mustOK(do("", "POST", "host/regions/us-central1/subnetworks/a/setIamPolicy", map[string]any{"policy": map[string]any{"etag": pol["etag"],
		"bindings": []any{map[string]any{"role": "roles/compute.networkUser", "members": []any{"user:dev@example.com"}}}}}))
	mustOK(addr("x", "a"))
	if code, _ := addr("y", "b"); code != http.StatusForbidden {
		t.Fatalf("subnet b is not shared with dev: %d", code)
	}
	tp := mustOK(do("dev@example.com", "POST", "host/regions/us-central1/subnetworks/a/testIamPermissions",
		map[string]any{"permissions": []any{"compute.subnetworks.use", "compute.subnetworks.setIamPolicy"}}))
	if got := asString(tp["permissions"]); got != `["compute.subnetworks.use"]` {
		t.Fatalf("testIamPermissions on a: %v", tp)
	}
	// On b, which is not shared with dev, it holds none (#2112's answer:
	// what the caller holds, with no permission of its own needed).
	if tb := mustOK(do("dev@example.com", "POST", "host/regions/us-central1/subnetworks/b/testIamPermissions",
		map[string]any{"permissions": []any{"compute.subnetworks.use"}})); tb["permissions"] != nil {
		t.Fatalf("testIamPermissions on b: %v", tb)
	}
	if code, _ := do("dev@example.com", "POST", "host/regions/us-central1/subnetworks/a/setIamPolicy", map[string]any{"policy": map[string]any{}}); code != http.StatusForbidden {
		t.Fatalf("dev setIamPolicy: %d", code)
	}
	usable := mustOK(do("dev@example.com", "GET", "svc/aggregated/subnetworks/listUsable", nil))
	var hostSubs []string
	own := 0
	for _, it := range usable["items"].([]any) {
		link := it.(map[string]any)["subnetwork"].(string)
		switch {
		case strings.Contains(link, "projects/host/"):
			hostSubs = append(hostSubs, link[strings.LastIndex(link, "/")+1:])
		case strings.Contains(link, "projects/svc/"):
			own++
		}
	}
	if len(hostSubs) != 1 || hostSubs[0] != "a" || own == 0 {
		t.Fatalf("listUsable as dev: host %v, own %d", hostSubs, own)
	}
	on = false
	mustOK(addr("y", "b"))
}

func asString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
