//go:build compat

package compat

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	dataformb1 "cloud.google.com/go/dataform/apiv1beta1"
	b1pb "cloud.google.com/go/dataform/apiv1beta1/dataformpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// dataformBetaClients are the official v1beta1 clients, gRPC and REST, on
// the instance's Dataform port.
func dataformBetaClients(t *testing.T, h *Harness) map[string]*dataformb1.Client {
	t.Helper()
	addr := h.Endpoint(EnvDataform)
	ctx := h.Context()
	g, err := dataformb1.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err := dataformb1.NewRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return map[string]*dataformb1.Client{"grpc": g, "rest": r}
}

func betaCode(err error) codes.Code { return quotaCode(err) }

func betaBranches(ctx context.Context, t *testing.T, c *dataformb1.Client, ws string, f b1pb.FetchWorkspaceBranchesRequest_BranchFilter) []string {
	t.Helper()
	var out []string
	for it := c.FetchWorkspaceBranches(ctx, &b1pb.FetchWorkspaceBranchesRequest{Name: ws, Filter: f}); ; {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("FetchWorkspaceBranches: %v", err)
		}
		if b.GetLastCommit().GetCommitSha() == "" {
			t.Errorf("branch %s has no last commit", b.GetBranchName())
		}
		out = append(out, b.GetBranchName())
	}
	sort.Strings(out)
	return out
}

func betaHistory(ctx context.Context, t *testing.T, c *dataformb1.Client, ws string) ([]string, bool) {
	t.Helper()
	var msgs []string
	it := c.FetchWorkspaceHistory(ctx, &b1pb.FetchWorkspaceHistoryRequest{Name: ws})
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("FetchWorkspaceHistory: %v", err)
		}
		msgs = append(msgs, e.GetCommitMessage())
	}
	resp, _ := it.Response.(*b1pb.FetchWorkspaceHistoryResponse)
	return msgs, resp.GetReachesShallowBoundary()
}

