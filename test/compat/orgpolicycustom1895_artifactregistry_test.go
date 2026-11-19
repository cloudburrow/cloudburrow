//go:build compat

package compat

import (
	"testing"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateRepository
//
// TestOrgPolicyCustomConstraintsArtifactRegistry (#1895), with the
// official Artifact Registry client: a DENY on
// artifactregistry.googleapis.com/Repository resource.format == 'PYTHON'
// refuses a Python repository, FAILED_PRECONDITION "Operation denied by
// custom org policies: [...]" (the page's example output), and creates a
// Docker one; a DENY on the description (CREATE and UPDATE) refuses
// updating it to a forbidden one and allows another. A project without
// the policies is unaffected.
func TestOrgPolicyCustomConstraintsArtifactRegistry(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "oca-")
	noPython := customName(h, "arnopython")
	noDesc := customName(h, "arnodesc")
	pdesc, ddesc := "No Python repositories.", "No forbidden descriptions."
	createCustom(t, h, c, noPython, "artifactregistry.googleapis.com/Repository", "resource.format == 'PYTHON'", ccDeny, pdesc, ccCreate)
	createCustom(t, h, c, noDesc, "artifactregistry.googleapis.com/Repository", "resource.description.contains('forbidden')", ccDeny,
		ddesc, ccCreate, ccUpdate)
	setOrgPolicy(t, h, c, "projects/"+project, noPython, opEnforce())
	setOrgPolicy(t, h, c, "projects/"+project, noDesc, opEnforce())
	parent := "projects/" + project + "/locations/us-east1"
	_, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "py",
		Repository: &arpb.Repository{Format: arpb.Repository_PYTHON}})
	wantFailedPrecondition(t, "a Python repository", err, customMessage(noPython, pdesc))
	repo := formatRepo(t, ctx, ar, parent, "docker", &arpb.Repository{Format: arpb.Repository_DOCKER, Description: "fine"})
	_, err = ar.UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{Repository: &arpb.Repository{Name: repo.GetName(), Description: "forbidden"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	wantFailedPrecondition(t, "a forbidden description", err, customMessage(noDesc, ddesc))
	if _, err := ar.UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{Repository: &arpb.Repository{Name: repo.GetName(), Description: "still fine",
		Labels: map[string]string{"team": "a"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "labels"}}}); err != nil {
		t.Errorf("an allowed update: %v", err)
	}
	free := orgPolicyProject(t, h, pc, localOrg, "ocaf-")
	formatRepo(t, ctx, ar, "projects/"+free+"/locations/us-east1", "py", &arpb.Repository{Format: arpb.Repository_PYTHON})
}
