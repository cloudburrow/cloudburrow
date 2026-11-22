//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	dataform "cloud.google.com/go/dataform/apiv1"
	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// EnvDataform is Dataform's endpoint (#2105), gRPC and JSON on one port.
const EnvDataform = "CLOUDBURROW_TEST_DATAFORM"

func dataformClients(t *testing.T, h *Harness) map[string]*dataform.Client {
	t.Helper()
	addr := h.Endpoint(EnvDataform)
	ctx := h.Context()
	g, err := dataform.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err := dataform.NewRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return map[string]*dataform.Client{"grpc": g, "rest": r}
}

// dataformBigQuery is the BigQuery client the invocations' results are read
// with, in the one project the emulator serves.
func dataformBigQuery(t *testing.T, h *Harness) (*bigquery.Client, string) {
	t.Helper()
	endpoint := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_BIGQUERY", EnvBigQuery)
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set: the emulator serves one project, and the test must use it", EnvBigQueryProject)
	}
	c, err := bigquery.NewClient(h.Context(), project, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, project
}

func dataformWrite(ctx context.Context, t *testing.T, c *dataform.Client, ws, path, contents string) {
	t.Helper()
	if _, err := c.WriteFile(ctx, &dfpb.WriteFileRequest{Workspace: ws, Path: path, Contents: []byte(contents)}); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func waitInvocation(ctx context.Context, t *testing.T, c *dataform.Client, name string) *dfpb.WorkflowInvocation {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for {
		inv, err := c.GetWorkflowInvocation(ctx, &dfpb.GetWorkflowInvocationRequest{Name: name})
		if err != nil {
			t.Fatalf("GetWorkflowInvocation: %v", err)
		}
		if inv.GetState() != dfpb.WorkflowInvocation_RUNNING && inv.GetState() != dfpb.WorkflowInvocation_CANCELING {
			return inv
		}
		if time.Now().After(deadline) {
			t.Fatalf("invocation %s still %v after 4 minutes", name, inv.GetState())
		}
		time.Sleep(time.Second)
	}
}

func invocationActions(ctx context.Context, t *testing.T, c *dataform.Client, name string) map[string]*dfpb.WorkflowInvocationAction {
	t.Helper()
	out := map[string]*dfpb.WorkflowInvocationAction{}
	it := c.QueryWorkflowInvocationActions(ctx, &dfpb.QueryWorkflowInvocationActionsRequest{Name: name})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("QueryWorkflowInvocationActions: %v", err)
		}
		out[a.GetTarget().GetName()] = a
	}
}

