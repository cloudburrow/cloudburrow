package resourcemanager

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/wif"
)

func wifCall(t *testing.T, r *Registry, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	rec := httptest.NewRecorder()
	r.ServeWorkloadIdentity(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// Pools and providers (#1941): create, get, list, patch, soft delete,
// showDeleted, undelete, ID reuse, validation, and what the STS and IAM
// read of them.
func TestWorkloadIdentityPoolsAndProviders(t *testing.T) {
	r := New(store.NewMemory())
	p, err := r.Create(Project{ProjectID: "wif-proj"})
	if err != nil {
		t.Fatal(err)
	}
	num := strconv.FormatInt(r.projectNumber(p.ProjectID), 10)
	base := "/v1/projects/wif-proj/locations/global/workloadIdentityPools"
	if code, _ := wifCall(t, r, "POST", base+"?workloadIdentityPoolId=gcp-x", map[string]any{}); code != 400 {
		t.Fatalf("gcp- prefix = %d", code)
	}
	if code, _ := wifCall(t, r, "POST", base+"?workloadIdentityPoolId=abc", map[string]any{}); code != 400 {
		t.Fatalf("3-character ID = %d", code)
	}
	code, op := wifCall(t, r, "POST", base+"?workloadIdentityPoolId=ci-pool", map[string]any{"displayName": "CI"})
	if code != 200 || op["done"] != true {
		t.Fatalf("create = %d %v", code, op)
	}
	resp := op["response"].(map[string]any)
	if resp["name"] != "projects/"+num+"/locations/global/workloadIdentityPools/ci-pool" || resp["state"] != "ACTIVE" {
		t.Fatalf("created = %v", resp)
	}
	if code, _ := wifCall(t, r, "POST", base+"?workloadIdentityPoolId=ci-pool", map[string]any{}); code != 409 {
		t.Fatalf("duplicate = %d", code)
	}
	prov := base + "/ci-pool/providers"
	if code, out := wifCall(t, r, "POST", prov+"?workloadIdentityPoolProviderId=gh-oidc", map[string]any{
		"oidc": map[string]any{"issuerUri": "https://idp.example"}}); code != 400 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "google.subject") {
		t.Fatalf("OIDC without a mapping = %d %v", code, out)
	}
	if code, _ := wifCall(t, r, "POST", prov+"?workloadIdentityPoolProviderId=gh-oidc", map[string]any{
		"oidc": map[string]any{"issuerUri": "http://idp.example"}, "attributeMapping": map[string]string{"google.subject": "assertion.sub"}}); code != 400 {
		t.Fatalf("http issuer = %d", code)
	}
	code, _ = wifCall(t, r, "POST", prov+"?workloadIdentityPoolProviderId=gh-oidc", map[string]any{
		"oidc":               map[string]any{"issuerUri": "https://idp.example", "allowedAudiences": []string{"aud-1"}},
		"attributeMapping":   map[string]string{"google.subject": "assertion.sub", "attribute.repo": "assertion.repo"},
		"attributeCondition": "attribute.repo == 'org/app'",
	})
	if code != 200 {
		t.Fatalf("create provider = %d", code)
	}
	code, _ = wifCall(t, r, "POST", prov+"?workloadIdentityPoolProviderId=aws-acct", map[string]any{"aws": map[string]any{"accountId": "123456789012"}})
	if code != 200 {
		t.Fatalf("create AWS provider = %d", code)
	}
	if _, got := wifCall(t, r, "GET", prov+"/aws-acct", nil); got["attributeMapping"].(map[string]any)["google.subject"] != "assertion.arn" {
		t.Fatalf("AWS default mapping = %v", got["attributeMapping"])
	}
	aud := "//iam.googleapis.com/projects/" + num + "/locations/global/workloadIdentityPools/ci-pool/providers/gh-oidc"
	fp, ok := r.FederationProvider(aud)
	if !ok || fp.IssuerURI != "https://idp.example" || fp.Kind != wif.KindOIDC {
		t.Fatalf("FederationProvider = %+v %v", fp, ok)
	}
	principal := wif.Principal(num, "ci-pool", "job")
	set := "principalSet://iam.googleapis.com/projects/" + num + "/locations/global/workloadIdentityPools/ci-pool/attribute.repo/org/app"
	if err := r.RecordFederatedPrincipal(principal, []string{set}); err != nil {
		t.Fatal(err)
	}
	if got := r.Principals(strings.ToLower(principal)); len(got) != 2 || got[1] != set {
		t.Fatalf("Principals = %v", got)
	}

	// Disable the provider: no exchange. Disable the pool: its tokens
	// grant nothing.
	if code, _ := wifCall(t, r, "PATCH", prov+"/gh-oidc?updateMask=disabled", map[string]any{"disabled": true}); code != 200 {
		t.Fatalf("disable provider = %d", code)
	}
	if _, ok := r.FederationProvider(aud); ok {
		t.Fatal("a disabled provider exchanges")
	}
	if code, _ := wifCall(t, r, "PATCH", base+"/ci-pool?updateMask=disabled", map[string]any{"disabled": true}); code != 200 {
		t.Fatalf("disable pool = %d", code)
	}
	if r.FederatedPrincipalActive(principal) || len(r.Principals(principal)) != 0 {
		t.Fatal("a disabled pool's principal is still active")
	}
	if code, _ := wifCall(t, r, "PATCH", base+"/ci-pool?updateMask=mode", map[string]any{}); code != 400 {
		t.Fatalf("immutable mask = %d", code)
	}

	// Soft delete, list, undelete.
	if code, _ := wifCall(t, r, "DELETE", base+"/ci-pool", nil); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	_, list := wifCall(t, r, "GET", base, nil)
	if list["workloadIdentityPools"] != nil {
		t.Fatalf("list after delete = %v", list)
	}
	_, list = wifCall(t, r, "GET", base+"?showDeleted=true", nil)
	pools := list["workloadIdentityPools"].([]any)
	if len(pools) != 1 || pools[0].(map[string]any)["state"] != "DELETED" || pools[0].(map[string]any)["expireTime"] == "" {
		t.Fatalf("showDeleted = %v", list)
	}
	if code, _ := wifCall(t, r, "POST", base+"?workloadIdentityPoolId=ci-pool", map[string]any{}); code != 409 {
		t.Fatalf("reuse of a deleted ID = %d", code)
	}
	if code, _ := wifCall(t, r, "POST", base+"/ci-pool:undelete", map[string]any{}); code != 200 {
		t.Fatalf("undelete = %d", code)
	}
	r.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	_, _ = wifCall(t, r, "DELETE", base+"/ci-pool", nil)
	r.now = func() time.Time { return time.Now().Add(62 * 24 * time.Hour) }
	if code, _ := wifCall(t, r, "GET", base+"/ci-pool", nil); code != 404 {
		t.Fatalf("get after the undelete period = %d", code)
	}
}

// Every unserved workload identity method (managed workload identity's
// namespaces and attestation rules, the pools' IAM policies) answers 501.
func TestWorkloadIdentityUnservedAre501(t *testing.T) {
	r := New(store.NewMemory())
	if _, err := r.Create(Project{ProjectID: "wif-unserved"}); err != nil {
		t.Fatal(err)
	}
	pool := "/v1/projects/wif-unserved/locations/global/workloadIdentityPools/p-one"
	if code, _ := wifCall(t, r, "POST", "/v1/projects/wif-unserved/locations/global/workloadIdentityPools?workloadIdentityPoolId=p-one", map[string]any{}); code != 200 {
		t.Fatal(code)
	}
	n := 0
	for id, served := range WorkloadIdentityMethods {
		if served {
			continue
		}
		n++
		tail := strings.TrimPrefix(id, "iam.projects.locations.workloadIdentityPools.")
		method, path := "POST", pool+":"+tail
		if strings.HasPrefix(tail, "namespaces.") {
			method, path = "GET", pool+"/namespaces/ns-one"
		}
		if code, _ := wifCall(t, r, method, path, map[string]any{}); code != 501 {
			t.Errorf("%s (%s %s) = %d; want 501", id, method, path, code)
		}
	}
	if n == 0 {
		t.Fatal("no unserved methods")
	}
}
