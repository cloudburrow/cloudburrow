package iameval

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// TestAuthorizeAnonymous: a request with no credentials is allUsers alone
// (ADR-0006), refused a pull of a repository until its own policy grants
// allUsers Artifact Registry Reader; the project's owner binding is not
// its; with enforcement off nothing is refused.
func TestAuthorizeAnonymous(t *testing.T) {
	const repo = "projects/p/locations/us-central1/repositories/r"
	const pull = "artifactregistry.repositories.downloadArtifacts"
	own := map[string]*iampolicy.Stored{}
	src := fakeSource{policies: map[string]*iampolicy.Stored{"projects/p": pol(iampolicy.Binding{
		Role: "roles/owner", Members: []string{"user:owner@example.com"}})}}
	on := true
	e := &Enforcer{Eval: &Evaluator{Source: src}, On: func() bool { return on }, OwnerEmail: "owner@example.com",
		Policies: map[string]PolicyGetter{ArtifactRegistry.Name: func(_ context.Context, r string) *iampolicy.Stored { return own[r] }}}
	ctx := context.Background()
	if err := e.AuthorizeAnonymous(ctx, ArtifactRegistry, pull, repo); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("anonymous pull of a private repository = %v; want PERMISSION_DENIED", err)
	}
	if err := e.Authorize(ctx, ArtifactRegistry, "", pull, repo); err != nil {
		t.Fatalf("Authorize with no bearer (the instance's account) = %v", err)
	}
	own[repo] = pol(iampolicy.Binding{Role: "roles/artifactregistry.reader", Members: []string{"allUsers"}})
	if err := e.AuthorizeAnonymous(ctx, ArtifactRegistry, pull, repo); err != nil {
		t.Fatalf("anonymous pull with allUsers Reader = %v", err)
	}
	if err := e.AuthorizeAnonymous(ctx, ArtifactRegistry, "artifactregistry.repositories.uploadArtifacts", repo); err == nil {
		t.Fatal("anonymous push with allUsers Reader was allowed")
	}
	own[repo] = pol(iampolicy.Binding{Role: "roles/artifactregistry.reader", Members: []string{"allAuthenticatedUsers"}})
	if err := e.AuthorizeAnonymous(ctx, ArtifactRegistry, pull, repo); err == nil {
		t.Fatal("anonymous pull with allAuthenticatedUsers Reader was allowed")
	}
	delete(own, repo)
	on = false
	if err := e.AuthorizeAnonymous(ctx, ArtifactRegistry, pull, repo); err != nil {
		t.Fatalf("anonymous pull with enforcement off = %v", err)
	}
}