// covers: google.cloud.dataform.v1beta1.Dataform/CreateRepository, google.cloud.dataform.v1beta1.Dataform/GetRepository, google.cloud.dataform.v1beta1.Dataform/ListRepositories, google.cloud.dataform.v1beta1.Dataform/UpdateRepository, google.cloud.dataform.v1beta1.Dataform/DeleteRepository, google.cloud.dataform.v1beta1.Dataform/DeleteRepositoryLongRunning, google.cloud.dataform.v1beta1.Dataform/CreateWorkspace, google.cloud.dataform.v1beta1.Dataform/GetWorkspace, google.cloud.dataform.v1beta1.Dataform/ListWorkspaces, google.cloud.dataform.v1beta1.Dataform/DeleteWorkspace, google.cloud.dataform.v1beta1.Dataform/WriteFile, google.cloud.dataform.v1beta1.Dataform/ReadFile, google.cloud.dataform.v1beta1.Dataform/RemoveFile, google.cloud.dataform.v1beta1.Dataform/MoveFile, google.cloud.dataform.v1beta1.Dataform/MakeDirectory, google.cloud.dataform.v1beta1.Dataform/RemoveDirectory, google.cloud.dataform.v1beta1.Dataform/MoveDirectory, google.cloud.dataform.v1beta1.Dataform/QueryDirectoryContents, google.cloud.dataform.v1beta1.Dataform/SearchFiles, google.cloud.dataform.v1beta1.Dataform/FetchFileGitStatuses, google.cloud.dataform.v1beta1.Dataform/FetchFileDiff, google.cloud.dataform.v1beta1.Dataform/CommitWorkspaceChanges, google.cloud.dataform.v1beta1.Dataform/ResetWorkspaceChanges, google.cloud.dataform.v1beta1.Dataform/PushGitCommits, google.cloud.dataform.v1beta1.Dataform/PullGitCommits, google.cloud.dataform.v1beta1.Dataform/FetchGitAheadBehind, google.cloud.dataform.v1beta1.Dataform/FetchRepositoryHistory, google.cloud.dataform.v1beta1.Dataform/ReadRepositoryFile, google.cloud.dataform.v1beta1.Dataform/QueryRepositoryDirectoryContents, google.cloud.dataform.v1beta1.Dataform/CommitRepositoryChanges, google.cloud.dataform.v1beta1.Dataform/CheckoutWorkspaceBranch, google.cloud.dataform.v1beta1.Dataform/FetchCurrentWorkspaceBranch, google.cloud.dataform.v1beta1.Dataform/FetchWorkspaceBranches, google.cloud.dataform.v1beta1.Dataform/DeleteBranch, google.cloud.dataform.v1beta1.Dataform/SyncWorkspaceRefs, google.cloud.dataform.v1beta1.Dataform/FetchWorkspaceHistory, google.cloud.dataform.v1beta1.Dataform/GetIamPolicy, google.cloud.dataform.v1beta1.Dataform/SetIamPolicy, google.cloud.dataform.v1beta1.Dataform/TestIamPermissions
//
// TestDataformV1beta1WorkspacesAndBranches (#2105): the official v1beta1
// client, over gRPC and REST, on a repository without a git remote: a
// workspace with enable_branch_management whose files and directories are
// written, moved, searched and removed, its changes diffed, reset,
// committed and pushed; a branch created from origin/main, committed on
// and pushed to by default, listed with the remote's, its history read,
// refused deletion while checked out and while unmerged, then forced; a
// second workspace, a shallow clone (depth 1) of original_branch feature,
// whose history reaches the shallow boundary until SyncWorkspaceRefs
// deepens it, pulls; the repository's own history, files and a direct
// commit; IAM; account_connector refused; and DeleteRepositoryLongRunning,
// refused while workspaces exist, as an operation.
func TestDataformV1beta1WorkspacesAndBranches(t *testing.T) {
	h := New(t)
	clients := dataformBetaClients(t, h)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	ctx := h.Context()
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			parent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
			id := "b" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
			repo, err := c.CreateRepository(ctx, &b1pb.CreateRepositoryRequest{Parent: parent, RepositoryId: id,
				Repository: &b1pb.Repository{DisplayName: "beta", Labels: map[string]string{"team": "data"}}})
			if err != nil {
				t.Fatalf("CreateRepository: %v", err)
			}
			t.Cleanup(func() {
				_ = c.DeleteRepository(context.Background(), &b1pb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
			})
			if _, err := c.CreateRepository(ctx, &b1pb.CreateRepositoryRequest{Parent: parent, RepositoryId: id + "x", Repository: &b1pb.Repository{
				GitRemoteSettings: &b1pb.Repository_GitRemoteSettings{Url: "https://example.com/r.git", DefaultBranch: "main",
					AccountConnector: proto.String("projects/p/locations/us-central1/accountConnectors/a")}}}); betaCode(err) != codes.Unimplemented {
				t.Errorf("CreateRepository with account_connector: %v, want UNIMPLEMENTED", err)
			}
			if got, err := c.GetRepository(ctx, &b1pb.GetRepositoryRequest{Name: repo.GetName()}); err != nil || got.GetDisplayName() != "beta" {
				t.Errorf("GetRepository = %v, %v", got, err)
			}
			if up, err := c.UpdateRepository(ctx, &b1pb.UpdateRepositoryRequest{Repository: &b1pb.Repository{Name: repo.GetName(), DisplayName: "beta 2"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil || up.GetDisplayName() != "beta 2" || up.GetLabels()["team"] != "data" {
				t.Errorf("UpdateRepository = %v, %v", up, err)
			}
			listed := false
			for it := c.ListRepositories(ctx, &b1pb.ListRepositoriesRequest{Parent: parent}); ; {
				r, err := it.Next()
				if err != nil {
					break
				}
				listed = listed || r.GetName() == repo.GetName()
			}
			if !listed {
				t.Error("ListRepositories does not list the repository")
			}

			if _, err := c.CreateWorkspace(ctx, &b1pb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "bad", Workspace: &b1pb.Workspace{Depth: 1}}); betaCode(err) != codes.InvalidArgument {
				t.Errorf("CreateWorkspace with depth and no branch management: %v, want INVALID_ARGUMENT", err)
			}
			ws, err := c.CreateWorkspace(ctx, &b1pb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "dev",
				Workspace: &b1pb.Workspace{EnableBranchManagement: proto.Bool(true)}})
			if err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			if !ws.GetEnableBranchManagement() || ws.GetShallow() {
				t.Errorf("CreateWorkspace = %v", ws)
			}
			if got, err := c.GetWorkspace(ctx, &b1pb.GetWorkspaceRequest{Name: ws.GetName()}); err != nil || !got.GetEnableBranchManagement() {
				t.Errorf("GetWorkspace = %v, %v", got, err)
			}
			w := ws.GetName()
			write := func(path, contents string) {
				t.Helper()
				if _, err := c.WriteFile(ctx, &b1pb.WriteFileRequest{Workspace: w, Path: path, Contents: []byte(contents)}); err != nil {
					t.Fatalf("WriteFile %s: %v", path, err)
				}
			}
			author := &b1pb.CommitAuthor{Name: "Ada", EmailAddress: "ada@example.com"}
			write("definitions/a.sqlx", "select 1 as a\n")
			if _, err := c.MakeDirectory(ctx, &b1pb.MakeDirectoryRequest{Workspace: w, Path: "includes"}); err != nil {
				t.Fatalf("MakeDirectory: %v", err)
			}
			write("includes/c.js", "module.exports = {};\n")
			if _, err := c.MoveDirectory(ctx, &b1pb.MoveDirectoryRequest{Workspace: w, Path: "includes", NewPath: "lib"}); err != nil {
				t.Fatalf("MoveDirectory: %v", err)
			}
			if _, err := c.MoveFile(ctx, &b1pb.MoveFileRequest{Workspace: w, Path: "lib/c.js", NewPath: "lib/d.js"}); err != nil {
				t.Fatalf("MoveFile: %v", err)
			}
			var names []string
			for it := c.QueryDirectoryContents(ctx, &b1pb.QueryDirectoryContentsRequest{Workspace: w}); ; {
				e, err := it.Next()
				if err != nil {
					break
				}
				names = append(names, e.GetFile()+e.GetDirectory())
			}
			if strings.Join(names, ",") != "definitions,lib" {
				t.Errorf("QueryDirectoryContents = %v", names)
			}
			found := false
			for it := c.SearchFiles(ctx, &b1pb.SearchFilesRequest{Workspace: w, Filter: `path:"d.js"`}); ; {
				r, err := it.Next()
				if err != nil {
					break
				}
				found = found || r.GetFile().GetPath() == "lib/d.js"
			}
			if !found {
				t.Error("SearchFiles does not find lib/d.js")
			}
			if _, err := c.RemoveFile(ctx, &b1pb.RemoveFileRequest{Workspace: w, Path: "lib/d.js"}); err != nil {
				t.Fatalf("RemoveFile: %v", err)
			}
			if _, err := c.RemoveDirectory(ctx, &b1pb.RemoveDirectoryRequest{Workspace: w, Path: "lib"}); err != nil {
				t.Fatalf("RemoveDirectory: %v", err)
			}
			write("scratch.txt", "draft\n")
			st, err := c.FetchFileGitStatuses(ctx, &b1pb.FetchFileGitStatusesRequest{Name: w})
			if err != nil || len(st.GetUncommittedFileChanges()) != 2 {
				t.Errorf("FetchFileGitStatuses = %v, %v", st, err)
			}
			if d, err := c.FetchFileDiff(ctx, &b1pb.FetchFileDiffRequest{Workspace: w, Path: "scratch.txt"}); err != nil || !strings.Contains(d.GetFormattedDiff(), "+draft") {
				t.Errorf("FetchFileDiff = %v, %v", d, err)
			}
			if _, err := c.ResetWorkspaceChanges(ctx, &b1pb.ResetWorkspaceChangesRequest{Name: w, Paths: []string{"scratch.txt"}, Clean: true}); err != nil {
				t.Fatalf("ResetWorkspaceChanges: %v", err)
			}
			if _, err := c.CommitWorkspaceChanges(ctx, &b1pb.CommitWorkspaceChangesRequest{Name: w, Author: author, CommitMessage: "one"}); err != nil {
				t.Fatalf("CommitWorkspaceChanges: %v", err)
			}
			write("definitions/b.sqlx", "select 2 as b\n")
			if _, err := c.CommitWorkspaceChanges(ctx, &b1pb.CommitWorkspaceChangesRequest{Name: w, Author: author, CommitMessage: "two"}); err != nil {
				t.Fatalf("CommitWorkspaceChanges: %v", err)
			}
			if ab, err := c.FetchGitAheadBehind(ctx, &b1pb.FetchGitAheadBehindRequest{Name: w}); err != nil || ab.GetCommitsAhead() != 2 {
				t.Errorf("FetchGitAheadBehind before the push = %v, %v", ab, err)
			}
			if _, err := c.PushGitCommits(ctx, &b1pb.PushGitCommitsRequest{Name: w}); err != nil {
				t.Fatalf("PushGitCommits: %v", err)
			}
			if cur, err := c.FetchCurrentWorkspaceBranch(ctx, &b1pb.FetchCurrentWorkspaceBranchRequest{Name: w}); err != nil || cur.GetBranchName() != "main" {
				t.Errorf("FetchCurrentWorkspaceBranch = %v, %v", cur, err)
			}

			// Branches.
			if err := c.CheckoutWorkspaceBranch(ctx, &b1pb.CheckoutWorkspaceBranchRequest{Name: w, Branch: "feature"}); betaCode(err) != codes.NotFound {
				t.Errorf("CheckoutWorkspaceBranch of a missing branch: %v, want NOT_FOUND", err)
			}
			if err := c.CheckoutWorkspaceBranch(ctx, &b1pb.CheckoutWorkspaceBranchRequest{Name: w, Branch: "feature", SourceBranch: "main"}); betaCode(err) != codes.InvalidArgument {
				t.Errorf("CheckoutWorkspaceBranch with source_branch and no create_if_not_exists: %v, want INVALID_ARGUMENT", err)
			}
			if err := c.CheckoutWorkspaceBranch(ctx, &b1pb.CheckoutWorkspaceBranchRequest{Name: w, Branch: "feature", CreateIfNotExists: true, SourceBranch: "origin/main"}); err != nil {
				t.Fatalf("CheckoutWorkspaceBranch: %v", err)
			}
			write("definitions/c.sqlx", "select 3 as c\n")
			if _, err := c.CommitWorkspaceChanges(ctx, &b1pb.CommitWorkspaceChangesRequest{Name: w, Author: author, CommitMessage: "three"}); err != nil {
				t.Fatalf("CommitWorkspaceChanges: %v", err)
			}
			// A push names no branch: the checked-out one is pushed.
			if _, err := c.PushGitCommits(ctx, &b1pb.PushGitCommitsRequest{Name: w}); err != nil {
				t.Fatalf("PushGitCommits of feature: %v", err)
			}
			if got := betaBranches(ctx, t, c, w, b1pb.FetchWorkspaceBranchesRequest_ALL); strings.Join(got, ",") != "feature,main,origin/feature,origin/main" {
				t.Errorf("FetchWorkspaceBranches(ALL) = %v", got)
			}
			if got := betaBranches(ctx, t, c, w, b1pb.FetchWorkspaceBranchesRequest_REMOTE_ONLY); strings.Join(got, ",") != "origin/feature,origin/main" {
				t.Errorf("FetchWorkspaceBranches(REMOTE_ONLY) = %v", got)
			}
			if msgs, edge := betaHistory(ctx, t, c, w); strings.Join(msgs, ",") != "three,two,one" || edge {
				t.Errorf("FetchWorkspaceHistory = %v (boundary %v)", msgs, edge)
			}
			if _, err := c.DeleteBranch(ctx, &b1pb.DeleteBranchRequest{Name: w, Branch: "feature"}); betaCode(err) != codes.FailedPrecondition {
				t.Errorf("DeleteBranch of the checked-out branch: %v, want FAILED_PRECONDITION", err)
			}
			if err := c.CheckoutWorkspaceBranch(ctx, &b1pb.CheckoutWorkspaceBranchRequest{Name: w, Branch: "main"}); err != nil {
				t.Fatalf("CheckoutWorkspaceBranch main: %v", err)
			}
			if _, err := c.DeleteBranch(ctx, &b1pb.DeleteBranchRequest{Name: w, Branch: "feature"}); betaCode(err) != codes.FailedPrecondition {
				t.Errorf("DeleteBranch of an unmerged branch: %v, want FAILED_PRECONDITION", err)
			}
			if _, err := c.DeleteBranch(ctx, &b1pb.DeleteBranchRequest{Name: w, Branch: "feature", Force: true}); err != nil {
				t.Errorf("DeleteBranch with force: %v", err)
			}
			if got := betaBranches(ctx, t, c, w, b1pb.FetchWorkspaceBranchesRequest_LOCAL_ONLY); strings.Join(got, ",") != "main" {
				t.Errorf("FetchWorkspaceBranches(LOCAL_ONLY) after the delete = %v", got)
			}

			// A shallow clone of feature.
			sh, err := c.CreateWorkspace(ctx, &b1pb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "shallow",
				Workspace: &b1pb.Workspace{EnableBranchManagement: proto.Bool(true), Depth: 1, OriginalBranch: proto.String("feature")}})
			if err != nil {
				t.Fatalf("CreateWorkspace(depth 1): %v", err)
			}
			if !sh.GetShallow() {
				t.Errorf("the shallow workspace = %v", sh)
			}
			if msgs, edge := betaHistory(ctx, t, c, sh.GetName()); strings.Join(msgs, ",") != "three" || !edge {
				t.Errorf("FetchWorkspaceHistory of the shallow clone = %v (boundary %v)", msgs, edge)
			}
			if _, err := c.SyncWorkspaceRefs(ctx, &b1pb.SyncWorkspaceRefsRequest{Name: sh.GetName(), RemoteBranchName: "feature", Deepen: 5}); err != nil {
				t.Fatalf("SyncWorkspaceRefs: %v", err)
			}
			if msgs, _ := betaHistory(ctx, t, c, sh.GetName()); strings.Join(msgs, ",") != "three,two,one" {
				t.Errorf("FetchWorkspaceHistory after deepening = %v", msgs)
			}
			if cur, _ := c.FetchCurrentWorkspaceBranch(ctx, &b1pb.FetchCurrentWorkspaceBranchRequest{Name: sh.GetName()}); cur.GetBranchName() != "feature" {
				t.Errorf("the shallow workspace's branch = %v, want original_branch feature", cur)
			}
			// main moves on; the first workspace pulls feature into main.
			if _, err := c.PullGitCommits(ctx, &b1pb.PullGitCommitsRequest{Name: w, RemoteBranch: "feature", Author: author}); err != nil {
				t.Fatalf("PullGitCommits: %v", err)
			}
			if f, err := c.ReadFile(ctx, &b1pb.ReadFileRequest{Workspace: w, Path: "definitions/c.sqlx"}); err != nil || string(f.GetFileContents()) != "select 3 as c\n" {
				t.Errorf("ReadFile after the pull = %v, %v", f, err)
			}
			var listedWs []string
			for it := c.ListWorkspaces(ctx, &b1pb.ListWorkspacesRequest{Parent: repo.GetName()}); ; {
				x, err := it.Next()
				if err != nil {
					break
				}
				listedWs = append(listedWs, fmt.Sprintf("%s:%v", x.GetName()[strings.LastIndex(x.GetName(), "/")+1:], x.GetShallow()))
			}
			if strings.Join(listedWs, ",") != "dev:false,shallow:true" {
				t.Errorf("ListWorkspaces = %v", listedWs)
			}

			// The repository's own git.
			var hist []string
			for it := c.FetchRepositoryHistory(ctx, &b1pb.FetchRepositoryHistoryRequest{Name: repo.GetName()}); ; {
				e, err := it.Next()
				if err != nil {
					break
				}
				hist = append(hist, e.GetCommitMessage())
			}
			if strings.Join(hist, ",") != "two,one" {
				t.Errorf("FetchRepositoryHistory = %v", hist)
			}
			if f, err := c.ReadRepositoryFile(ctx, &b1pb.ReadRepositoryFileRequest{Name: repo.GetName(), Path: "definitions/a.sqlx"}); err != nil || string(f.GetContents()) != "select 1 as a\n" {
				t.Errorf("ReadRepositoryFile = %v, %v", f, err)
			}
			var top []string
			for it := c.QueryRepositoryDirectoryContents(ctx, &b1pb.QueryRepositoryDirectoryContentsRequest{Name: repo.GetName()}); ; {
				e, err := it.Next()
				if err != nil {
					break
				}
				top = append(top, e.GetFile()+e.GetDirectory())
			}
			if strings.Join(top, ",") != "definitions" {
				t.Errorf("QueryRepositoryDirectoryContents = %v", top)
			}
			if r, err := c.CommitRepositoryChanges(ctx, &b1pb.CommitRepositoryChangesRequest{Name: repo.GetName(),
				CommitMetadata: &b1pb.CommitMetadata{Author: author, CommitMessage: "direct"},
				FileOperations: map[string]*b1pb.CommitRepositoryChangesRequest_FileOperation{"README.md": {Operation: &b1pb.CommitRepositoryChangesRequest_FileOperation_WriteFile_{
					WriteFile: &b1pb.CommitRepositoryChangesRequest_FileOperation_WriteFile{Contents: []byte("# hi\n")}}}}}); err != nil || len(r.GetCommitSha()) != 40 {
				t.Errorf("CommitRepositoryChanges = %v, %v", r, err)
			}

			// IAM on the workspace.
			pol, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: w, Policy: &iampb.Policy{Bindings: []*iampb.Binding{{Role: "roles/dataform.editor", Members: []string{"user:ada@example.com"}}}}})
			if err != nil {
				t.Fatalf("SetIamPolicy: %v", err)
			}
			if got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: w}); err != nil || len(got.GetBindings()) != 1 || string(got.GetEtag()) != string(pol.GetEtag()) {
				t.Errorf("GetIamPolicy = %v, %v", got, err)
			}
			if got, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: w, Permissions: []string{"dataform.workspaces.get"}}); err != nil || len(got.GetPermissions()) != 1 {
				t.Errorf("TestIamPermissions = %v, %v", got, err)
			}

			// DeleteRepositoryLongRunning.
			if _, err := c.DeleteRepositoryLongRunning(ctx, &b1pb.DeleteRepositoryLongRunningRequest{Name: repo.GetName(), Force: true}); betaCode(err) != codes.FailedPrecondition {
				t.Errorf("DeleteRepositoryLongRunning with workspaces: %v, want FAILED_PRECONDITION", err)
			}
			for _, x := range []string{w, sh.GetName()} {
				if err := c.DeleteWorkspace(ctx, &b1pb.DeleteWorkspaceRequest{Name: x}); err != nil {
					t.Fatalf("DeleteWorkspace: %v", err)
				}
			}
			op, err := c.DeleteRepositoryLongRunning(ctx, &b1pb.DeleteRepositoryLongRunningRequest{Name: repo.GetName()})
			if err != nil {
				t.Fatalf("DeleteRepositoryLongRunning: %v", err)
			}
			if _, err := op.Wait(ctx); err != nil {
				t.Fatalf("DeleteRepositoryLongRunning: %v", err)
			}
			if md, err := op.Metadata(); err != nil || md.GetState() != b1pb.DeleteRepositoryLongRunningMetadata_SUCCEEDED || md.GetTarget() != repo.GetName() {
				t.Errorf("DeleteRepositoryLongRunning metadata = %v, %v", md, err)
			}
			if _, err := c.GetRepository(ctx, &b1pb.GetRepositoryRequest{Name: repo.GetName()}); betaCode(err) != codes.NotFound {
				t.Errorf("GetRepository after the delete: %v, want NOT_FOUND", err)
			}
			// DeleteRepository too.
			r2, err := c.CreateRepository(ctx, &b1pb.CreateRepositoryRequest{Parent: parent, RepositoryId: id + "d", Repository: &b1pb.Repository{}})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.DeleteRepository(ctx, &b1pb.DeleteRepositoryRequest{Name: r2.GetName()}); err != nil {
				t.Errorf("DeleteRepository: %v", err)
			}
		})
	}
}