// covers: google.cloud.dataform.v1.Dataform/CreateRepository, google.cloud.dataform.v1.Dataform/GetRepository, google.cloud.dataform.v1.Dataform/ListRepositories, google.cloud.dataform.v1.Dataform/UpdateRepository, google.cloud.dataform.v1.Dataform/DeleteRepository, google.cloud.dataform.v1.Dataform/CreateWorkspace, google.cloud.dataform.v1.Dataform/GetWorkspace, google.cloud.dataform.v1.Dataform/ListWorkspaces, google.cloud.dataform.v1.Dataform/WriteFile, google.cloud.dataform.v1.Dataform/ReadFile, google.cloud.dataform.v1.Dataform/MakeDirectory, google.cloud.dataform.v1.Dataform/QueryDirectoryContents, google.cloud.dataform.v1.Dataform/SearchFiles, google.cloud.dataform.v1.Dataform/MoveFile, google.cloud.dataform.v1.Dataform/RemoveFile, google.cloud.dataform.v1.Dataform/MoveDirectory, google.cloud.dataform.v1.Dataform/RemoveDirectory, google.cloud.dataform.v1.Dataform/FetchFileGitStatuses, google.cloud.dataform.v1.Dataform/FetchFileDiff, google.cloud.dataform.v1.Dataform/CommitWorkspaceChanges, google.cloud.dataform.v1.Dataform/ResetWorkspaceChanges, google.cloud.dataform.v1.Dataform/PushGitCommits, google.cloud.dataform.v1.Dataform/PullGitCommits, google.cloud.dataform.v1.Dataform/FetchGitAheadBehind, google.cloud.dataform.v1.Dataform/FetchRepositoryHistory, google.cloud.dataform.v1.Dataform/ReadRepositoryFile, google.cloud.dataform.v1.Dataform/QueryRepositoryDirectoryContents, google.cloud.dataform.v1.Dataform/CommitRepositoryChanges, google.cloud.dataform.v1.Dataform/DeleteWorkspace, google.cloud.dataform.v1.Dataform/GetIamPolicy, google.cloud.dataform.v1.Dataform/SetIamPolicy, google.cloud.dataform.v1.Dataform/TestIamPermissions
//
// TestDataformWorkspaceFilesAndGit (#2105): over gRPC and REST, a
// repository without a git remote and two workspaces: files and
// directories written, listed, searched, moved and removed; uncommitted
// changes reported, diffed, reset and committed; a push to the
// repository's own git, whose history and files read back, and a pull into
// the second workspace; and a commit made on the repository directly.
func TestDataformWorkspaceFilesAndGit(t *testing.T) {
	h := New(t)
	clients := dataformClients(t, h)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	ctx := h.Context()
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			parent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
			id := "r" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
			repo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id,
				Repository: &dfpb.Repository{DisplayName: "first", Labels: map[string]string{"team": "data"}}})
			if err != nil {
				t.Fatalf("CreateRepository: %v", err)
			}
			t.Cleanup(func() {
				_ = c.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
			})
			if _, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id}); err == nil {
				t.Error("a second CreateRepository with the same ID succeeded")
			}
			if got, err := c.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: repo.GetName()}); err != nil || got.GetDisplayName() != "first" || got.GetCreateTime() == nil {
				t.Errorf("GetRepository = %v, %v", got, err)
			}
			up, err := c.UpdateRepository(ctx, &dfpb.UpdateRepositoryRequest{Repository: &dfpb.Repository{Name: repo.GetName(), DisplayName: "second"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
			if err != nil || up.GetDisplayName() != "second" || up.GetLabels()["team"] != "data" {
				t.Errorf("UpdateRepository = %v, %v", up, err)
			}
			listed := false
			for it := c.ListRepositories(ctx, &dfpb.ListRepositoriesRequest{Parent: parent}); ; {
				r, err := it.Next()
				if err != nil {
					break
				}
				listed = listed || r.GetName() == repo.GetName()
			}
			if !listed {
				t.Error("ListRepositories does not list the repository")
			}

			ws, err := c.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "dev"})
			if err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			if got, err := c.GetWorkspace(ctx, &dfpb.GetWorkspaceRequest{Name: ws.GetName()}); err != nil || got.GetName() != ws.GetName() {
				t.Errorf("GetWorkspace = %v, %v", got, err)
			}
			dataformWrite(ctx, t, c, ws.GetName(), "definitions/a.sqlx", "config { type: \"view\" }\nselect 1 as x\n")
			dataformWrite(ctx, t, c, ws.GetName(), "workflow_settings.yaml", "defaultProject: p\ndefaultLocation: US\ndefaultDataset: d\n")
			if _, err := c.MakeDirectory(ctx, &dfpb.MakeDirectoryRequest{Workspace: ws.GetName(), Path: "includes"}); err != nil {
				t.Fatalf("MakeDirectory: %v", err)
			}
			if got, err := c.ReadFile(ctx, &dfpb.ReadFileRequest{Workspace: ws.GetName(), Path: "definitions/a.sqlx"}); err != nil || !strings.Contains(string(got.GetFileContents()), "select 1") {
				t.Errorf("ReadFile = %v, %v", got, err)
			}
			var entries []string
			for it := c.QueryDirectoryContents(ctx, &dfpb.QueryDirectoryContentsRequest{Workspace: ws.GetName()}); ; {
				e, err := it.Next()
				if err != nil {
					break
				}
				entries = append(entries, e.GetFile()+e.GetDirectory())
			}
			if strings.Join(entries, ",") != "definitions,includes,workflow_settings.yaml" {
				t.Errorf("QueryDirectoryContents = %v", entries)
			}
			var found []string
			for it := c.SearchFiles(ctx, &dfpb.SearchFilesRequest{Workspace: ws.GetName(), Filter: `path:"sqlx"`}); ; {
				r, err := it.Next()
				if err != nil {
					break
				}
				found = append(found, r.GetFile().GetPath())
			}
			if strings.Join(found, ",") != "definitions/a.sqlx" {
				t.Errorf("SearchFiles = %v", found)
			}
			st, err := c.FetchFileGitStatuses(ctx, &dfpb.FetchFileGitStatusesRequest{Name: ws.GetName()})
			if err != nil || len(st.GetUncommittedFileChanges()) != 2 || st.GetUncommittedFileChanges()[0].GetState() != dfpb.FetchFileGitStatusesResponse_UncommittedFileChange_ADDED {
				t.Errorf("FetchFileGitStatuses = %v, %v", st, err)
			}
			author := &dfpb.CommitAuthor{Name: "Dev", EmailAddress: "dev@example.com"}
			if _, err := c.CommitWorkspaceChanges(ctx, &dfpb.CommitWorkspaceChangesRequest{Name: ws.GetName(), Author: author, CommitMessage: "first"}); err != nil {
				t.Fatalf("CommitWorkspaceChanges: %v", err)
			}
			dataformWrite(ctx, t, c, ws.GetName(), "definitions/a.sqlx", "config { type: \"view\" }\nselect 2 as x\n")
			diff, err := c.FetchFileDiff(ctx, &dfpb.FetchFileDiffRequest{Workspace: ws.GetName(), Path: "definitions/a.sqlx"})
			if err != nil || !strings.Contains(diff.GetFormattedDiff(), "-select 1 as x") || !strings.Contains(diff.GetFormattedDiff(), "+select 2 as x") {
				t.Errorf("FetchFileDiff = %v, %v", diff, err)
			}
			if _, err := c.ResetWorkspaceChanges(ctx, &dfpb.ResetWorkspaceChangesRequest{Name: ws.GetName(), Paths: []string{"definitions/a.sqlx"}}); err != nil {
				t.Fatalf("ResetWorkspaceChanges: %v", err)
			}
			if got, _ := c.ReadFile(ctx, &dfpb.ReadFileRequest{Workspace: ws.GetName(), Path: "definitions/a.sqlx"}); !strings.Contains(string(got.GetFileContents()), "select 1") {
				t.Errorf("the reset left %q", got.GetFileContents())
			}
			if _, err := c.MoveFile(ctx, &dfpb.MoveFileRequest{Workspace: ws.GetName(), Path: "definitions/a.sqlx", NewPath: "definitions/b.sqlx"}); err != nil {
				t.Fatalf("MoveFile: %v", err)
			}
			if _, err := c.ReadFile(ctx, &dfpb.ReadFileRequest{Workspace: ws.GetName(), Path: "definitions/a.sqlx"}); err == nil {
				t.Error("the moved file is still at its old path")
			}
			if _, err := c.MoveDirectory(ctx, &dfpb.MoveDirectoryRequest{Workspace: ws.GetName(), Path: "includes", NewPath: "lib"}); err != nil {
				t.Fatalf("MoveDirectory: %v", err)
			}
			if _, err := c.RemoveDirectory(ctx, &dfpb.RemoveDirectoryRequest{Workspace: ws.GetName(), Path: "lib"}); err != nil {
				t.Fatalf("RemoveDirectory: %v", err)
			}
			if _, err := c.RemoveFile(ctx, &dfpb.RemoveFileRequest{Workspace: ws.GetName(), Path: "definitions/b.sqlx"}); err != nil {
				t.Fatalf("RemoveFile: %v", err)
			}
			if _, err := c.ResetWorkspaceChanges(ctx, &dfpb.ResetWorkspaceChangesRequest{Name: ws.GetName(), Clean: true}); err != nil {
				t.Fatalf("ResetWorkspaceChanges(clean): %v", err)
			}
			ab, err := c.FetchGitAheadBehind(ctx, &dfpb.FetchGitAheadBehindRequest{Name: ws.GetName()})
			if err != nil || ab.GetCommitsAhead() != 1 || ab.GetCommitsBehind() != 0 {
				t.Errorf("FetchGitAheadBehind before the push = %v, %v", ab, err)
			}
			if _, err := c.PushGitCommits(ctx, &dfpb.PushGitCommitsRequest{Name: ws.GetName()}); err != nil {
				t.Fatalf("PushGitCommits: %v", err)
			}
			hist := c.FetchRepositoryHistory(ctx, &dfpb.FetchRepositoryHistoryRequest{Name: repo.GetName()})
			first, err := hist.Next()
			if err != nil || first.GetCommitMessage() != "first" || first.GetAuthor().GetEmailAddress() != "dev@example.com" {
				t.Fatalf("FetchRepositoryHistory = %v, %v", first, err)
			}
			if f, err := c.ReadRepositoryFile(ctx, &dfpb.ReadRepositoryFileRequest{Name: repo.GetName(), Path: "definitions/a.sqlx"}); err != nil || !strings.Contains(string(f.GetContents()), "select 1") {
				t.Errorf("ReadRepositoryFile = %v, %v", f, err)
			}
			var dir []string
			for it := c.QueryRepositoryDirectoryContents(ctx, &dfpb.QueryRepositoryDirectoryContentsRequest{Name: repo.GetName(), CommitSha: first.GetCommitSha()}); ; {
				e, err := it.Next()
				if err != nil {
					break
				}
				dir = append(dir, e.GetFile()+e.GetDirectory())
			}
			if strings.Join(dir, ",") != "definitions,workflow_settings.yaml" {
				t.Errorf("QueryRepositoryDirectoryContents = %v", dir)
			}
			commit, err := c.CommitRepositoryChanges(ctx, &dfpb.CommitRepositoryChangesRequest{Name: repo.GetName(), RequiredHeadCommitSha: first.GetCommitSha(),
				CommitMetadata: &dfpb.CommitMetadata{Author: author, CommitMessage: "direct"},
				FileOperations: map[string]*dfpb.CommitRepositoryChangesRequest_FileOperation{
					"README.md": {Operation: &dfpb.CommitRepositoryChangesRequest_FileOperation_WriteFile_{WriteFile: &dfpb.CommitRepositoryChangesRequest_FileOperation_WriteFile{Contents: []byte("hello")}}},
				}})
			if err != nil || len(commit.GetCommitSha()) != 40 {
				t.Fatalf("CommitRepositoryChanges = %v, %v", commit, err)
			}
			if _, err := c.CommitRepositoryChanges(ctx, &dfpb.CommitRepositoryChangesRequest{Name: repo.GetName(), RequiredHeadCommitSha: first.GetCommitSha(),
				CommitMetadata: &dfpb.CommitMetadata{Author: author},
				FileOperations: map[string]*dfpb.CommitRepositoryChangesRequest_FileOperation{
					"x": {Operation: &dfpb.CommitRepositoryChangesRequest_FileOperation_DeleteFile_{DeleteFile: &dfpb.CommitRepositoryChangesRequest_FileOperation_DeleteFile{}}},
				}}); err == nil {
				t.Error("CommitRepositoryChanges with a stale required_head_commit_sha succeeded")
			}
			ab, err = c.FetchGitAheadBehind(ctx, &dfpb.FetchGitAheadBehindRequest{Name: ws.GetName()})
			if err != nil || ab.GetCommitsAhead() != 0 || ab.GetCommitsBehind() != 1 {
				t.Errorf("FetchGitAheadBehind after the direct commit = %v, %v", ab, err)
			}
			if _, err := c.PullGitCommits(ctx, &dfpb.PullGitCommitsRequest{Name: ws.GetName(), Author: author}); err != nil {
				t.Fatalf("PullGitCommits: %v", err)
			}
			if got, err := c.ReadFile(ctx, &dfpb.ReadFileRequest{Workspace: ws.GetName(), Path: "README.md"}); err != nil || string(got.GetFileContents()) != "hello" {
				t.Errorf("ReadFile after the pull = %v, %v", got, err)
			}
			ws2, err := c.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "other"})
			if err != nil {
				t.Fatalf("CreateWorkspace other: %v", err)
			}
			if got, err := c.ReadFile(ctx, &dfpb.ReadFileRequest{Workspace: ws2.GetName(), Path: "README.md"}); err != nil || string(got.GetFileContents()) != "hello" {
				t.Errorf("a new workspace does not start from the default branch: %v, %v", got, err)
			}
			n := 0
			for it := c.ListWorkspaces(ctx, &dfpb.ListWorkspacesRequest{Parent: repo.GetName()}); ; {
				if _, err := it.Next(); err != nil {
					break
				}
				n++
			}
			if n != 2 {
				t.Errorf("ListWorkspaces listed %d, want 2", n)
			}
			pol, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: repo.GetName(), Policy: &iampb.Policy{Bindings: []*iampb.Binding{{Role: "roles/dataform.editor", Members: []string{"user:dev@example.com"}}}}})
			if err != nil || len(pol.GetBindings()) != 1 {
				t.Errorf("SetIamPolicy = %v, %v", pol, err)
			}
			if got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: repo.GetName()}); err != nil || len(got.GetBindings()) != 1 {
				t.Errorf("GetIamPolicy = %v, %v", got, err)
			}
			if got, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: repo.GetName(), Permissions: []string{"dataform.repositories.get"}}); err != nil || len(got.GetPermissions()) != 1 {
				t.Errorf("TestIamPermissions = %v, %v", got, err)
			}
			if err := c.DeleteWorkspace(ctx, &dfpb.DeleteWorkspaceRequest{Name: ws2.GetName()}); err != nil {
				t.Errorf("DeleteWorkspace: %v", err)
			}
			if err := c.DeleteRepository(ctx, &dfpb.DeleteRepositoryRequest{Name: repo.GetName()}); err == nil {
				t.Error("DeleteRepository of a repository with a workspace succeeded without force")
			}
			if err := c.DeleteRepository(ctx, &dfpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true}); err != nil {
				t.Errorf("DeleteRepository(force): %v", err)
			}
			if _, err := c.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: repo.GetName()}); err == nil {
				t.Error("the deleted repository is still there")
			}
		})
	}
}

