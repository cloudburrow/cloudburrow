package run

import (
	"context"
	"testing"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

type arCustoms struct{ list []orgpolicy.CustomConstraint }

func (arCustoms) Effective(context.Context, string, string) (orgpolicy.Effective, error) {
	return orgpolicy.Effective{}, nil
}

func (f arCustoms) Customs(_ context.Context, _, resourceType, _ string) ([]orgpolicy.CustomConstraint, error) {
	if resourceType != orgpolicy.TypeARRepository {
		return nil, nil
	}
	return f.list, nil
}

// TestRepositoryCustomConstraints (#1895): the Artifact Registry page's
// example, an ALLOW on resource.format == 'DOCKER', refuses creating a
// PYTHON repository, FAILED_PRECONDITION "Operation denied by custom org
// policies: [...]"; a DENY on the description refuses an update to it;
// labels, which the page excludes, cannot be evaluated.
func TestRepositoryCustomConstraints(t *testing.T) {
	enforced := orgpolicy.AsResponse(orgpolicy.Effective{Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
		{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}})
	orgpolicy.Use(arCustoms{list: []orgpolicy.CustomConstraint{
		{Name: "custom.docker", Condition: "resource.format == 'DOCKER' && resource.cleanupPolicyDryRun == false", ActionType: "ALLOW",
			Description: "Docker only.", Effective: enforced},
		{Name: "custom.desc", Condition: "resource.description.contains('forbidden')", ActionType: "DENY",
			Description: "No forbidden descriptions.", Effective: enforced},
	}})
	defer orgpolicy.Use(nil)
	ctx := context.Background()
	a := &ArtifactRegistryServer{}
	parent := "projects/ar-custom-test/locations/us-west1"
	_, err := a.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "py",
		Repository: &arpb.Repository{Format: arpb.Repository_PYTHON}})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition ||
		st.Message() != `Operation denied by custom org policies: ["customConstraints/custom.docker":"Docker only."]` {
		t.Errorf("a PYTHON repository = %v", err)
	}
	name := parent + "/repositories/d"
	repositories.mu.Lock()
	if repositories.repos == nil {
		repositories.repos = map[string]*arpb.Repository{}
	}
	repositories.repos[name] = &arpb.Repository{Name: name, Format: arpb.Repository_DOCKER, Mode: arpb.Repository_STANDARD_REPOSITORY}
	repositories.mu.Unlock()
	t.Cleanup(func() {
		repositories.mu.Lock()
		delete(repositories.repos, name)
		repositories.mu.Unlock()
	})
	_, err = a.UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{Repository: &arpb.Repository{Name: name, Description: "forbidden"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a forbidden description = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := a.UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{Repository: &arpb.Repository{Name: name, Description: "fine"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}}); err != nil {
		t.Errorf("an allowed description: %v", err)
	}
	orgpolicy.Use(arCustoms{list: []orgpolicy.CustomConstraint{{Name: "custom.labels", Condition: "size(resource.labels) == 0",
		ActionType: "ALLOW", Description: "labels", Effective: enforced}}})
	if err := checkRepositoryCustom(ctx, &arpb.Repository{Name: name}, orgpolicy.MethodUpdate); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a condition on labels = %v, want FAILED_PRECONDITION (not evaluable)", err)
	}
}
