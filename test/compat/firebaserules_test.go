//go:build compat

package compat

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	rules "google.golang.org/api/firebaserules/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// EnvFirebaseRules is the Firebase Rules API v1 endpoint (#2048), served
// with Firestore.
const EnvFirebaseRules = "CLOUDBURROW_TEST_FIREBASERULES"

func firebaseRulesClient(t *testing.T, h *Harness) *rules.Service {
	t.Helper()
	hc := &http.Client{}
	t.Cleanup(hc.CloseIdleConnections)
	c, err := rules.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvFirebaseRules)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ownerRules let each signed-in user read and write their own users/{uid}
// document only.
const ownerRules = `rules_version = '2';
service cloud.firestore {
  match /databases/{database}/documents {
    match /users/{uid} {
      allow read, write: if request.auth != null && request.auth.uid == uid;
    }
  }
}
`

// firebaseIDToken is an unsigned Firebase ID token for uid, as the
// Firestore emulator documents accepting (its signature is not checked).
func firebaseIDToken(project, uid string) string {
	now := time.Now().Unix()
	head, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	body, _ := json.Marshal(map[string]any{"iss": "https://securetoken.google.com/" + project, "aud": project,
		"sub": uid, "user_id": uid, "iat": now, "auth_time": now, "exp": now + 3600,
		"firebase": map[string]any{"sign_in_provider": "custom", "identities": map[string]any{}}})
	enc := base64.RawURLEncoding
	return enc.EncodeToString(head) + "." + enc.EncodeToString(body) + "."
}