// covers: google.cloud.dataform.v1beta1.Dataform/CreateFolder, google.cloud.dataform.v1beta1.Dataform/GetFolder, google.cloud.dataform.v1beta1.Dataform/UpdateFolder, google.cloud.dataform.v1beta1.Dataform/DeleteFolder, google.cloud.dataform.v1beta1.Dataform/DeleteFolderTree, google.cloud.dataform.v1beta1.Dataform/MoveFolder, google.cloud.dataform.v1beta1.Dataform/QueryFolderContents, google.cloud.dataform.v1beta1.Dataform/QueryUserRootContents, google.cloud.dataform.v1beta1.Dataform/MoveRepository, google.cloud.dataform.v1beta1.Dataform/CreateTeamFolder, google.cloud.dataform.v1beta1.Dataform/GetTeamFolder, google.cloud.dataform.v1beta1.Dataform/UpdateTeamFolder, google.cloud.dataform.v1beta1.Dataform/DeleteTeamFolder, google.cloud.dataform.v1beta1.Dataform/DeleteTeamFolderTree, google.cloud.dataform.v1beta1.Dataform/QueryTeamFolderContents, google.cloud.dataform.v1beta1.Dataform/SearchTeamFolders, google.cloud.dataform.v1beta1.Dataform/GetConfig, google.cloud.dataform.v1beta1.Dataform/UpdateConfig
//
// TestDataformV1beta1FoldersAndConfig (#2105): v1beta1's folders and team
// folders, over gRPC and REST, with the folder_id and team_folder_id a
// create may choose (a duplicate ALREADY_EXISTS): a repository and a folder
// moved in (operations with v1beta1 metadata), contents listed, non-empty
// deletes refused, tree deletes; and the location's config.
func TestDataformV1beta1FoldersAndConfig(t *testing.T) {
	h := New(t)
	clients := dataformBetaClients(t, h)
	ctx := h.Context()
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			loc := fmt.Sprintf("projects/%s/locations/asia-northeast1", h.Project())
			sfx := transport + strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
			team, err := c.CreateTeamFolder(ctx, &b1pb.CreateTeamFolderRequest{Parent: loc, TeamFolderId: "team-" + sfx, TeamFolder: &b1pb.TeamFolder{DisplayName: "Team " + sfx}})
			if err != nil || team.GetName() != loc+"/teamFolders/team-"+sfx {
				t.Fatalf("CreateTeamFolder = %v, %v", team, err)
			}
			if _, err := c.CreateTeamFolder(ctx, &b1pb.CreateTeamFolderRequest{Parent: loc, TeamFolderId: "team-" + sfx, TeamFolder: &b1pb.TeamFolder{DisplayName: "again"}}); betaCode(err) != codes.AlreadyExists {
				t.Errorf("a duplicate team_folder_id: %v, want ALREADY_EXISTS", err)
			}
			if got, err := c.GetTeamFolder(ctx, &b1pb.GetTeamFolderRequest{Name: team.GetName()}); err != nil || got.GetDisplayName() != "Team "+sfx {
				t.Errorf("GetTeamFolder = %v, %v", got, err)
			}
			if got, err := c.UpdateTeamFolder(ctx, &b1pb.UpdateTeamFolderRequest{TeamFolder: &b1pb.TeamFolder{Name: team.GetName(), DisplayName: "Data " + sfx},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil || got.GetDisplayName() != "Data "+sfx {
				t.Errorf("UpdateTeamFolder = %v, %v", got, err)
			}
			found := false
			for it := c.SearchTeamFolders(ctx, &b1pb.SearchTeamFoldersRequest{Location: loc, Filter: `display_name="Data ` + sfx + `"`}); ; {
				r, err := it.Next()
				if err != nil {
					break
				}
				found = found || r.GetTeamFolder().GetName() == team.GetName()
			}
			if !found {
				t.Error("SearchTeamFolders does not find the team folder")
			}
			f, err := c.CreateFolder(ctx, &b1pb.CreateFolderRequest{Parent: loc, FolderId: "reports-" + sfx, Folder: &b1pb.Folder{DisplayName: "reports", ContainingFolder: team.GetName()}})
			if err != nil || f.GetName() != loc+"/folders/reports-"+sfx || f.GetTeamFolderName() != team.GetName() {
				t.Fatalf("CreateFolder = %v, %v", f, err)
			}
			if _, err := c.CreateFolder(ctx, &b1pb.CreateFolderRequest{Parent: loc, FolderId: "reports-" + sfx, Folder: &b1pb.Folder{DisplayName: "again"}}); betaCode(err) != codes.AlreadyExists {
				t.Errorf("a duplicate folder_id: %v, want ALREADY_EXISTS", err)
			}
			if got, err := c.UpdateFolder(ctx, &b1pb.UpdateFolderRequest{Folder: &b1pb.Folder{Name: f.GetName(), DisplayName: "dashboards"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil || got.GetDisplayName() != "dashboards" {
				t.Errorf("UpdateFolder = %v, %v", got, err)
			}
			repo, err := c.CreateRepository(ctx, &b1pb.CreateRepositoryRequest{Parent: loc, RepositoryId: "f" + sfx, Repository: &b1pb.Repository{}})
			if err != nil {
				t.Fatal(err)
			}
			mop, err := c.MoveRepository(ctx, &b1pb.MoveRepositoryRequest{Name: repo.GetName(), DestinationContainingFolder: proto.String(f.GetName())})
			if err != nil {
				t.Fatalf("MoveRepository: %v", err)
			}
			if err := mop.Wait(ctx); err != nil {
				t.Fatalf("MoveRepository: %v", err)
			}
			if md, err := mop.Metadata(); err != nil || md.GetState() != b1pb.MoveRepositoryMetadata_SUCCESS {
				t.Errorf("MoveRepository metadata = %v, %v", md, err)
			}
			sub, err := c.CreateFolder(ctx, &b1pb.CreateFolderRequest{Parent: loc, Folder: &b1pb.Folder{DisplayName: "archive"}})
			if err != nil {
				t.Fatal(err)
			}
			var root []string
			for it := c.QueryUserRootContents(ctx, &b1pb.QueryUserRootContentsRequest{Location: loc}); ; {
				e, err := it.Next()
				if err != nil {
					break
				}
				root = append(root, e.GetFolder().GetName())
			}
			if !strings.Contains(strings.Join(root, ","), sub.GetName()) {
				t.Errorf("QueryUserRootContents = %v", root)
			}
			fop, err := c.MoveFolder(ctx, &b1pb.MoveFolderRequest{Name: sub.GetName(), DestinationContainingFolder: proto.String(f.GetName())})
			if err != nil {
				t.Fatalf("MoveFolder: %v", err)
			}
			if err := fop.Wait(ctx); err != nil {
				t.Fatalf("MoveFolder: %v", err)
			}
			if got, err := c.GetFolder(ctx, &b1pb.GetFolderRequest{Name: sub.GetName()}); err != nil || got.GetContainingFolder() != f.GetName() {
				t.Errorf("GetFolder of the moved folder = %v, %v", got, err)
			}
			var entries []string
			for it := c.QueryFolderContents(ctx, &b1pb.QueryFolderContentsRequest{Folder: f.GetName()}); ; {
				e, err := it.Next()
				if err != nil {
					break
				}
				entries = append(entries, e.GetFolder().GetName()+e.GetRepository().GetName())
			}
			if strings.Join(entries, ",") != sub.GetName()+","+repo.GetName() {
				t.Errorf("QueryFolderContents = %v", entries)
			}
			var inTeam []string
			for it := c.QueryTeamFolderContents(ctx, &b1pb.QueryTeamFolderContentsRequest{TeamFolder: team.GetName()}); ; {
				e, err := it.Next()
				if err != nil {
					break
				}
				inTeam = append(inTeam, e.GetFolder().GetName())
			}
			if strings.Join(inTeam, ",") != f.GetName() {
				t.Errorf("QueryTeamFolderContents = %v", inTeam)
			}
			if err := c.DeleteFolder(ctx, &b1pb.DeleteFolderRequest{Name: f.GetName()}); err == nil {
				t.Error("a folder that is not empty was deleted")
			}
			dop, err := c.DeleteFolderTree(ctx, &b1pb.DeleteFolderTreeRequest{Name: f.GetName(), Force: true})
			if err != nil {
				t.Fatalf("DeleteFolderTree: %v", err)
			}
			if err := dop.Wait(ctx); err != nil {
				t.Fatalf("DeleteFolderTree: %v", err)
			}
			if _, err := c.GetRepository(ctx, &b1pb.GetRepositoryRequest{Name: repo.GetName()}); err == nil {
				t.Error("the folder tree's repository is still there")
			}
			empty, err := c.CreateFolder(ctx, &b1pb.CreateFolderRequest{Parent: loc, Folder: &b1pb.Folder{DisplayName: "empty", ContainingFolder: team.GetName()}})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.DeleteFolder(ctx, &b1pb.DeleteFolderRequest{Name: empty.GetName()}); err != nil {
				t.Errorf("DeleteFolder: %v", err)
			}
			if err := c.DeleteTeamFolder(ctx, &b1pb.DeleteTeamFolderRequest{Name: team.GetName()}); err != nil {
				t.Errorf("DeleteTeamFolder of the emptied team folder: %v", err)
			}
			tf2, err := c.CreateTeamFolder(ctx, &b1pb.CreateTeamFolderRequest{Parent: loc, TeamFolder: &b1pb.TeamFolder{DisplayName: "Scratch"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.CreateFolder(ctx, &b1pb.CreateFolderRequest{Parent: loc, Folder: &b1pb.Folder{DisplayName: "in", ContainingFolder: tf2.GetName()}}); err != nil {
				t.Fatal(err)
			}
			top, err := c.DeleteTeamFolderTree(ctx, &b1pb.DeleteTeamFolderTreeRequest{Name: tf2.GetName(), Force: true})
			if err != nil {
				t.Fatalf("DeleteTeamFolderTree: %v", err)
			}
			if err := top.Wait(ctx); err != nil {
				t.Fatalf("DeleteTeamFolderTree: %v", err)
			}
			if _, err := c.GetTeamFolder(ctx, &b1pb.GetTeamFolderRequest{Name: tf2.GetName()}); betaCode(err) != codes.NotFound {
				t.Errorf("GetTeamFolder after the tree delete: %v", err)
			}
			cfg, err := c.GetConfig(ctx, &b1pb.GetConfigRequest{Name: loc + "/config"})
			if err != nil || cfg.GetName() != loc+"/config" {
				t.Fatalf("GetConfig = %v, %v", cfg, err)
			}
			if got, err := c.UpdateConfig(ctx, &b1pb.UpdateConfigRequest{Config: &b1pb.Config{Name: loc + "/config"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_kms_key_name"}}}); err != nil || got.GetDefaultKmsKeyName() != "" {
				t.Errorf("UpdateConfig = %v, %v", got, err)
			}
		})
	}
}

// covers: google.cloud.dataform.v1beta1.Dataform/CreateCompilationResult, google.cloud.dataform.v1beta1.Dataform/CreateCompilationResultLongRunning, google.cloud.dataform.v1beta1.Dataform/GetCompilationResult, google.cloud.dataform.v1beta1.Dataform/ListCompilationResults, google.cloud.dataform.v1beta1.Dataform/QueryCompilationResultActions, google.cloud.dataform.v1beta1.Dataform/CreateReleaseConfig, google.cloud.dataform.v1beta1.Dataform/GetReleaseConfig, google.cloud.dataform.v1beta1.Dataform/ListReleaseConfigs, google.cloud.dataform.v1beta1.Dataform/UpdateReleaseConfig, google.cloud.dataform.v1beta1.Dataform/DeleteReleaseConfig, google.cloud.dataform.v1beta1.Dataform/CreateWorkflowConfig, google.cloud.dataform.v1beta1.Dataform/GetWorkflowConfig, google.cloud.dataform.v1beta1.Dataform/ListWorkflowConfigs, google.cloud.dataform.v1beta1.Dataform/UpdateWorkflowConfig, google.cloud.dataform.v1beta1.Dataform/DeleteWorkflowConfig, google.cloud.dataform.v1beta1.Dataform/CreateWorkflowInvocation, google.cloud.dataform.v1beta1.Dataform/GetWorkflowInvocation, google.cloud.dataform.v1beta1.Dataform/ListWorkflowInvocations, google.cloud.dataform.v1beta1.Dataform/QueryWorkflowInvocationActions, google.cloud.dataform.v1beta1.Dataform/CancelWorkflowInvocation, google.cloud.dataform.v1beta1.Dataform/DeleteWorkflowInvocation
//
// TestDataformV1beta1CompileInvokeAndTrigger (#2105): through the
// official v1beta1 client, a workspace's project compiles with Dataform
// core (and as an operation), runs on BigQuery, and a release config's
// compilation is what a workflow config with a table update trigger
// invokes once the table it watches changes in BigQuery: rows inserted
// into the watched table make the next evaluation run the workflow, which
// records the evaluation and the table's modification time.
func TestDataformV1beta1CompileInvokeAndTrigger(t *testing.T) {
	h := New(t)
	clients := dataformBetaClients(t, h)
	bq, project := dataformBigQuery(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: Dataform core runs in a container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	c, rest := clients["grpc"], clients["rest"]
	parent := fmt.Sprintf("projects/%s/locations/us-central1", project)
	sfx := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	dataset := "dfb_" + sfx
	t.Cleanup(func() {
		_ = bq.Dataset(dataset).DeleteWithContents(context.Background())
		_ = bq.Dataset(dataset + "_assertions").DeleteWithContents(context.Background())
	})
	// The watched table, outside Dataform's actions.
	if err := bq.Dataset(dataset).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("create the dataset: %v", err)
	}
	if err := bq.Dataset(dataset).Table("source").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatalf("create the watched table: %v", err)
	}
	repo, err := rest.CreateRepository(ctx, &b1pb.CreateRepositoryRequest{Parent: parent, RepositoryId: "t" + sfx, Repository: &b1pb.Repository{}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteRepository(context.Background(), &b1pb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
	})
	ws, err := c.CreateWorkspace(ctx, &b1pb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "dev", Workspace: &b1pb.Workspace{}})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	for path, body := range map[string]string{
		"workflow_settings.yaml": fmt.Sprintf("defaultProject: %s\ndefaultLocation: US\ndefaultDataset: %s\ndefaultAssertionDataset: %s_assertions\ndataformCoreVersion: 3.1.6\n", project, dataset, dataset),
		"definitions/copy.sqlx":  "config { type: \"table\" }\nselect count(*) as rows_seen from `" + project + "." + dataset + ".source`\n",
	} {
		if _, err := c.WriteFile(ctx, &b1pb.WriteFileRequest{Workspace: ws.GetName(), Path: path, Contents: []byte(body)}); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}
	if _, err := c.CommitWorkspaceChanges(ctx, &b1pb.CommitWorkspaceChangesRequest{Name: ws.GetName(), CommitMessage: "project",
		Author: &b1pb.CommitAuthor{Name: "Ada", EmailAddress: "ada@example.com"}}); err != nil {
		t.Fatalf("CommitWorkspaceChanges: %v", err)
	}
	if _, err := c.PushGitCommits(ctx, &b1pb.PushGitCommitsRequest{Name: ws.GetName()}); err != nil {
		t.Fatalf("PushGitCommits: %v", err)
	}
	comp, err := rest.CreateCompilationResult(ctx, &b1pb.CreateCompilationResultRequest{Parent: repo.GetName(),
		CompilationResult: &b1pb.CompilationResult{Source: &b1pb.CompilationResult_Workspace{Workspace: ws.GetName()}}})
	if err != nil || len(comp.GetCompilationErrors()) > 0 {
		t.Fatalf("CreateCompilationResult = %v, %v", comp, err)
	}
	lop, err := c.CreateCompilationResultLongRunning(ctx, &b1pb.CreateCompilationResultRequest{Parent: repo.GetName(),
		CompilationResult: &b1pb.CompilationResult{Source: &b1pb.CompilationResult_GitCommitish{GitCommitish: "main"}}})
	if err != nil {
		t.Fatalf("CreateCompilationResultLongRunning: %v", err)
	}
	lcomp, err := lop.Wait(ctx)
	if err != nil || len(lcomp.GetResolvedGitCommitSha()) != 40 {
		t.Fatalf("CreateCompilationResultLongRunning = %v, %v", lcomp, err)
	}
	if got, err := c.GetCompilationResult(ctx, &b1pb.GetCompilationResultRequest{Name: comp.GetName()}); err != nil || got.GetWorkspace() != ws.GetName() {
		t.Errorf("GetCompilationResult = %v, %v", got, err)
	}
	n := 0
	for it := rest.ListCompilationResults(ctx, &b1pb.ListCompilationResultsRequest{Parent: repo.GetName()}); ; {
		if _, err := it.Next(); err != nil {
			break
		}
		n++
	}
	if n != 2 {
		t.Errorf("ListCompilationResults: %d results, want 2", n)
	}
	var acts []string
	for it := c.QueryCompilationResultActions(ctx, &b1pb.QueryCompilationResultActionsRequest{Name: comp.GetName()}); ; {
		a, err := it.Next()
		if err != nil {
			break
		}
		acts = append(acts, a.GetTarget().GetName())
	}
	if strings.Join(acts, ",") != "copy" {
		t.Errorf("QueryCompilationResultActions = %v", acts)
	}
	inv, err := rest.CreateWorkflowInvocation(ctx, &b1pb.CreateWorkflowInvocationRequest{Parent: repo.GetName(),
		WorkflowInvocation: &b1pb.WorkflowInvocation{CompilationSource: &b1pb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()}}})
	if err != nil {
		t.Fatalf("CreateWorkflowInvocation: %v", err)
	}
	waitBeta := func(name string) *b1pb.WorkflowInvocation {
		t.Helper()
		deadline := time.Now().Add(4 * time.Minute)
		for {
			got, err := c.GetWorkflowInvocation(ctx, &b1pb.GetWorkflowInvocationRequest{Name: name})
			if err != nil {
				t.Fatalf("GetWorkflowInvocation: %v", err)
			}
			if got.GetState() != b1pb.WorkflowInvocation_RUNNING && got.GetState() != b1pb.WorkflowInvocation_CANCELING {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("invocation %s still %v", name, got.GetState())
			}
			time.Sleep(time.Second)
		}
	}
	if got := waitBeta(inv.GetName()); got.GetState() != b1pb.WorkflowInvocation_SUCCEEDED {
		t.Fatalf("the invocation = %v", got)
	}
	for it := c.QueryWorkflowInvocationActions(ctx, &b1pb.QueryWorkflowInvocationActionsRequest{Name: inv.GetName()}); ; {
		a, err := it.Next()
		if err != nil {
			break
		}
		if a.GetState() != b1pb.WorkflowInvocationAction_SUCCEEDED || !strings.Contains(a.GetBigqueryAction().GetSqlScript(), "rows_seen") {
			t.Errorf("action = %v", a)
		}
	}
	if _, err := c.CancelWorkflowInvocation(ctx, &b1pb.CancelWorkflowInvocationRequest{Name: inv.GetName()}); betaCode(err) != codes.FailedPrecondition {
		t.Errorf("CancelWorkflowInvocation of a finished invocation: %v, want FAILED_PRECONDITION", err)
	}

	rc, err := c.CreateReleaseConfig(ctx, &b1pb.CreateReleaseConfigRequest{Parent: repo.GetName(), ReleaseConfigId: "prod",
		ReleaseConfig: &b1pb.ReleaseConfig{GitCommitish: "main"}})
	if err != nil {
		t.Fatalf("CreateReleaseConfig: %v", err)
	}
	rcomp, err := c.CreateCompilationResult(ctx, &b1pb.CreateCompilationResultRequest{Parent: repo.GetName(),
		CompilationResult: &b1pb.CompilationResult{Source: &b1pb.CompilationResult_ReleaseConfig{ReleaseConfig: rc.GetName()}}})
	if err != nil {
		t.Fatalf("CreateCompilationResult(release config): %v", err)
	}
	if rc, err = rest.UpdateReleaseConfig(ctx, &b1pb.UpdateReleaseConfigRequest{ReleaseConfig: &b1pb.ReleaseConfig{Name: rc.GetName(), ReleaseCompilationResult: rcomp.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"release_compilation_result"}}}); err != nil || rc.GetReleaseCompilationResult() != rcomp.GetName() {
		t.Fatalf("UpdateReleaseConfig = %v, %v", rc, err)
	}
	if got, err := c.GetReleaseConfig(ctx, &b1pb.GetReleaseConfigRequest{Name: rc.GetName()}); err != nil || got.GetGitCommitish() != "main" {
		t.Errorf("GetReleaseConfig = %v, %v", got, err)
	}
	if it := c.ListReleaseConfigs(ctx, &b1pb.ListReleaseConfigsRequest{Parent: repo.GetName()}); true {
		if got, err := it.Next(); err != nil || got.GetName() != rc.GetName() {
			t.Errorf("ListReleaseConfigs = %v, %v", got, err)
		}
	}

	watched := &b1pb.Target{Database: project, Schema: dataset, Name: "source"}
	if _, err := c.CreateWorkflowConfig(ctx, &b1pb.CreateWorkflowConfigRequest{Parent: repo.GetName(), WorkflowConfigId: "bad",
		WorkflowConfig: &b1pb.WorkflowConfig{ReleaseConfig: rc.GetName(), WorkflowTriggerConfig: &b1pb.WorkflowTriggerConfig{}}}); betaCode(err) != codes.InvalidArgument {
		t.Errorf("CreateWorkflowConfig with no triggers: %v, want INVALID_ARGUMENT", err)
	}
	wc, err := rest.CreateWorkflowConfig(ctx, &b1pb.CreateWorkflowConfigRequest{Parent: repo.GetName(), WorkflowConfigId: "on-source",
		WorkflowConfig: &b1pb.WorkflowConfig{ReleaseConfig: rc.GetName(), WorkflowTriggerConfig: &b1pb.WorkflowTriggerConfig{
			Condition: b1pb.WorkflowTriggerConfig_ANY, MinExecutionDuration: durationpb.New(time.Second),
			WorkflowTriggers: []*b1pb.WorkflowTrigger{{Trigger: &b1pb.WorkflowTrigger_TableUpdateTrigger{TableUpdateTrigger: &b1pb.TableUpdateTrigger{Table: watched}}}}}}})
	if err != nil {
		t.Fatalf("CreateWorkflowConfig with a trigger: %v", err)
	}
	if tr := wc.GetWorkflowTriggerConfig().GetWorkflowTriggers(); len(tr) != 1 || tr[0].GetTableUpdateTrigger().GetTable().GetName() != "source" {
		t.Fatalf("the created trigger = %v", wc.GetWorkflowTriggerConfig())
	}
	invocationsOf := func() []string {
		var out []string
		for it := c.ListWorkflowInvocations(ctx, &b1pb.ListWorkflowInvocationsRequest{Parent: repo.GetName()}); ; {
			x, err := it.Next()
			if err != nil {
				break
			}
			if x.GetWorkflowConfig() == wc.GetName() {
				out = append(out, x.GetName())
			}
		}
		return out
	}
	// The first evaluation records the table's time; wait for it.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		got, err := c.GetWorkflowConfig(ctx, &b1pb.GetWorkflowConfigRequest{Name: wc.GetName()})
		if err != nil {
			t.Fatalf("GetWorkflowConfig: %v", err)
		}
		if len(got.GetWorkflowTriggerConfig().GetRecentTriggerEvaluationRecords()) > 0 {
			if s := got.GetWorkflowTriggerConfig().GetRecentTriggerEvaluationRecords()[0].GetStatus(); s.GetCode() != 0 {
				t.Fatalf("the trigger evaluation failed: %v", s)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the trigger was not evaluated in 2 minutes")
		}
		time.Sleep(2 * time.Second)
	}
	if got := invocationsOf(); len(got) != 0 {
		t.Fatalf("the trigger ran before the table changed: %v", got)
	}
	before, err := bq.Dataset(dataset).Table("source").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	q := bq.Query("insert into `" + project + "." + dataset + ".source` (n) values (1), (2), (3)")
	q.Location = "US"
	job, err := q.Run(ctx)
	if err == nil {
		var st *bigquery.JobStatus
		if st, err = job.Wait(ctx); err == nil {
			err = st.Err()
		}
	}
	if err != nil {
		t.Fatalf("insert into the watched table: %v", err)
	}
	// BigQuery moves a table's lastModifiedTime when its rows change.
	if after, err := bq.Dataset(dataset).Table("source").Metadata(ctx); err != nil {
		t.Errorf("tables.get after the insert: %v", err)
	} else if !after.LastModifiedTime.After(before.LastModifiedTime) {
		t.Errorf("lastModifiedTime after the insert = %v, before %v", after.LastModifiedTime, before.LastModifiedTime)
	}
	deadline = time.Now().Add(2 * time.Minute)
	var runs []string
	for {
		if runs = invocationsOf(); len(runs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the table update trigger did not run the workflow within 2 minutes of the insert")
		}
		time.Sleep(2 * time.Second)
	}
	if got := waitBeta(runs[0]); got.GetState() != b1pb.WorkflowInvocation_SUCCEEDED || got.GetResolvedCompilationResult() != rcomp.GetName() {
		t.Errorf("the triggered invocation = %v", got)
	}
	it := bq.Dataset(dataset).Table("copy").Read(ctx)
	var row []bigquery.Value
	if err := it.Next(&row); err != nil || len(row) != 1 || row[0] != int64(3) {
		t.Errorf("the triggered run's table: %v, %v", row, err)
	}
	got, err := c.GetWorkflowConfig(ctx, &b1pb.GetWorkflowConfigRequest{Name: wc.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	tc := got.GetWorkflowTriggerConfig()
	if tc.GetLastSuccessfulEvaluationTime() == nil || tc.GetWorkflowTriggers()[0].GetTableUpdateTrigger().GetTriggerUpdateTime() == nil {
		t.Errorf("the trigger after its run = %v", tc)
	}
	listed := false
	for it := c.ListWorkflowConfigs(ctx, &b1pb.ListWorkflowConfigsRequest{Parent: repo.GetName()}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		listed = listed || (x.GetName() == wc.GetName() && x.GetWorkflowTriggerConfig() != nil)
	}
	if !listed {
		t.Error("ListWorkflowConfigs does not list the config with its trigger")
	}
	// The trigger alone is updated, then cleared, the rest kept.
	up, err := c.UpdateWorkflowConfig(ctx, &b1pb.UpdateWorkflowConfigRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"workflow_trigger_config"}},
		WorkflowConfig: &b1pb.WorkflowConfig{Name: wc.GetName(), WorkflowTriggerConfig: &b1pb.WorkflowTriggerConfig{Condition: b1pb.WorkflowTriggerConfig_ALL,
			WorkflowTriggers: []*b1pb.WorkflowTrigger{{Trigger: &b1pb.WorkflowTrigger_TableUpdateTrigger{TableUpdateTrigger: &b1pb.TableUpdateTrigger{Table: watched}}}}}}})
	if err != nil || up.GetWorkflowTriggerConfig().GetCondition() != b1pb.WorkflowTriggerConfig_ALL || up.GetReleaseConfig() != rc.GetName() ||
		up.GetWorkflowTriggerConfig().GetWorkflowTriggers()[0].GetTableUpdateTrigger().GetTriggerUpdateTime() == nil {
		t.Errorf("UpdateWorkflowConfig(trigger) = %v, %v", up, err)
	}
	up, err = rest.UpdateWorkflowConfig(ctx, &b1pb.UpdateWorkflowConfigRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"workflow_trigger_config", "cron_schedule"}},
		WorkflowConfig: &b1pb.WorkflowConfig{Name: wc.GetName(), CronSchedule: "0 5 * * *"}})
	if err != nil || up.GetWorkflowTriggerConfig() != nil || up.GetCronSchedule() != "0 5 * * *" {
		t.Errorf("UpdateWorkflowConfig(clear the trigger, set a schedule) = %v, %v", up, err)
	}
	for _, r := range append(runs, inv.GetName()) {
		if err := c.DeleteWorkflowInvocation(ctx, &b1pb.DeleteWorkflowInvocationRequest{Name: r}); err != nil {
			t.Errorf("DeleteWorkflowInvocation: %v", err)
		}
	}
	if err := c.DeleteWorkflowConfig(ctx, &b1pb.DeleteWorkflowConfigRequest{Name: wc.GetName()}); err != nil {
		t.Errorf("DeleteWorkflowConfig: %v", err)
	}
	if err := c.DeleteReleaseConfig(ctx, &b1pb.DeleteReleaseConfigRequest{Name: rc.GetName()}); err != nil {
		t.Errorf("DeleteReleaseConfig: %v", err)
	}
}