// dataformProject writes a SQLX project: a table x of two rows, a view,
// an incremental table, an operation, an assertion that passes and one
// that fails.
func dataformProject(ctx context.Context, t *testing.T, c *dataform.Client, ws, project, dataset string) {
	t.Helper()
	dataformWrite(ctx, t, c, ws, "workflow_settings.yaml", fmt.Sprintf(
		"defaultProject: %s\ndefaultLocation: US\ndefaultDataset: %s\ndefaultAssertionDataset: %s_assertions\ndataformCoreVersion: 3.1.6\n", project, dataset, dataset))
	dataformWrite(ctx, t, c, ws, "definitions/x.sqlx", "config { type: \"table\", tags: [\"daily\"] }\nselect 1 as a, 'one' as b union all select 2 as a, 'two' as b\n")
	dataformWrite(ctx, t, c, ws, "definitions/v.sqlx", "config { type: \"view\" }\nselect a * 10 as a10 from ${ref(\"x\")}\n")
	dataformWrite(ctx, t, c, ws, "definitions/inc.sqlx", "config { type: \"incremental\" }\nselect a from ${ref(\"x\")} ${when(incremental(), \"where a > 1\")}\n")
	dataformWrite(ctx, t, c, ws, "definitions/op.sqlx", "config { type: \"operations\", dependencies: [\"x\"] }\nselect count(*) from ${ref(\"x\")}\n")
	dataformWrite(ctx, t, c, ws, "definitions/ok.sqlx", "config { type: \"assertion\" }\nselect * from ${ref(\"x\")} where a > 2\n")
	dataformWrite(ctx, t, c, ws, "definitions/bad.sqlx", "config { type: \"assertion\" }\nselect * from ${ref(\"x\")} where a = 1\n")
}

