package dataform

import (
	"context"
	"errors"
	"testing"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
)

// TestStrictActAs (#2173): creating or updating a repository, creating a
// workflow config and an invocation and updating a release config check
// actAs on the effective service account; the invocation runs with its
// account's token, and an account that cannot be minted fails it.
func TestStrictActAs(t *testing.T) {
	ctx := context.Background()
	allowed := map[string]bool{}
	var checked []string
	tokens := map[string]string{}
	s, err := NewServer(Options{DataDir: t.TempDir(), Warehouse: &tokenWarehouse{seen: tokens},
		ActAs: func(_ context.Context, project, account string) error {
			checked = append(checked, project+" "+account)
			if !allowed[account] {
				return errors.New("denied actAs on " + account)
			}
			return nil
		},
		RunAs: func(account string) (string, error) {
			if account == "gone@p.iam.gserviceaccount.com" {
				return "", errors.New("does not exist")
			}
			return "tok:" + account, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const parent = "projects/p/locations/us-central1"
	repoSA := "repo@p.iam.gserviceaccount.com"
	if _, err := s.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r", Repository: &dfpb.Repository{ServiceAccount: repoSA}}); err == nil {
		t.Fatal("CreateRepository without actAs on its service account succeeded")
	}
	allowed[repoSA] = true
	repo, err := s.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r", Repository: &dfpb.Repository{ServiceAccount: repoSA}})
	if err != nil {
		t.Fatal(err)
	}
	rel, err := s.CreateReleaseConfig(ctx, &dfpb.CreateReleaseConfigRequest{Parent: repo.GetName(), ReleaseConfigId: "rel", ReleaseConfig: &dfpb.ReleaseConfig{GitCommitish: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	comp := repo.GetName() + "/compilationResults/c1"
	s.mu.Lock()
	s.compilations[comp] = &compilation{C: &dfpb.CompilationResult{Name: comp}}
	s.mu.Unlock()
	other := "other@p.iam.gserviceaccount.com"
	if _, err := s.CreateWorkflowConfig(ctx, &dfpb.CreateWorkflowConfigRequest{Parent: repo.GetName(), WorkflowConfigId: "wc",
		WorkflowConfig: &dfpb.WorkflowConfig{ReleaseConfig: rel.GetName(), InvocationConfig: &dfpb.InvocationConfig{ServiceAccount: other}}}); err == nil {
		t.Error("CreateWorkflowConfig without actAs on its invocation's service account succeeded")
	}
	inv := func(sa string) (*dfpb.WorkflowInvocation, error) {
		return s.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo.GetName(), WorkflowInvocation: &dfpb.WorkflowInvocation{
			CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp}, InvocationConfig: &dfpb.InvocationConfig{ServiceAccount: sa}}})
	}
	if _, err := inv(other); err == nil {
		t.Error("an invocation without actAs on its service account succeeded")
	}
	// No account in the config: the repository's.
	checked = nil
	if _, err := inv(""); err != nil {
		t.Errorf("an invocation as the repository's account = %v", err)
	}
	if len(checked) != 1 || checked[0] != "p "+repoSA {
		t.Errorf("checked %v, want the repository's account", checked)
	}
	allowed["gone@p.iam.gserviceaccount.com"] = true
	if _, err := inv("gone@p.iam.gserviceaccount.com"); err == nil {
		t.Error("an invocation as an account that cannot be minted succeeded")
	}
	s.wg.Wait()
	allowed[repoSA] = false
	if _, err := s.UpdateReleaseConfig(ctx, &dfpb.UpdateReleaseConfigRequest{ReleaseConfig: &dfpb.ReleaseConfig{Name: rel.GetName(), GitCommitish: "dev"}}); err == nil {
		t.Error("UpdateReleaseConfig without actAs on the repository's account succeeded")
	}
}

// tokenWarehouse is a Warehouse recording each call's run token.
type tokenWarehouse struct{ seen map[string]string }

func (w *tokenWarehouse) EnsureDataset(ctx context.Context, project, dataset, _ string) error {
	return nil
}
func (w *tokenWarehouse) Table(context.Context, string, string, string) (bool, string, []string, error) {
	return false, "", nil, nil
}
func (w *tokenWarehouse) Exec(ctx context.Context, _, _, sql string) (string, error) {
	w.seen[sql] = RunToken(ctx)
	return "job", nil
}
func (w *tokenWarehouse) Count(ctx context.Context, _, _, sql string) (string, int64, error) {
	w.seen[sql] = RunToken(ctx)
	return "job", 0, nil
}