// covers: google.cloud.dataform.v1.Dataform/InstallNpmPackages, google.cloud.dataform.v1beta1.Dataform/InstallNpmPackages
//
// TestDataformInstallNpmPackages (#2105): a workspace whose package.json
// names @dataform/core, as Google's projects before workflow_settings.yaml
// did, compiles only after InstallNpmPackages has put it in node_modules:
// before, the compilation fails; after (v1, then v1beta1 on a second
// workspace), it compiles its action with the installed core's version.
func TestDataformInstallNpmPackages(t *testing.T) {
	h := New(t)
	v1 := dataformClients(t, h)["grpc"]
	beta := dataformBetaClients(t, h)["rest"]
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: npm and Dataform core run in a container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	parent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
	repo, err := beta.CreateRepository(ctx, &b1pb.CreateRepositoryRequest{Parent: parent, RepositoryId: "npm" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8], Repository: &b1pb.Repository{}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		_ = beta.DeleteRepository(context.Background(), &b1pb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
	})
	files := map[string]string{
		"package.json":           `{"name": "npm-project", "dependencies": {"@dataform/core": "3.1.6"}}`,
		"workflow_settings.yaml": fmt.Sprintf("defaultProject: %s\ndefaultLocation: US\ndefaultDataset: npm_ds\n", h.Project()),
		"definitions/t.sqlx":     "config { type: \"table\" }\nselect 1 as a\n",
	}
	for _, id := range []string{"v1", "beta"} {
		ws, err := beta.CreateWorkspace(ctx, &b1pb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: id, Workspace: &b1pb.Workspace{}})
		if err != nil {
			t.Fatalf("CreateWorkspace: %v", err)
		}
		for p, body := range files {
			if _, err := beta.WriteFile(ctx, &b1pb.WriteFileRequest{Workspace: ws.GetName(), Path: p, Contents: []byte(body)}); err != nil {
				t.Fatalf("WriteFile %s: %v", p, err)
			}
		}
		compile := func() *b1pb.CompilationResult {
			t.Helper()
			c, err := beta.CreateCompilationResult(ctx, &b1pb.CreateCompilationResultRequest{Parent: repo.GetName(),
				CompilationResult: &b1pb.CompilationResult{Source: &b1pb.CompilationResult_Workspace{Workspace: ws.GetName()}}})
			if err != nil {
				return nil
			}
			return c
		}
		if c := compile(); c != nil && len(c.GetCompilationErrors()) == 0 {
			t.Fatalf("%s: the workspace compiled before its packages were installed: %v", id, c)
		}
		if id == "v1" {
			if _, err := v1.InstallNpmPackages(ctx, &dfpb.InstallNpmPackagesRequest{Workspace: ws.GetName()}); err != nil {
				t.Fatalf("InstallNpmPackages (v1): %v", err)
			}
		} else if _, err := beta.InstallNpmPackages(ctx, &b1pb.InstallNpmPackagesRequest{Workspace: ws.GetName()}); err != nil {
			t.Fatalf("InstallNpmPackages (v1beta1): %v", err)
		}
		if _, err := beta.ReadFile(ctx, &b1pb.ReadFileRequest{Workspace: ws.GetName(), Path: "node_modules/@dataform/core/package.json"}); err != nil {
			t.Errorf("%s: node_modules/@dataform/core after InstallNpmPackages: %v", id, err)
		}
		c := compile()
		if c == nil || len(c.GetCompilationErrors()) > 0 || c.GetDataformCoreVersion() != "3.1.6" {
			t.Errorf("%s: the compilation after InstallNpmPackages = %v", id, c)
		}
	}
}