// covers: google.cloud.dataform.v1.Dataform/CommitWorkspaceChanges, google.cloud.dataform.v1.Dataform/CreateCompilationResult, google.cloud.dataform.v1.Dataform/CreateReleaseConfig, google.cloud.dataform.v1.Dataform/CreateRepository, google.cloud.dataform.v1.Dataform/CreateWorkflowConfig, google.cloud.dataform.v1.Dataform/CreateWorkflowInvocation, google.cloud.dataform.v1.Dataform/CreateWorkspace, google.cloud.dataform.v1.Dataform/DeleteReleaseConfig, google.cloud.dataform.v1.Dataform/DeleteRepository, google.cloud.dataform.v1.Dataform/DeleteWorkflowConfig, google.cloud.dataform.v1.Dataform/DeleteWorkflowInvocation, google.cloud.dataform.v1.Dataform/GetCompilationResult, google.cloud.dataform.v1.Dataform/GetReleaseConfig, google.cloud.dataform.v1.Dataform/GetWorkflowConfig, google.cloud.dataform.v1.Dataform/GetWorkspace, google.cloud.dataform.v1.Dataform/ListCompilationResults, google.cloud.dataform.v1.Dataform/ListReleaseConfigs, google.cloud.dataform.v1.Dataform/ListWorkflowConfigs, google.cloud.dataform.v1.Dataform/ListWorkflowInvocations, google.cloud.dataform.v1.Dataform/PushGitCommits, google.cloud.dataform.v1.Dataform/QueryCompilationResultActions, google.cloud.dataform.v1.Dataform/RemoveFile, google.cloud.dataform.v1.Dataform/UpdateReleaseConfig, google.cloud.dataform.v1.Dataform/UpdateWorkflowConfig, google.cloud.dataform.v1.Dataform/WriteFile, google.cloud.dataform.v1.Dataform/GetWorkflowInvocation, google.cloud.dataform.v1.Dataform/QueryWorkflowInvocationActions
//
// TestDataformCompileAndInvoke (#2105): a workspace's definitions/x.sqlx
// and its neighbours compile with Dataform core into a table, a view, an
// incremental table, an operation and two assertions; the invocation runs
// them on BigQuery, where x has its two rows, the view and the incremental
// table exist, the passing assertion SUCCEEDED and the failing one FAILED,
// so the invocation FAILED. A second invocation of the incremental table
// alone inserts into it. A release config compiles the committed main
// branch, and a workflow config invokes its release.
func TestDataformCompileAndInvoke(t *testing.T) {
	h := New(t)
	clients := dataformClients(t, h)
	bq, project := dataformBigQuery(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: Dataform core runs in a container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	c := clients["grpc"]
	rest := clients["rest"]
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	dataset := "df_" + suffix
	t.Cleanup(func() {
		_ = bq.Dataset(dataset).DeleteWithContents(context.Background())
		_ = bq.Dataset(dataset + "_assertions").DeleteWithContents(context.Background())
	})
	repo, err := rest.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r" + suffix})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
	})
	ws, err := c.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "dev"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	dataformProject(ctx, t, c, ws.GetName(), project, dataset)

	comp, err := rest.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(),
		CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_Workspace{Workspace: ws.GetName()}}})
	if err != nil {
		t.Fatalf("CreateCompilationResult: %v", err)
	}
	if len(comp.GetCompilationErrors()) > 0 || comp.GetDataformCoreVersion() == "" {
		t.Fatalf("the compilation result = %v", comp)
	}
	kinds := map[string]string{}
	for it := c.QueryCompilationResultActions(ctx, &dfpb.QueryCompilationResultActionsRequest{Name: comp.GetName()}); ; {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("QueryCompilationResultActions: %v", err)
		}
		switch {
		case a.GetRelation() != nil:
			kinds[a.GetTarget().GetName()] = a.GetRelation().GetRelationType().String()
		case a.GetOperations() != nil:
			kinds[a.GetTarget().GetName()] = "OPERATIONS"
		case a.GetAssertion() != nil:
			kinds[a.GetTarget().GetName()] = "ASSERTION"
		}
		if a.GetTarget().GetDatabase() != project {
			t.Errorf("action %v: database %q, want %q", a.GetTarget(), a.GetTarget().GetDatabase(), project)
		}
	}
	want := map[string]string{"x": "TABLE", "v": "VIEW", "inc": "INCREMENTAL_TABLE", "op": "OPERATIONS", "ok": "ASSERTION", "bad": "ASSERTION"}
	for k, v := range want {
		if kinds[k] != v {
			t.Errorf("action %s compiled as %q, want %q (all: %v)", k, kinds[k], v, kinds)
		}
	}
	if got, err := c.GetCompilationResult(ctx, &dfpb.GetCompilationResultRequest{Name: comp.GetName()}); err != nil || got.GetWorkspace() != ws.GetName() {
		t.Errorf("GetCompilationResult = %v, %v", got, err)
	}

	// A compilation error is reported against its file.
	dataformWrite(ctx, t, c, ws.GetName(), "definitions/broken.sqlx", "config { type: \"table\" }\nselect * from ${ref(\"missing\")}\n")
	broken, err := c.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(),
		CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_Workspace{Workspace: ws.GetName()}}})
	if err != nil {
		t.Fatalf("CreateCompilationResult of the broken project: %v", err)
	}
	if len(broken.GetCompilationErrors()) == 0 || broken.GetCompilationErrors()[0].GetPath() != "definitions/broken.sqlx" {
		t.Errorf("the broken project's errors = %v", broken.GetCompilationErrors())
	}
	if _, err := c.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo.GetName(),
		WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: broken.GetName()}}}); err == nil {
		t.Error("a compilation result with errors was invoked")
	}
	if _, err := c.RemoveFile(ctx, &dfpb.RemoveFileRequest{Workspace: ws.GetName(), Path: "definitions/broken.sqlx"}); err != nil {
		t.Fatal(err)
	}

	// builtin_assertion_name_prefix (#2094) renames the assertions Dataform
	// core generates from a table's assertions config, and only those.
	dataformWrite(ctx, t, c, ws.GetName(), "definitions/checked.sqlx", "config { type: \"table\", assertions: { nonNull: [\"a\"] } }\nselect 1 as a\n")
	pre, err := c.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(),
		CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_Workspace{Workspace: ws.GetName()},
			CodeCompilationConfig: &dfpb.CodeCompilationConfig{BuiltinAssertionNamePrefix: "chk_"}}})
	if err != nil {
		t.Fatalf("CreateCompilationResult with builtin_assertion_name_prefix: %v", err)
	}
	builtin, manual := 0, 0
	for it := c.QueryCompilationResultActions(ctx, &dfpb.QueryCompilationResultActionsRequest{Name: pre.GetName()}); ; {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("QueryCompilationResultActions: %v", err)
		}
		as := a.GetAssertion()
		switch {
		case as == nil:
		case as.GetParentAction().GetName() == "checked":
			builtin++
			if !strings.HasPrefix(a.GetTarget().GetName(), "chk_") {
				t.Errorf("built-in assertion %v has no prefix", a.GetTarget())
			}
		default:
			manual++
			if strings.HasPrefix(a.GetTarget().GetName(), "chk_") {
				t.Errorf("hand-written assertion %v was prefixed", a.GetTarget())
			}
		}
	}
	if builtin == 0 || manual != 2 {
		t.Errorf("built-in assertions %d, hand-written %d, want >0 and 2", builtin, manual)
	}
	if _, err := c.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(),
		CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_Workspace{Workspace: ws.GetName()},
			CodeCompilationConfig: &dfpb.CodeCompilationConfig{BuiltinAssertionNamePrefix: "not valid"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an invalid prefix = %v, want InvalidArgument", err)
	}
	if _, err := c.RemoveFile(ctx, &dfpb.RemoveFileRequest{Workspace: ws.GetName(), Path: "definitions/checked.sqlx"}); err != nil {
		t.Fatal(err)
	}

	inv, err := rest.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo.GetName(),
		WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()}}})
	if err != nil {
		t.Fatalf("CreateWorkflowInvocation: %v", err)
	}
	done := waitInvocation(ctx, t, c, inv.GetName())
	acts := invocationActions(ctx, t, rest, inv.GetName())
	for name, st := range map[string]dfpb.WorkflowInvocationAction_State{"x": dfpb.WorkflowInvocationAction_SUCCEEDED, "v": dfpb.WorkflowInvocationAction_SUCCEEDED,
		"inc": dfpb.WorkflowInvocationAction_SUCCEEDED, "op": dfpb.WorkflowInvocationAction_SUCCEEDED, "ok": dfpb.WorkflowInvocationAction_SUCCEEDED, "bad": dfpb.WorkflowInvocationAction_FAILED} {
		if acts[name].GetState() != st {
			t.Errorf("action %s = %v (%s), want %v", name, acts[name].GetState(), acts[name].GetFailureReason(), st)
		}
	}
	if !strings.Contains(acts["bad"].GetFailureReason(), "Assertion failed") {
		t.Errorf("the failing assertion's reason = %q", acts["bad"].GetFailureReason())
	}
	if !strings.Contains(acts["x"].GetBigqueryAction().GetSqlScript(), "create or replace table") {
		t.Errorf("x's SQL = %q", acts["x"].GetBigqueryAction().GetSqlScript())
	}
	if done.GetState() != dfpb.WorkflowInvocation_FAILED || done.GetInvocationTiming().GetEndTime() == nil {
		t.Errorf("the invocation = %v, want FAILED (an assertion failed)", done)
	}
	rows := func(q string) []string {
		it, err := bq.Query(q).Read(ctx)
		if err != nil {
			t.Fatalf("query %s: %v", q, err)
		}
		var out []string
		for {
			var r []bigquery.Value
			err := it.Next(&r)
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			var parts []string
			for _, v := range r {
				parts = append(parts, fmt.Sprint(v))
			}
			out = append(out, strings.Join(parts, ""))
		}
	}
	if got := rows(fmt.Sprintf("select a, b from `%s.%s.x` order by a", project, dataset)); strings.Join(got, ",") != "1one,2two" {
		t.Errorf("table x = %v, want 1one,2two", got)
	}
	if got := rows(fmt.Sprintf("select a10 from `%s.%s.v` order by a10", project, dataset)); strings.Join(got, ",") != "10,20" {
		t.Errorf("view v = %v", got)
	}
	if got := rows(fmt.Sprintf("select a from `%s.%s.inc` order by a", project, dataset)); strings.Join(got, ",") != "1,2" {
		t.Errorf("incremental table inc after the first run = %v", got)
	}

	// The incremental table alone, again: its incremental query inserts.
	inv2, err := c.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo.GetName(),
		WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()},
			InvocationConfig: &dfpb.InvocationConfig{IncludedTargets: []*dfpb.Target{{Database: project, Schema: dataset, Name: "inc"}}}}})
	if err != nil {
		t.Fatalf("CreateWorkflowInvocation(inc): %v", err)
	}
	if got := waitInvocation(ctx, t, c, inv2.GetName()); got.GetState() != dfpb.WorkflowInvocation_SUCCEEDED {
		t.Errorf("the incremental invocation = %v, %v", got, invocationActions(ctx, t, c, inv2.GetName()))
	}
	if acts := invocationActions(ctx, t, c, inv2.GetName()); len(acts) != 1 || !strings.Contains(acts["inc"].GetBigqueryAction().GetSqlScript(), "insert into") {
		t.Errorf("the incremental invocation's actions = %v", acts)
	}
	if got := rows(fmt.Sprintf("select a from `%s.%s.inc` order by a", project, dataset)); strings.Join(got, ",") != "1,2,2" {
		t.Errorf("incremental table inc after the second run = %v, want 1,2,2", got)
	}
	listed := 0
	for it := c.ListWorkflowInvocations(ctx, &dfpb.ListWorkflowInvocationsRequest{Parent: repo.GetName()}); ; {
		if _, err := it.Next(); err != nil {
			break
		}
		listed++
	}
	if listed != 2 {
		t.Errorf("ListWorkflowInvocations listed %d, want 2", listed)
	}
	if err := c.DeleteWorkflowInvocation(ctx, &dfpb.DeleteWorkflowInvocationRequest{Name: inv2.GetName()}); err != nil {
		t.Errorf("DeleteWorkflowInvocation: %v", err)
	}

	// A release config of the committed main branch, and a workflow config
	// of its release that runs the table alone.
	author := &dfpb.CommitAuthor{Name: "Dev", EmailAddress: "dev@example.com"}
	if _, err := c.CommitWorkspaceChanges(ctx, &dfpb.CommitWorkspaceChangesRequest{Name: ws.GetName(), Author: author, CommitMessage: "project"}); err != nil {
		t.Fatalf("CommitWorkspaceChanges: %v", err)
	}
	if _, err := c.PushGitCommits(ctx, &dfpb.PushGitCommitsRequest{Name: ws.GetName()}); err != nil {
		t.Fatalf("PushGitCommits: %v", err)
	}
	rc, err := c.CreateReleaseConfig(ctx, &dfpb.CreateReleaseConfigRequest{Parent: repo.GetName(), ReleaseConfigId: "prod",
		ReleaseConfig: &dfpb.ReleaseConfig{GitCommitish: "main", CronSchedule: "0 3 * * *", TimeZone: "Etc/UTC",
			CodeCompilationConfig: &dfpb.CodeCompilationConfig{Vars: map[string]string{"env": "prod"}}}})
	if err != nil {
		t.Fatalf("CreateReleaseConfig: %v", err)
	}
	rcomp, err := c.CreateCompilationResult(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(),
		CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_ReleaseConfig{ReleaseConfig: rc.GetName()}}})
	if err != nil || len(rcomp.GetResolvedGitCommitSha()) != 40 || rcomp.GetCodeCompilationConfig().GetVars()["env"] != "prod" {
		t.Fatalf("CreateCompilationResult(release config) = %v, %v", rcomp, err)
	}
	rc, err = c.UpdateReleaseConfig(ctx, &dfpb.UpdateReleaseConfigRequest{ReleaseConfig: &dfpb.ReleaseConfig{Name: rc.GetName(), ReleaseCompilationResult: rcomp.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"release_compilation_result"}}})
	if err != nil || rc.GetReleaseCompilationResult() != rcomp.GetName() || rc.GetGitCommitish() != "main" {
		t.Fatalf("UpdateReleaseConfig = %v, %v", rc, err)
	}
	if got, err := c.GetReleaseConfig(ctx, &dfpb.GetReleaseConfigRequest{Name: rc.GetName()}); err != nil || got.GetCronSchedule() != "0 3 * * *" {
		t.Errorf("GetReleaseConfig = %v, %v", got, err)
	}
	if it := c.ListReleaseConfigs(ctx, &dfpb.ListReleaseConfigsRequest{Parent: repo.GetName()}); true {
		if got, err := it.Next(); err != nil || got.GetName() != rc.GetName() {
			t.Errorf("ListReleaseConfigs = %v, %v", got, err)
		}
	}
	n := 0
	for it := c.ListCompilationResults(ctx, &dfpb.ListCompilationResultsRequest{Parent: repo.GetName()}); ; {
		if _, err := it.Next(); err != nil {
			break
		}
		n++
	}
	if n != 4 { // the workspace, the broken project, the prefixed one (#2094) and the release
		t.Errorf("ListCompilationResults listed %d, want 4", n)
	}
	wc, err := c.CreateWorkflowConfig(ctx, &dfpb.CreateWorkflowConfigRequest{Parent: repo.GetName(), WorkflowConfigId: "nightly",
		WorkflowConfig: &dfpb.WorkflowConfig{ReleaseConfig: rc.GetName(), CronSchedule: "0 4 * * *",
			InvocationConfig: &dfpb.InvocationConfig{IncludedTags: []string{"daily"}}}})
	if err != nil {
		t.Fatalf("CreateWorkflowConfig: %v", err)
	}
	if got, err := c.GetWorkflowConfig(ctx, &dfpb.GetWorkflowConfigRequest{Name: wc.GetName()}); err != nil || got.GetReleaseConfig() != rc.GetName() {
		t.Errorf("GetWorkflowConfig = %v, %v", got, err)
	}
	if got, err := c.UpdateWorkflowConfig(ctx, &dfpb.UpdateWorkflowConfigRequest{WorkflowConfig: &dfpb.WorkflowConfig{Name: wc.GetName(), CronSchedule: "0 5 * * *"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cron_schedule"}}}); err != nil || got.GetCronSchedule() != "0 5 * * *" {
		t.Errorf("UpdateWorkflowConfig = %v, %v", got, err)
	}
	if it := c.ListWorkflowConfigs(ctx, &dfpb.ListWorkflowConfigsRequest{Parent: repo.GetName()}); true {
		if got, err := it.Next(); err != nil || got.GetName() != wc.GetName() {
			t.Errorf("ListWorkflowConfigs = %v, %v", got, err)
		}
	}
	winv, err := c.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo.GetName(),
		WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_WorkflowConfig{WorkflowConfig: wc.GetName()}}})
	if err != nil {
		t.Fatalf("CreateWorkflowInvocation(workflow config): %v", err)
	}
	if got := waitInvocation(ctx, t, c, winv.GetName()); got.GetState() != dfpb.WorkflowInvocation_SUCCEEDED || got.GetResolvedCompilationResult() != rcomp.GetName() {
		t.Errorf("the workflow config's invocation = %v", got)
	}
	if acts := invocationActions(ctx, t, c, winv.GetName()); len(acts) != 1 || acts["x"].GetState() != dfpb.WorkflowInvocationAction_SUCCEEDED {
		t.Errorf("the tagged invocation's actions = %v", acts)
	}
	if err := c.DeleteReleaseConfig(ctx, &dfpb.DeleteReleaseConfigRequest{Name: rc.GetName()}); err == nil {
		t.Error("a release config a workflow config uses was deleted")
	}
	if err := c.DeleteWorkflowConfig(ctx, &dfpb.DeleteWorkflowConfigRequest{Name: wc.GetName()}); err != nil {
		t.Errorf("DeleteWorkflowConfig: %v", err)
	}
	if err := c.DeleteReleaseConfig(ctx, &dfpb.DeleteReleaseConfigRequest{Name: rc.GetName()}); err != nil {
		t.Errorf("DeleteReleaseConfig: %v", err)
	}
}