// clientGet reads a document over Firestore's REST API as a Firebase
// client SDK does: with uid's ID token, or with no credentials for "".
func clientGet(t *testing.T, addr, project, doc, uid string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/projects/"+project+"/databases/(default)/documents/"+doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Goog-Api-Client", "gl-js/ fire/11.0.0")
	if uid != "" {
		req.Header.Set("Authorization", "Bearer "+firebaseIDToken(project, uid))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// covers: firebaserules.projects.rulesets.create, firebaserules.projects.rulesets.get, firebaserules.projects.rulesets.list, firebaserules.projects.rulesets.delete, firebaserules.projects.releases.create, firebaserules.projects.releases.get, firebaserules.projects.releases.list, firebaserules.projects.releases.patch, firebaserules.projects.releases.getExecutable, firebaserules.projects.releases.delete, firebaserules.projects.test
//
// TestFirebaseRulesReleaseEnforcesOnClientCalls (#2048): a ruleset the
// emulator compiles is kept (a bad one is 400 INVALID_ARGUMENT), a
// cloud.firestore release puts it in force, projects.test evaluates ALLOW
// and DENY cases by the emulator, and a client-SDK call (a Firebase ID
// token, or none) is then allowed or denied by the rules, while the server
// client library is not, as on Google. Deleting the release opens the
// project's Firestore again.
func TestFirebaseRulesReleaseEnforcesOnClientCalls(t *testing.T) {
	h := New(t)
	c := firebaseRulesClient(t, h) // skips without CLOUDBURROW_TEST_FIREBASERULES
	fsAddr := h.Endpoint(EnvFirestore)
	ctx := h.Context()
	// A project of its own: the emulator keeps one set of rules per project.
	project := fmt.Sprintf("rules-%d", time.Now().UnixNano()%1e8)
	parent := "projects/" + project

	src := &rules.Source{Files: []*rules.File{{Name: "firestore.rules", Content: ownerRules}}}
	rs, err := c.Projects.Rulesets.Create(parent, &rules.Ruleset{Source: src}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("rulesets.create: %v", err)
	}
	t.Cleanup(func() { _, _ = c.Projects.Rulesets.Delete(rs.Name).Do() })
	if !strings.HasPrefix(rs.Name, parent+"/rulesets/") || rs.CreateTime == "" || rs.Metadata == nil || len(rs.Metadata.Services) != 1 || rs.Metadata.Services[0] != "cloud.firestore" {
		t.Fatalf("the ruleset: %+v", rs)
	}
	_, err = c.Projects.Rulesets.Create(parent, &rules.Ruleset{Source: &rules.Source{Files: []*rules.File{{Name: "firestore.rules", Content: "service cloud.firestore { match x"}}}}).Context(ctx).Do()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusBadRequest {
		t.Fatalf("a ruleset that does not compile: %v; want 400", err)
	}
	got, err := c.Projects.Rulesets.Get(rs.Name).Context(ctx).Do()
	if err != nil || len(got.Source.Files) != 1 || got.Source.Files[0].Content != ownerRules || got.Source.Files[0].Fingerprint != "" {
		t.Fatalf("rulesets.get: %+v %v", got, err)
	}
	list, err := c.Projects.Rulesets.List(parent).Context(ctx).Do()
	if err != nil || len(list.Rulesets) != 1 || list.Rulesets[0].Name != rs.Name {
		t.Fatalf("rulesets.list: %+v %v", list, err)
	}

	// The project's Firestore is open before any release, as it always was.
	admin := func() *firestore.Client {
		t.Setenv("FIRESTORE_EMULATOR_HOST", fsAddr)
		fc, err := firestore.NewClient(ctx, project)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = fc.Close() })
		return fc
	}()
	if _, err := admin.Doc("users/alice").Set(ctx, map[string]any{"name": "Alice"}); err != nil {
		t.Fatalf("the admin write: %v", err)
	}
	if code, body := clientGet(t, fsAddr, project, "users/alice", "bob"); code != http.StatusOK {
		t.Fatalf("before a release, bob's read: %d %s; want 200", code, body)
	}

	relName := parent + "/releases/cloud.firestore"
	rel, err := c.Projects.Releases.Create(parent, &rules.Release{Name: relName, RulesetName: rs.Name}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("releases.create: %v", err)
	}
	t.Cleanup(func() { _, _ = c.Projects.Releases.Delete(relName).Do() })
	if rel.RulesetName != rs.Name || rel.CreateTime == "" || rel.UpdateTime == "" {
		t.Fatalf("the release: %+v", rel)
	}
	if _, err := c.Projects.Releases.Create(parent, &rules.Release{Name: relName, RulesetName: rs.Name}).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusConflict {
		t.Fatalf("a second release of the same name: %v; want 409", err)
	}
	if g, err := c.Projects.Releases.Get(relName).Context(ctx).Do(); err != nil || g.RulesetName != rs.Name {
		t.Fatalf("releases.get: %+v %v", g, err)
	}
	if l, err := c.Projects.Releases.List(parent).Filter("name=" + relName).Context(ctx).Do(); err != nil || len(l.Releases) != 1 {
		t.Fatalf("releases.list: %+v %v", l, err)
	}
	if ex, err := c.Projects.Releases.GetExecutable(relName).Context(ctx).Do(); err != nil || ex.RulesetName != rs.Name || ex.Executable == "" {
		t.Fatalf("releases.getExecutable: %+v %v", ex, err)
	}
	if _, err := c.Projects.Rulesets.Delete(rs.Name).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusBadRequest {
		t.Fatalf("deleting a released ruleset: %v; want 400 FAILED_PRECONDITION", err)
	}

	// Enforced on client calls, not on the server library.
	if code, body := clientGet(t, fsAddr, project, "users/alice", "alice"); code != http.StatusOK {
		t.Errorf("alice's read of her own document: %d %s; want 200", code, body)
	}
	if code, body := clientGet(t, fsAddr, project, "users/alice", "bob"); code != http.StatusForbidden {
		t.Errorf("bob's read of alice's document: %d %s; want 403", code, body)
	}
	if code, body := clientGet(t, fsAddr, project, "users/alice", ""); code != http.StatusForbidden {
		t.Errorf("an unauthenticated client read: %d %s; want 403", code, body)
	}
	if snap, err := admin.Doc("users/alice").Get(ctx); err != nil || snap.Data()["name"] != "Alice" {
		t.Errorf("the server library's read, which rules do not apply to: %v", err)
	}

	// projects.test, evaluated by the emulator.
	path := "/databases/(default)/documents/users/alice"
	res, err := c.Projects.Test(parent, &rules.TestRulesetRequest{Source: src, TestSuite: &rules.TestSuite{TestCases: []*rules.TestCase{
		{Expectation: "ALLOW", Request: map[string]any{"method": "get", "path": path, "auth": map[string]any{"uid": "alice"}}},
		{Expectation: "DENY", Request: map[string]any{"method": "get", "path": path}},
		{Expectation: "ALLOW", Request: map[string]any{"method": "get", "path": path, "auth": map[string]any{"uid": "bob"}}},
		{Expectation: "ALLOW", Request: map[string]any{"method": "create", "path": path, "auth": map[string]any{"uid": "alice"},
			"resource": map[string]any{"data": map[string]any{"name": "Alice"}}}},
		{Expectation: "DENY", Request: map[string]any{"method": "update", "path": path, "auth": map[string]any{"uid": "bob"},
			"resource": map[string]any{"data": map[string]any{"name": "Bob"}}}, Resource: map[string]any{"data": map[string]any{"name": "Alice"}}},
	}}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("projects.test: %v", err)
	}
	var states []string
	for _, r := range res.TestResults {
		states = append(states, r.State)
	}
	if strings.Join(states, ",") != "SUCCESS,SUCCESS,FAILURE,SUCCESS,SUCCESS" {
		t.Errorf("projects.test results: %v (%+v)", states, res.TestResults)
	}
	// A ruleset's own :test, and a source that does not compile, whose
	// issues are answered.
	if r, err := c.Projects.Test(rs.Name, &rules.TestRulesetRequest{TestSuite: &rules.TestSuite{TestCases: []*rules.TestCase{
		{Expectation: "DENY", Request: map[string]any{"method": "get", "path": path}}}}}).Context(ctx).Do(); err != nil || len(r.TestResults) != 1 || r.TestResults[0].State != "SUCCESS" {
		t.Errorf("rulesets/*:test: %+v %v", r, err)
	}
	bad, err := c.Projects.Test(parent, &rules.TestRulesetRequest{Source: &rules.Source{Files: []*rules.File{{Name: "firestore.rules", Content: "service cloud.firestore { match x"}}}}).Context(ctx).Do()
	if err != nil || len(bad.Issues) == 0 || bad.Issues[0].SourcePosition == nil || bad.Issues[0].SourcePosition.Line == 0 {
		t.Errorf("projects.test of a source that does not compile: %+v %v", bad, err)
	}

	// patch to an open ruleset, then delete: open again.
	open, err := c.Projects.Rulesets.Create(parent, &rules.Ruleset{Source: &rules.Source{Files: []*rules.File{{Name: "firestore.rules",
		Content: "rules_version = '2';\nservice cloud.firestore {\n  match /databases/{database}/documents {\n    match /{document=**} {\n      allow read: if true;\n    }\n  }\n}\n"}}}}).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.Projects.Rulesets.Delete(open.Name).Do() })
	if p, err := c.Projects.Releases.Patch(relName, &rules.UpdateReleaseRequest{Release: &rules.Release{Name: relName, RulesetName: open.Name}}).Context(ctx).Do(); err != nil || p.RulesetName != open.Name {
		t.Fatalf("releases.patch: %+v %v", p, err)
	}
	if code, body := clientGet(t, fsAddr, project, "users/alice", "bob"); code != http.StatusOK {
		t.Errorf("after the patch, bob's read: %d %s; want 200", code, body)
	}
	if _, err := c.Projects.Releases.Delete(relName).Context(ctx).Do(); err != nil {
		t.Fatalf("releases.delete: %v", err)
	}
	if _, err := c.Projects.Releases.Get(relName).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("after delete, releases.get: %v; want 404", err)
	}
	if _, err := c.Projects.Rulesets.Delete(rs.Name).Context(ctx).Do(); err != nil {
		t.Errorf("rulesets.delete: %v", err)
	}
	if code, body := clientGet(t, fsAddr, project, "users/alice", ""); code != http.StatusOK {
		t.Errorf("after the release is deleted, an unauthenticated read: %d %s; want 200", code, body)
	}
}

