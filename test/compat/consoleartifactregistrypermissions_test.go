//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/iam/apiv1/iampb"
)

// TestConsolePermissionsOnARepository (#1439), in the shards that serve
// Cloud Run (Artifact Registry is on its port): the Permissions tab on an
// Artifact Registry repository's page drives the stored policy (#1304,
// ADR-0006), read back and changed through the official artifactregistry
// client's GetIamPolicy, SetIamPolicy and TestIamPermissions.
//
// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/GetIamPolicy, google.devtools.artifactregistry.v1.ArtifactRegistry/SetIamPolicy, google.devtools.artifactregistry.v1.ArtifactRegistry/TestIamPermissions
func TestConsolePermissionsOnARepository(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ctx := h.Context()
	addr := consoleAddr(t, h)
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	id := "console-perms-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/repositories/" + id
	op, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: runParent(h), RepositoryId: id,
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("waiting for the repository: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	consolePermissionsRoundTrip(t, h, addr, "artifact-registry", []string{id}, policyHandle{
		get: func(ctx context.Context) (*iampb.Policy, error) {
			return ar.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		},
		set: func(ctx context.Context, p *iampb.Policy) error {
			_, err := ar.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: p})
			return err
		},
		test: func(ctx context.Context, perms []string) ([]string, error) {
			r, err := ar.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name, Permissions: perms})
			return r.GetPermissions(), err
		},
	}, "roles/artifactregistry.reader", "artifactregistry.repositories.downloadArtifacts")
}