// covers: google.cloud.dataform.v1.Dataform/CommitWorkspaceChanges, google.cloud.dataform.v1.Dataform/ComputeRepositoryAccessTokenStatus, google.cloud.dataform.v1.Dataform/CreateRepository, google.cloud.dataform.v1.Dataform/CreateWorkspace, google.cloud.dataform.v1.Dataform/DeleteRepository, google.cloud.dataform.v1.Dataform/FetchRemoteBranches, google.cloud.dataform.v1.Dataform/FetchRepositoryHistory, google.cloud.dataform.v1.Dataform/PullGitCommits, google.cloud.dataform.v1.Dataform/PushGitCommits
//
// TestDataformSecureSourceManagerRemote (#2105): a repository whose git
// remote is a Secure Source Manager repository of this instance, with its
// access token in Secret Manager: a workspace commits and pushes to it,
// the remote's branches and the token's status read back; a remote
// anywhere else is refused.
func TestDataformSecureSourceManagerRemote(t *testing.T) {
	h := New(t)
	clients := dataformClients(t, h)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	sm := secretsClient(t, h)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	ssm, err := securesourcemanager.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ssm.Close() })
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "i-" + suffix})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r-" + suffix,
		Repository: &ssmpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository (Secure Source Manager): %v", err)
	}
	remote, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + h.Project(), SecretId: "df-token-" + suffix,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secret.GetName()})
	})
	version, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secret.GetName(), Payload: &secretmanagerpb.SecretPayload{Data: []byte("local-token")}})
	if err != nil {
		t.Fatal(err)
	}
	// gRPC first, then REST, not a map's random order: the two share one
	// remote, which is empty until the gRPC workspace pushes main, and the
	// REST workspace's PullGitCommits then pulls that branch. In the other
	// order the pull found no main ("couldn't find remote ref").
	for _, transport := range []string{"grpc", "rest"} {
		c := clients[transport]
		if c == nil {
			t.Fatalf("no %s Dataform client", transport)
		}
		t.Run(transport, func(t *testing.T) {
			dfParent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
			if _, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: dfParent, RepositoryId: "elsewhere" + transport,
				Repository: &dfpb.Repository{GitRemoteSettings: &dfpb.Repository_GitRemoteSettings{Url: "https://github.com/example/repo.git", DefaultBranch: "main",
					AuthenticationTokenSecretVersion: version.GetName()}}}); err == nil {
				t.Error("a repository with a remote outside the instance was created")
			}
			repo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: dfParent, RepositoryId: "remote" + transport + suffix,
				Repository: &dfpb.Repository{GitRemoteSettings: &dfpb.Repository_GitRemoteSettings{Url: remote.GetUris().GetGitHttps(), DefaultBranch: "main",
					AuthenticationTokenSecretVersion: version.GetName()}}})
			if err != nil {
				t.Fatalf("CreateRepository with a Secure Source Manager remote: %v", err)
			}
			t.Cleanup(func() {
				_ = c.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
			})
			if repo.GetGitRemoteSettings().GetEffectiveDefaultBranch() != "main" {
				t.Errorf("effective_default_branch = %q", repo.GetGitRemoteSettings().GetEffectiveDefaultBranch())
			}
			st, err := c.ComputeRepositoryAccessTokenStatus(ctx, &dfpb.ComputeRepositoryAccessTokenStatusRequest{Name: repo.GetName()})
			if err != nil || st.GetTokenStatus() != dfpb.ComputeRepositoryAccessTokenStatusResponse_VALID {
				t.Errorf("ComputeRepositoryAccessTokenStatus = %v, %v", st, err)
			}
			ws, err := c.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "dev"})
			if err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			dataformWrite(ctx, t, c, ws.GetName(), "definitions/"+transport+".sqlx", "config { type: \"view\" }\nselect 1 as x\n")
			author := &dfpb.CommitAuthor{Name: "Dev", EmailAddress: "dev@example.com"}
			if _, err := c.CommitWorkspaceChanges(ctx, &dfpb.CommitWorkspaceChangesRequest{Name: ws.GetName(), Author: author, CommitMessage: transport}); err != nil {
				t.Fatalf("CommitWorkspaceChanges: %v", err)
			}
			if _, err := c.PullGitCommits(ctx, &dfpb.PullGitCommitsRequest{Name: ws.GetName(), Author: author}); err != nil && transport == "rest" {
				t.Fatalf("PullGitCommits: %v", err)
			}
			if _, err := c.PushGitCommits(ctx, &dfpb.PushGitCommitsRequest{Name: ws.GetName(), RemoteBranch: "main"}); err != nil {
				t.Fatalf("PushGitCommits: %v", err)
			}
			if _, err := c.PushGitCommits(ctx, &dfpb.PushGitCommitsRequest{Name: ws.GetName(), RemoteBranch: "feature-" + transport}); err != nil {
				t.Fatalf("PushGitCommits to a new branch: %v", err)
			}
			br, err := c.FetchRemoteBranches(ctx, &dfpb.FetchRemoteBranchesRequest{Name: repo.GetName()})
			if err != nil || !strings.Contains(strings.Join(br.GetBranches(), ","), "feature-"+transport) || !strings.Contains(strings.Join(br.GetBranches(), ","), "main") {
				t.Errorf("FetchRemoteBranches = %v, %v", br, err)
			}
			if _, err := c.FetchRepositoryHistory(ctx, &dfpb.FetchRepositoryHistoryRequest{Name: repo.GetName()}).Next(); err == nil {
				t.Error("FetchRepositoryHistory answered for a repository with a git remote")
			}
		})
	}
}

