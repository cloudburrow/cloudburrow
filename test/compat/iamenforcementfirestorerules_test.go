//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/firestore"
	rules "google.golang.org/api/firebaserules/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.firestore.v1.Firestore/GetDocument, google.firestore.v1.Firestore/Commit, firebaserules.projects.rulesets.create, firebaserules.projects.releases.create
//
// TestIAMEnforcementFirestoreClientSDKRules (#1879, #2048, #2050): under
// IAM enforcement, as on Google, Security Rules alone govern a Firebase
// client SDK's calls (a Firebase ID token, or none) and IAM alone a server
// client library's (an OAuth access token for an IAM principal), which
// Rules never bind. Before any release a client is denied (deny-all); with
// a restrictive release, a client read the rules refuse is denied over
// REST and gRPC although the project grants roles/owner, and the one they
// allow is answered although the Firebase user holds no IAM role. The
// official Go server client, as a principal granted roles/owner, reads and
// writes what the rules refuse every client; as one with no grant it is
// refused by IAM, "Missing or insufficient permissions.".
func TestIAMEnforcementFirestoreClientSDKRules(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvFirestore)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	rc := firebaseRulesClient(t, h) // skips without CLOUDBURROW_TEST_FIREBASERULES
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iefsr-")
	granted := newIAMPrincipal(t, h, project, "iefsr-owner", "roles/owner")
	nobody := newIAMPrincipal(t, h, project, "iefsr-none")

	owner, err := firestore.NewClient(ctx, project, grpcAs(addr, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	for doc, data := range map[string]map[string]any{
		"users/alice":  {"name": "Alice"},
		"secrets/plan": {"code": 42},
	} {
		if _, err := owner.Doc(doc).Set(ctx, data); err != nil {
			t.Fatalf("the instance's own account: Set %s = %v", doc, err)
		}
	}

	// No release yet: every client call is denied.
	if code, body := clientGet(t, addr, project, "users/alice", "alice"); code != http.StatusForbidden {
		t.Errorf("before any release, alice's own document: %d %s; want 403", code, body)
	}

	parent := "projects/" + project
	rs, err := rc.Projects.Rulesets.Create(parent, &rules.Ruleset{Source: &rules.Source{
		Files: []*rules.File{{Name: "firestore.rules", Content: ownerRules}}}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("rulesets.create: %v", err)
	}
	t.Cleanup(func() { _, _ = rc.Projects.Rulesets.Delete(rs.Name).Do() })
	rel := parent + "/releases/cloud.firestore"
	if _, err := rc.Projects.Releases.Create(parent, &rules.Release{Name: rel, RulesetName: rs.Name}).Context(ctx).Do(); err != nil {
		t.Fatalf("releases.create: %v", err)
	}
	t.Cleanup(func() { _, _ = rc.Projects.Releases.Delete(rel).Do() })

	// Client SDK calls: the rules decide, whatever IAM grants.
	for _, c := range []struct{ doc, uid string }{
		{"users/alice", "bob"},
		{"users/alice", ""},
		{"secrets/plan", "alice"},
	} {
		if code, body := clientGet(t, addr, project, c.doc, c.uid); code != http.StatusForbidden || !strings.Contains(body, "PERMISSION_DENIED") {
			t.Errorf("client %q reads %s with roles/owner granted on the project: %d %s; want 403 PERMISSION_DENIED by the rules", c.uid, c.doc, code, body)
		}
	}
	if code, body := clientGet(t, addr, project, "users/alice", "alice"); code != http.StatusOK || !strings.Contains(body, "Alice") {
		t.Errorf("alice reads her own document, which the rules allow (she holds no IAM role): %d %s; want 200", code, body)
	}
	// Over gRPC, as the mobile SDKs call, with bob's Firebase ID token.
	bob, err := firestore.NewClient(ctx, project, grpcAs(addr, firebaseIDToken(project, "bob"))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bob.Close() })
	if _, err := bob.Doc("users/alice").Get(ctx); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob's gRPC read of users/alice: %v; want PERMISSION_DENIED by the rules", err)
	}
	if _, err := bob.Doc("secrets/plan").Set(ctx, map[string]any{"code": 0}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob's gRPC write of secrets/plan: %v; want PERMISSION_DENIED by the rules", err)
	}

	// Server client library calls: IAM decides, and the rules never bind.
	as, err := firestore.NewClient(ctx, project, grpcAs(addr, granted.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	waitGranted(t, "the roles/owner principal's read of secrets/plan", func() error {
		snap, err := as.Doc("secrets/plan").Get(ctx)
		if err == nil && snap.Data()["code"] != int64(42) {
			return fmt.Errorf("data %v", snap.Data())
		}
		return err
	})
	if _, err := as.Doc("secrets/plan").Set(ctx, map[string]any{"code": 43}); err != nil {
		t.Errorf("the roles/owner principal's write of secrets/plan: %v", err)
	}
	none, err := firestore.NewClient(ctx, project, grpcAs(addr, nobody.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = none.Close() })
	const msg = "Missing or insufficient permissions."
	_, err = none.Doc("users/alice").Get(ctx)
	wantDenied(t, "the ungranted principal's GetDocument", err, msg, "", false)
	_, err = none.Doc("users/alice").Set(ctx, map[string]any{"name": "Mallory"})
	wantDenied(t, "the ungranted principal's Commit", err, msg, "", false)
	if snap, err := owner.Doc("users/alice").Get(ctx); err != nil || snap.Data()["name"] != "Alice" {
		t.Errorf("users/alice after the refused write: %v %v", snap.Data(), err)
	}
}
