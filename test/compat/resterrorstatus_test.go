//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
)

// The REST error bodies of the services that answer through the shared JSON
// error writer (apierror.WriteJSON, rest.Router) carry Google's status, the
// canonical code's name, beside the HTTP code (#1470). Without it a REST
// client can tell FAILED_PRECONDITION from INVALID_ARGUMENT (both 400) or
// ABORTED from ALREADY_EXISTS (both 409) by nothing. gcloud is the official
// REST client these tests read it through: it prints status before the
// message, "ERROR: (gcloud.…) ABORTED: …", and prints "HTTPError 409" when
// the body has none. On a 409 for a path of two or three segments under
// projects/ (CreateSecret, a project's setIamPolicy) gcloud writes its own
// "is the subject of a conflict" instead, for Google's answers too; there the
// tests read the status from the body the official Go REST client hands back
// in its *googleapi.Error, since gax maps 409 to ABORTED by the HTTP code
// alone.

// wantGcloudStatus runs gcloud, which must fail, and checks it printed the
// canonical code as status names it, followed by the message.
func wantGcloudStatus(t *testing.T, g *gcloudSession, status, names string, args ...string) {
	t.Helper()
	out, err := g.run(nil, args...)
	if err == nil {
		t.Errorf("gcloud %s succeeded; want %s:\n%s", strings.Join(args, " "), status, out)
		return
	}
	if !strings.Contains(out, ") "+status+": ") || !strings.Contains(out, names) {
		t.Errorf("gcloud %s printed:\n%s\nwant %s: and a message naming %q", strings.Join(args, " "), out, status, names)
	}
}

// wantGcloudConflict runs gcloud, which must fail with its own wording for
// a 409 on a short projects/ path, followed by the message.
func wantGcloudConflict(t *testing.T, g *gcloudSession, names string, args ...string) {
	t.Helper()
	out, err := g.run(nil, args...)
	if err == nil || !strings.Contains(out, "is the subject of a conflict") || !strings.Contains(out, names) {
		t.Errorf("gcloud %s = %v:\n%s\nwant gcloud's conflict wording and a message naming %q", strings.Join(args, " "), err, out, names)
	}
}

// wantBodyStatus checks the error an official REST client returned is a
// *googleapi.Error of the HTTP code whose body names status.
func wantBodyStatus(t *testing.T, what string, err error, code int, status string) {
	t.Helper()
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		t.Errorf("%s = %v (%T); want a *googleapi.Error", what, err, err)
		return
	}
	var body struct {
		Error struct {
			Code   int    `json:"code"`
			Status string `json:"status"`
		} `json:"error"`
	}
	if jerr := json.Unmarshal([]byte(ge.Body), &body); jerr != nil || ge.Code != code || body.Error.Code != code || body.Error.Status != status {
		t.Errorf("%s = HTTP %d, body %s; want %d with status %s", what, ge.Code, ge.Body, code, status)
	}
}

// stalePolicy writes an IAM policy file whose etag is not the resource's.
func stalePolicy(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(f, []byte(`{"etag":"c3RhbGUtZXRhZw==","bindings":[{"role":"roles/viewer","members":["user:a@example.com"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/SetIamPolicy
//
// TestGcloudSecretManagerErrorStatus (#1470): through gcloud-setup's
// configuration, gcloud reads FAILED_PRECONDITION from an access of a
// disabled version and ABORTED from a set-iam-policy with a stale etag, each
// from the body's status; a second create of a secret is gcloud's conflict
// wording, and the official REST client's error body says ALREADY_EXISTS.
func TestGcloudSecretManagerErrorStatus(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvSecrets)
	g := newGcloudSession(t, h)
	id := "gcloud-error-status"
	t.Cleanup(func() { _, _ = g.run(nil, "secrets", "delete", id, "--quiet") })
	g.must("secrets", "create", id, "--replication-policy=automatic")
	wantGcloudConflict(t, g, id, "secrets", "create", id, "--replication-policy=automatic")
	c := secretsRESTClient(t, h)
	_, err := c.CreateSecret(h.Context(), &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + g.project, SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}})
	wantBodyStatus(t, "a second CreateSecret over REST", err, http.StatusConflict, "ALREADY_EXISTS")

	payload := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(payload, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.must("secrets", "versions", "add", id, "--data-file="+payload)
	g.must("secrets", "versions", "disable", "1", "--secret="+id)
	wantGcloudStatus(t, g, "FAILED_PRECONDITION", id, "secrets", "versions", "access", "1", "--secret="+id)

	wantGcloudStatus(t, g, "ABORTED", "etag", "secrets", "set-iam-policy", id, stalePolicy(t))
}

// covers: google.cloud.resourcemanager.v3.TagKeys/CreateTagKey, google.cloud.resourcemanager.v3.TagKeys/DeleteTagKey, cloudresourcemanager.projects.setIamPolicy
//
// TestGcloudResourceManagerErrorStatus (#1470): through gcloud-setup's
// configuration, gcloud reads ALREADY_EXISTS from a second tag key of one
// short name and FAILED_PRECONDITION from deleting a tag key that still has
// a value, each from the body's status; a project set-iam-policy with a
// stale etag is gcloud's conflict wording, and the official v1 client's
// error body says ABORTED.
func TestGcloudResourceManagerErrorStatus(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	g := newGcloudSession(t, h)
	parent := "projects/" + g.project
	short := "errstatus"
	key := strings.TrimSpace(g.must("resource-manager", "tags", "keys", "create", short, "--parent="+parent, "--format=value(name)"))
	if !strings.HasPrefix(key, "tagKeys/") {
		t.Fatalf("tags keys create printed %q; want the key's name", key)
	}
	t.Cleanup(func() { _, _ = g.run(nil, "resource-manager", "tags", "keys", "delete", key, "--quiet") })
	wantGcloudStatus(t, g, "ALREADY_EXISTS", short, "resource-manager", "tags", "keys", "create", short, "--parent="+parent)

	value := strings.TrimSpace(g.must("resource-manager", "tags", "values", "create", "v", "--parent="+key, "--format=value(name)"))
	if !strings.HasPrefix(value, "tagValues/") {
		t.Fatalf("tags values create printed %q; want the value's name", value)
	}
	t.Cleanup(func() { _, _ = g.run(nil, "resource-manager", "tags", "values", "delete", value, "--quiet") })
	wantGcloudStatus(t, g, "FAILED_PRECONDITION", value, "resource-manager", "tags", "keys", "delete", key, "--quiet")

	id := v1ID(h, "es-")
	t.Cleanup(func() { _, _ = g.run(nil, "projects", "delete", id, "--quiet") })
	g.must("projects", "create", id, "--no-enable-cloud-apis", "--quiet")
	wantGcloudConflict(t, g, "etag", "projects", "set-iam-policy", id, stalePolicy(t))
	_, err := rmV1(t, h).Projects.SetIamPolicy(id, &crmv1.SetIamPolicyRequest{Policy: &crmv1.Policy{Etag: "c3RhbGUtZXRhZw==",
		Bindings: []*crmv1.Binding{{Role: "roles/viewer", Members: []string{"user:a@example.com"}}}}}).Context(h.Context()).Do()
	wantBodyStatus(t, "a v1 setIamPolicy with a stale etag", err, http.StatusConflict, "ABORTED")
}