// covers: google.cloud.dataform.v1.Dataform/CreateFolder, google.cloud.dataform.v1.Dataform/GetFolder, google.cloud.dataform.v1.Dataform/UpdateFolder, google.cloud.dataform.v1.Dataform/DeleteFolder, google.cloud.dataform.v1.Dataform/DeleteFolderTree, google.cloud.dataform.v1.Dataform/MoveFolder, google.cloud.dataform.v1.Dataform/QueryFolderContents, google.cloud.dataform.v1.Dataform/QueryUserRootContents, google.cloud.dataform.v1.Dataform/MoveRepository, google.cloud.dataform.v1.Dataform/CreateTeamFolder, google.cloud.dataform.v1.Dataform/GetTeamFolder, google.cloud.dataform.v1.Dataform/UpdateTeamFolder, google.cloud.dataform.v1.Dataform/DeleteTeamFolder, google.cloud.dataform.v1.Dataform/DeleteTeamFolderTree, google.cloud.dataform.v1.Dataform/QueryTeamFolderContents, google.cloud.dataform.v1.Dataform/SearchTeamFolders, google.cloud.dataform.v1.Dataform/GetConfig, google.cloud.dataform.v1.Dataform/UpdateConfig
//
// TestDataformFoldersAndConfig (#2105): folders and team folders hold
// folders and repositories, which move between them; their contents list
// folders first; a tree deletes with what is in it; the location's config
// reads and updates.
func TestDataformFoldersAndConfig(t *testing.T) {
	h := New(t)
	clients := dataformClients(t, h)
	ctx := h.Context()
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			loc := fmt.Sprintf("projects/%s/locations/europe-west1", h.Project())
			team, err := c.CreateTeamFolder(ctx, &dfpb.CreateTeamFolderRequest{Parent: loc, TeamFolder: &dfpb.TeamFolder{DisplayName: "Analytics " + transport}})
			if err != nil {
				t.Fatalf("CreateTeamFolder: %v", err)
			}
			if got, err := c.GetTeamFolder(ctx, &dfpb.GetTeamFolderRequest{Name: team.GetName()}); err != nil || got.GetDisplayName() != "Analytics "+transport {
				t.Errorf("GetTeamFolder = %v, %v", got, err)
			}
			if got, err := c.UpdateTeamFolder(ctx, &dfpb.UpdateTeamFolderRequest{TeamFolder: &dfpb.TeamFolder{Name: team.GetName(), DisplayName: "Data " + transport},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil || got.GetDisplayName() != "Data "+transport {
				t.Errorf("UpdateTeamFolder = %v, %v", got, err)
			}
			found := false
			for it := c.SearchTeamFolders(ctx, &dfpb.SearchTeamFoldersRequest{Location: loc, Filter: `display_name="Data ` + transport + `"`}); ; {
				r, err := it.Next()
				if err != nil {
					break
				}
				found = found || r.GetTeamFolder().GetName() == team.GetName()
			}
			if !found {
				t.Error("SearchTeamFolders does not find the team folder")
			}
			f, err := c.CreateFolder(ctx, &dfpb.CreateFolderRequest{Parent: loc, Folder: &dfpb.Folder{DisplayName: "reports", ContainingFolder: team.GetName()}})
			if err != nil || f.GetTeamFolderName() != team.GetName() {
				t.Fatalf("CreateFolder = %v, %v", f, err)
			}
			if got, err := c.UpdateFolder(ctx, &dfpb.UpdateFolderRequest{Folder: &dfpb.Folder{Name: f.GetName(), DisplayName: "dashboards"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil || got.GetDisplayName() != "dashboards" {
				t.Errorf("UpdateFolder = %v, %v", got, err)
			}
			repo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: loc, RepositoryId: "folder-" + transport + strings.ReplaceAll(uuid.NewString(), "-", "")[:6]})
			if err != nil {
				t.Fatal(err)
			}
			mop, err := c.MoveRepository(ctx, &dfpb.MoveRepositoryRequest{Name: repo.GetName(), DestinationContainingFolder: proto.String(f.GetName())})
			if err != nil {
				t.Fatalf("MoveRepository: %v", err)
			}
			if err := mop.Wait(ctx); err != nil {
				t.Fatalf("MoveRepository: %v", err)
			}
			if got, _ := c.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: repo.GetName()}); got.GetContainingFolder() != f.GetName() || got.GetTeamFolderName() != team.GetName() {
				t.Errorf("the moved repository = %v", got)
			}
			sub, err := c.CreateFolder(ctx, &dfpb.CreateFolderRequest{Parent: loc, Folder: &dfpb.Folder{DisplayName: "archive"}})
			if err != nil {
				t.Fatal(err)
			}
			var root []string
			for it := c.QueryUserRootContents(ctx, &dfpb.QueryUserRootContentsRequest{Location: loc}); ; {
				e, err := it.Next()
				if err != nil {
					break
				}
				root = append(root, e.GetFolder().GetName())
			}
			if !strings.Contains(strings.Join(root, ","), sub.GetName()) {
				t.Errorf("QueryUserRootContents = %v", root)
			}
			fop, err := c.MoveFolder(ctx, &dfpb.MoveFolderRequest{Name: sub.GetName(), DestinationContainingFolder: proto.String(f.GetName())})
			if err != nil {
				t.Fatalf("MoveFolder: %v", err)
			}
			if err := fop.Wait(ctx); err != nil {
				t.Fatalf("MoveFolder: %v", err)
			}
			if got, err := c.GetFolder(ctx, &dfpb.GetFolderRequest{Name: sub.GetName()}); err != nil || got.GetContainingFolder() != f.GetName() || got.GetTeamFolderName() != team.GetName() {
				t.Errorf("GetFolder of the moved folder = %v, %v", got, err)
			}
			var entries []string
			for it := c.QueryFolderContents(ctx, &dfpb.QueryFolderContentsRequest{Folder: f.GetName()}); ; {
				e, err := it.Next()
				if err != nil {
					break
				}
				entries = append(entries, e.GetFolder().GetName()+e.GetRepository().GetName())
			}
			if strings.Join(entries, ",") != sub.GetName()+","+repo.GetName() {
				t.Errorf("QueryFolderContents = %v, want the folder then the repository", entries)
			}
			var team1 []string
			for it := c.QueryTeamFolderContents(ctx, &dfpb.QueryTeamFolderContentsRequest{TeamFolder: team.GetName()}); ; {
				e, err := it.Next()
				if err != nil {
					break
				}
				team1 = append(team1, e.GetFolder().GetName())
			}
			if strings.Join(team1, ",") != f.GetName() {
				t.Errorf("QueryTeamFolderContents = %v", team1)
			}
			if err := c.DeleteFolder(ctx, &dfpb.DeleteFolderRequest{Name: f.GetName()}); err == nil {
				t.Error("a folder that is not empty was deleted")
			}
			if err := c.DeleteTeamFolder(ctx, &dfpb.DeleteTeamFolderRequest{Name: team.GetName()}); err == nil {
				t.Error("a team folder that is not empty was deleted")
			}
			dop, err := c.DeleteFolderTree(ctx, &dfpb.DeleteFolderTreeRequest{Name: f.GetName()})
			if err != nil {
				t.Fatalf("DeleteFolderTree: %v", err)
			}
			if err := dop.Wait(ctx); err != nil {
				t.Fatalf("DeleteFolderTree: %v", err)
			}
			if _, err := c.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: repo.GetName()}); err == nil {
				t.Error("the folder tree's repository is still there")
			}
			empty, err := c.CreateFolder(ctx, &dfpb.CreateFolderRequest{Parent: loc, Folder: &dfpb.Folder{DisplayName: "empty", ContainingFolder: team.GetName()}})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.DeleteFolder(ctx, &dfpb.DeleteFolderRequest{Name: empty.GetName()}); err != nil {
				t.Errorf("DeleteFolder: %v", err)
			}
			tf2, err := c.CreateTeamFolder(ctx, &dfpb.CreateTeamFolderRequest{Parent: loc, TeamFolder: &dfpb.TeamFolder{DisplayName: "Scratch"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.CreateFolder(ctx, &dfpb.CreateFolderRequest{Parent: loc, Folder: &dfpb.Folder{DisplayName: "in", ContainingFolder: tf2.GetName()}}); err != nil {
				t.Fatal(err)
			}
			top, err := c.DeleteTeamFolderTree(ctx, &dfpb.DeleteTeamFolderTreeRequest{Name: tf2.GetName(), Force: true})
			if err != nil {
				t.Fatalf("DeleteTeamFolderTree: %v", err)
			}
			if err := top.Wait(ctx); err != nil {
				t.Fatalf("DeleteTeamFolderTree: %v", err)
			}
			if err := c.DeleteTeamFolder(ctx, &dfpb.DeleteTeamFolderRequest{Name: team.GetName()}); err != nil {
				t.Errorf("DeleteTeamFolder: %v", err)
			}
			cfg, err := c.GetConfig(ctx, &dfpb.GetConfigRequest{Name: loc + "/config"})
			if err != nil || cfg.GetName() != loc+"/config" {
				t.Errorf("GetConfig = %v, %v", cfg, err)
			}
			if got, err := c.UpdateConfig(ctx, &dfpb.UpdateConfigRequest{Config: &dfpb.Config{Name: loc + "/config"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_kms_key_name"}}}); err != nil || got.GetDefaultKmsKeyName() != "" {
				t.Errorf("UpdateConfig = %v, %v", got, err)
			}
			// The long-running compile of a repository with no commits fails
			// as the plain one does.
			if _, err := c.CreateCompilationResultLongRunning(ctx, &dfpb.CreateCompilationResultRequest{Parent: loc + "/repositories/none",
				CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_GitCommitish{GitCommitish: "main"}}}); err == nil {
				t.Error("CreateCompilationResultLongRunning of a missing repository succeeded")
			}
		})
	}
}
