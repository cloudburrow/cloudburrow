package secrets

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
)

// A policy belongs to its secret: deleting the secret deletes it, and a new
// secret with the same ID starts with the empty policy (#365).
func TestIamPolicyGoesWithItsSecret(t *testing.T) {
	t.Parallel()
	g, s := newGRPC(t)
	ctx := context.Background()
	seed(t, s, "demo", "k", "one")
	name := SecretName("demo", "k")
	if _, err := g.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/secretmanager.secretAccessor", Members: []string{"user:a@example.com"}}}}}); err != nil {
		t.Fatal(err)
	}
	got, err := g.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || len(got.GetBindings()) != 1 {
		t.Fatalf("GetIamPolicy = %v, %v", got, err)
	}
	if _, err := g.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	_, err = g.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	wantGRPCCode(t, err, codes.NotFound)
	seed(t, s, "demo", "k", "two")
	if got, err := g.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name}); err != nil || len(got.GetBindings()) != 0 {
		t.Errorf("a new secret with the old ID has policy %v, %v; want the empty one", got, err)
	}
	_, err = g.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: SecretName("demo", "nope"), Permissions: []string{"x"}})
	wantGRPCCode(t, err, codes.NotFound)
}

// The JSON methods Terraform calls: GET :getIamPolicy with the version in
// the query, POST :setIamPolicy and :testIamPermissions. A GET with any other
// verb is UNIMPLEMENTED, never the secret itself.
func TestRESTIamPolicyMethods(t *testing.T) {
	t.Parallel()
	srv, s := newREST(t)
	seed(t, s, "demo", "k", "one")
	base := "/v1/projects/demo/secrets/k"

	code, body := do(t, srv, http.MethodGet, base+":getIamPolicy?options.requestedPolicyVersion=3", "")
	if code != http.StatusOK || !strings.Contains(body, `"etag":"ACAB"`) {
		t.Fatalf("empty policy: %d %s", code, body)
	}
	code, body = do(t, srv, http.MethodPost, base+":setIamPolicy",
		`{"policy":{"version":3,"etag":"ACAB","bindings":[{"role":"roles/secretmanager.secretAccessor","members":["serviceAccount:app@demo.iam.gserviceaccount.com"]}]}}`)
	if code != http.StatusOK || !strings.Contains(body, "serviceAccount:app@demo.iam.gserviceaccount.com") {
		t.Fatalf("setIamPolicy: %d %s", code, body)
	}
	if code, body = do(t, srv, http.MethodPost, base+":setIamPolicy", `{"policy":{"etag":"ACAB"}}`); code != http.StatusConflict {
		t.Errorf("a stale etag: %d %s, want 409 ABORTED", code, body)
	}
	if code, body = do(t, srv, http.MethodPost, base+":getIamPolicy", `{"options":{"requestedPolicyVersion":1}}`); code != http.StatusOK || !strings.Contains(body, "secretAccessor") {
		t.Errorf("POST getIamPolicy: %d %s", code, body)
	}
	if code, body = do(t, srv, http.MethodPost, base+":testIamPermissions", `{"permissions":["secretmanager.versions.access"]}`); code != http.StatusOK || !strings.Contains(body, "secretmanager.versions.access") {
		t.Errorf("testIamPermissions: %d %s", code, body)
	}
	if code, body = do(t, srv, http.MethodPost, base+":setIamPolicy", `{"policy":{"bindings":[{"role":"r","members":["user:a@example.com"],"condition":{"expression":"true"}}]}}`); code != http.StatusNotImplemented || !strings.Contains(body, "condition") {
		t.Errorf("a conditional binding: %d %s, want 501 naming the condition", code, body)
	}
	if code, body = do(t, srv, http.MethodGet, base+":somethingElse", ""); code != http.StatusNotImplemented {
		t.Errorf("GET with an unknown verb: %d %s, want 501", code, body)
	}
}