// covers: firebaserules.projects.rulesets.create, firebaserules.projects.rulesets.get, firebaserules.projects.releases.create, firebaserules.projects.releases.get, firebaserules.projects.releases.patch, firebaserules.projects.releases.delete, firebaserules.projects.rulesets.delete
//
// TestTerraformFirebaseRules (#2048): google_firebaserules_ruleset and
// google_firebaserules_release apply through firebaserules_custom_endpoint,
// plan clean, move the release to a new ruleset (the ruleset is replaced)
// and destroy.
func TestTerraformFirebaseRules(t *testing.T) {
	h := New(t)
	c := firebaseRulesClient(t, h) // skips without CLOUDBURROW_TEST_FIREBASERULES
	ctx := h.Context()
	m := newTFModule(t)
	conf := func(allow string) string {
		return fmt.Sprintf(`
resource "google_firebaserules_ruleset" "r" {
  project = %[1]q
  source {
    files {
      name    = "firestore.rules"
      content = "rules_version = '2';\nservice cloud.firestore {\n  match /databases/{database}/documents {\n    match /{document=**} {\n      allow read: if %[2]s;\n    }\n  }\n}\n"
    }
  }
  lifecycle {
    create_before_destroy = true
  }
}

resource "google_firebaserules_release" "rel" {
  project      = %[1]q
  name         = "cloud.firestore"
  ruleset_name = "projects/%[1]s/rulesets/${google_firebaserules_ruleset.r.name}"
}
`, m.project, allow)
	}
	m.write(conf("true"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	relName := "projects/" + m.project + "/releases/cloud.firestore"
	rel, err := c.Projects.Releases.Get(relName).Context(ctx).Do()
	if err != nil {
		t.Fatalf("the release Terraform made: %v", err)
	}
	rs, err := c.Projects.Rulesets.Get(rel.RulesetName).Context(ctx).Do()
	if err != nil || len(rs.Source.Files) != 1 || !strings.Contains(rs.Source.Files[0].Content, "if true") {
		t.Fatalf("its ruleset: %+v %v", rs, err)
	}
	m.planClean()
	// Still open, as the project's Firestore is shared with other tests.
	m.write(conf("request.time != null"))
	m.apply()
	rel2, err := c.Projects.Releases.Get(relName).Context(ctx).Do()
	if err != nil || rel2.RulesetName == rel.RulesetName {
		t.Fatalf("after the change, the release: %+v %v", rel2, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	var ge *googleapi.Error
	if _, err := c.Projects.Releases.Get(relName).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("after destroy, the release: %v; want 404", err)
	}
}
