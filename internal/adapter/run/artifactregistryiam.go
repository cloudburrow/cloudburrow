package run

import (
	"context"
	"encoding/json"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// A repository's IAM policy (#1304), stored and never enforced, under
// ADR-0006: Terraform's google_artifact_registry_repository_iam_member,
// _binding and _policy read and write it. It is kept beside the
// repositories, by repository name, saved to the registry's state directory
// with them, and removed with its repository, so a repository made again
// under the same name starts with no policy. A policy change leaves the
// repository alone, as on Google.

// repositoryPoliciesFile is where the policies are saved, beside
// repositoriesFile.
const repositoryPoliciesFile = "repository-iam.json"

// repositoryForIAM checks that name is a well-formed repository name of a
// repository that exists.
func repositoryForIAM(name string) error {
	if _, _, err := parseRepositoryName(name); err != nil {
		return err
	}
	if _, ok := repositories.repos[name]; !ok {
		return apierror.NotFound("Requested entity was not found.")
	}
	return nil
}

// GetIamPolicy is a repository's stored policy; nothing enforces it.
func (a *ArtifactRegistryServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	if err := repositoryForIAM(req.GetResource()); err != nil {
		return nil, err
	}
	var cur *iampolicy.Stored
	if st, ok := repositories.policies[req.GetResource()]; ok {
		cur = &st
	}
	return iampolicy.Get(cur, req.GetOptions())
}

// SetIamPolicy stores a repository's policy with a new etag; a stale etag
// is ABORTED.
func (a *ArtifactRegistryServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	if err := repositoryForIAM(req.GetResource()); err != nil {
		return nil, err
	}
	var cur *iampolicy.Stored
	if st, ok := repositories.policies[req.GetResource()]; ok {
		cur = &st
	}
	next, err := iampolicy.Set(cur, req)
	if err != nil {
		return nil, err
	}
	repositories.policies[req.GetResource()] = next
	saveRepositoryPoliciesLocked()
	return next.Proto(), nil
}

// TestIamPermissions grants every permission asked about on a repository
// that exists, and none on one that does not, as iam_policy.proto says.
func (a *ArtifactRegistryServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	repositories.mu.Lock()
	err := repositoryForIAM(req.GetResource())
	repositories.mu.Unlock()
	return testPermissions(err, req)
}

// saveRepositoryPoliciesLocked writes every stored policy to the registry's
// state directory; with no store they are kept in memory only.
func saveRepositoryPoliciesLocked() {
	st := RegistryStore()
	if st == nil {
		return
	}
	b, err := json.Marshal(repositories.policies)
	if err != nil {
		return
	}
	_ = st.WriteState(repositoryPoliciesFile, b)
}

// loadRepositoryPoliciesLocked reads the policies a previous run saved, of
// the repositories that are still there.
func loadRepositoryPoliciesLocked(st *registry.Store) {
	b, err := st.ReadState(repositoryPoliciesFile)
	if err != nil {
		return
	}
	saved := map[string]iampolicy.Stored{}
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for name, p := range saved {
		if _, ok := repositories.repos[name]; ok {
			repositories.policies[name] = p
		}
	}
}
