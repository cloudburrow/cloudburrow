package dataform

import (
	"context"
	"net"
	"net/http/httptest"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	dataformb1 "cloud.google.com/go/dataform/apiv1beta1"
	b1pb "cloud.google.com/go/dataform/apiv1beta1/dataformpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// betaOnlyFields are the fields v1beta1 has and v1 has not; every other
// field of a message both have must match by number, name and kind, or
// re-reading one as the other would lose or garble it.
var betaOnlyFields = map[string]bool{
	"Repository.GitRemoteSettings.account_connector": true,
	"Workspace.original_branch":                      true,
	"Workspace.enable_branch_management":             true,
	"Workspace.depth":                                true,
	"Workspace.shallow":                              true,
	"WorkflowConfig.workflow_trigger_config":         true,
	"CreateFolderRequest.folder_id":                  true,
	"CreateTeamFolderRequest.team_folder_id":         true,
}

func walkMessages(md protoreflect.MessageDescriptors, f func(protoreflect.MessageDescriptor)) {
	for i := 0; i < md.Len(); i++ {
		f(md.Get(i))
		walkMessages(md.Get(i).Messages(), f)
	}
}

func TestBetaWireCompatible(t *testing.T) {
	v1 := map[string]protoreflect.MessageDescriptor{}
	walkMessages(dfpb.File_google_cloud_dataform_v1_dataform_proto.Messages(), func(m protoreflect.MessageDescriptor) {
		v1[strings.TrimPrefix(string(m.FullName()), "google.cloud.dataform.v1.")] = m
	})
	walkMessages(b1pb.File_google_cloud_dataform_v1beta1_dataform_proto.Messages(), func(m protoreflect.MessageDescriptor) {
		n := strings.TrimPrefix(string(m.FullName()), "google.cloud.dataform.v1beta1.")
		a, ok := v1[n]
		if !ok {
			return
		}
		for i := 0; i < m.Fields().Len(); i++ {
			f := m.Fields().Get(i)
			g := a.Fields().ByNumber(f.Number())
			if g == nil {
				if !betaOnlyFields[n+"."+string(f.Name())] {
					t.Errorf("v1beta1 %s.%s (%d) is not in v1 and not served", n, f.Name(), f.Number())
				}
				continue
			}
			if g.Name() != f.Name() || g.Kind() != f.Kind() || g.Cardinality() != f.Cardinality() {
				t.Errorf("%s field %d: v1beta1 %s %v, v1 %s %v", n, f.Number(), f.Name(), f.Kind(), g.Name(), g.Kind())
			}
		}
	})
	// Every v1beta1 method is served.
	s := &Server{}
	s.clear()
	served := map[string]bool{}
	for _, m := range s.betaDesc().Methods {
		served[m.MethodName] = true
	}
	for _, m := range b1pb.Dataform_ServiceDesc.Methods {
		if !served[m.MethodName] {
			t.Errorf("%s is not served", m.MethodName)
		}
	}
}

// fakeTables is a Warehouse whose tables' modification times the test sets.
type fakeTables struct {
	mod map[string]time.Time
}

func (f *fakeTables) EnsureDataset(context.Context, string, string, string) error { return nil }
func (f *fakeTables) Table(context.Context, string, string, string) (bool, string, []string, error) {
	return false, "", nil, nil
}
func (f *fakeTables) Exec(context.Context, string, string, string) (string, error) { return "j", nil }
func (f *fakeTables) Count(context.Context, string, string, string) (string, int64, error) {
	return "j", 0, nil
}
func (f *fakeTables) Modified(_ context.Context, p, d, n string) (time.Time, bool, error) {
	m, ok := f.mod[p+"."+d+"."+n]
	return m, ok, nil
}

func betaClients(t *testing.T, s *Server) (grpcC, restC *dataformb1.Client) {
	t.Helper()
	ctx := context.Background()
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	s.Register(g)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	var err error
	grpcC, err = dataformb1.NewClient(ctx, option.WithEndpoint("bufnet"), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() })),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = grpcC.Close() })
	hs := httptest.NewServer(NewRESTHandler(s))
	t.Cleanup(hs.Close)
	restC, err = dataformb1.NewRESTClient(ctx, option.WithEndpoint(hs.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restC.Close() })
	return grpcC, restC
}

func branchNames(t *testing.T, c *dataformb1.Client, ws string, f b1pb.FetchWorkspaceBranchesRequest_BranchFilter) []string {
	t.Helper()
	it := c.FetchWorkspaceBranches(context.Background(), &b1pb.FetchWorkspaceBranchesRequest{Name: ws, Filter: f})
	var out []string
	for {
		b, err := it.Next()
		if err != nil {
			break
		}
		out = append(out, b.GetBranchName())
	}
	sort.Strings(out)
	return out
}

func TestBetaBranchesAndShallowWorkspaces(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	s, err := NewServer(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, rc := betaClients(t, s)
	const parent = "projects/p/locations/us-central1"
	repo, err := c.CreateRepository(ctx, &b1pb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r", Repository: &b1pb.Repository{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRepository(ctx, &b1pb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r2", Repository: &b1pb.Repository{
		GitRemoteSettings: &b1pb.Repository_GitRemoteSettings{Url: "https://x", DefaultBranch: "main", AccountConnector: ptr("projects/p/accountConnectors/a")}}}); code(err) != codes.InvalidArgument {
		t.Fatalf("a malformed account_connector: %v, want INVALID_ARGUMENT", err)
	}
	if _, err := c.CreateWorkspace(ctx, &b1pb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "bad", Workspace: &b1pb.Workspace{Depth: 1}}); code(err) != codes.InvalidArgument {
		t.Fatalf("depth without branch management: %v", err)
	}
	ws, err := c.CreateWorkspace(ctx, &b1pb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "w",
		Workspace: &b1pb.Workspace{EnableBranchManagement: ptr(true)}})
	if err != nil {
		t.Fatal(err)
	}
	if !ws.GetEnableBranchManagement() || ws.GetShallow() {
		t.Fatalf("workspace = %v", ws)
	}
	author := &b1pb.CommitAuthor{Name: "A", EmailAddress: "a@example.com"}
	commit := func(path, msg string) {
		t.Helper()
		if _, err := c.WriteFile(ctx, &b1pb.WriteFileRequest{Workspace: ws.GetName(), Path: path, Contents: []byte(msg)}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CommitWorkspaceChanges(ctx, &b1pb.CommitWorkspaceChangesRequest{Name: ws.GetName(), Author: author, CommitMessage: msg}); err != nil {
			t.Fatal(err)
		}
	}
	commit("a.sqlx", "one")
	commit("b.sqlx", "two")
	if _, err := c.PushGitCommits(ctx, &b1pb.PushGitCommitsRequest{Name: ws.GetName()}); err != nil {
		t.Fatal(err)
	}
	cur, err := rc.FetchCurrentWorkspaceBranch(ctx, &b1pb.FetchCurrentWorkspaceBranchRequest{Name: ws.GetName()})
	if err != nil || cur.GetBranchName() != "main" {
		t.Fatalf("current branch (REST) = %v, %v", cur, err)
	}
	if err := c.CheckoutWorkspaceBranch(ctx, &b1pb.CheckoutWorkspaceBranchRequest{Name: ws.GetName(), Branch: "feature"}); code(err) != codes.NotFound {
		t.Fatalf("checkout of a missing branch: %v", err)
	}
	if err := c.CheckoutWorkspaceBranch(ctx, &b1pb.CheckoutWorkspaceBranchRequest{Name: ws.GetName(), Branch: "feature", CreateIfNotExists: true, SourceBranch: "origin/main"}); err != nil {
		t.Fatal(err)
	}
	commit("c.sqlx", "three")
	// Push without remote_branch goes to the checked-out branch.
	if _, err := c.PushGitCommits(ctx, &b1pb.PushGitCommitsRequest{Name: ws.GetName()}); err != nil {
		t.Fatal(err)
	}
	if got := branchNames(t, rc, ws.GetName(), b1pb.FetchWorkspaceBranchesRequest_ALL); strings.Join(got, ",") != "feature,main,origin/feature,origin/main" {
		t.Fatalf("branches = %v", got)
	}
	if got := branchNames(t, c, ws.GetName(), b1pb.FetchWorkspaceBranchesRequest_LOCAL_ONLY); strings.Join(got, ",") != "feature,main" {
		t.Fatalf("local branches = %v", got)
	}
	h, err := c.FetchWorkspaceHistory(ctx, &b1pb.FetchWorkspaceHistoryRequest{Name: ws.GetName()}).Next()
	if err != nil || h.GetCommitMessage() != "three" {
		t.Fatalf("history head = %v, %v", h, err)
	}
	if _, err := c.DeleteBranch(ctx, &b1pb.DeleteBranchRequest{Name: ws.GetName(), Branch: "feature"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("delete of the checked-out branch: %v", err)
	}
	if err := c.CheckoutWorkspaceBranch(ctx, &b1pb.CheckoutWorkspaceBranchRequest{Name: ws.GetName(), Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteBranch(ctx, &b1pb.DeleteBranchRequest{Name: ws.GetName(), Branch: "feature"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("delete of an unmerged branch without force: %v", err)
	}
	if _, err := c.DeleteBranch(ctx, &b1pb.DeleteBranchRequest{Name: ws.GetName(), Branch: "feature", Force: true}); err != nil {
		t.Fatal(err)
	}

	// A shallow workspace of the remote's feature branch.
	sh, err := c.CreateWorkspace(ctx, &b1pb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "shallow",
		Workspace: &b1pb.Workspace{EnableBranchManagement: ptr(true), Depth: 1, OriginalBranch: ptr("feature")}})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.GetShallow() {
		t.Fatalf("shallow = %v", sh)
	}
	resp, err := c.FetchWorkspaceHistory(ctx, &b1pb.FetchWorkspaceHistoryRequest{Name: sh.GetName()}).Next()
	if err != nil || resp.GetCommitMessage() != "three" {
		t.Fatalf("shallow head = %v, %v", resp, err)
	}
	count := func(name string) (int, bool) {
		it := c.FetchWorkspaceHistory(ctx, &b1pb.FetchWorkspaceHistoryRequest{Name: name})
		n := 0
		for {
			if _, err := it.Next(); err != nil {
				break
			}
			n++
		}
		return n, it.Response.(*b1pb.FetchWorkspaceHistoryResponse).GetReachesShallowBoundary()
	}
	if n, edge := count(sh.GetName()); n != 1 || !edge {
		t.Fatalf("shallow history: %d commits, boundary %v", n, edge)
	}
	if _, err := c.SyncWorkspaceRefs(ctx, &b1pb.SyncWorkspaceRefsRequest{Name: sh.GetName(), Deepen: 1}); err != nil {
		t.Fatal(err)
	}
	if n, _ := count(sh.GetName()); n < 2 {
		t.Fatalf("deepened history: %d commits", n)
	}
	if cur, _ := c.FetchCurrentWorkspaceBranch(ctx, &b1pb.FetchCurrentWorkspaceBranchRequest{Name: sh.GetName()}); cur.GetBranchName() != "feature" {
		t.Fatalf("original_branch: current = %v", cur)
	}

	// A v1 workspace cannot check out branches.
	plain, err := c.CreateWorkspace(ctx, &b1pb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "plain", Workspace: &b1pb.Workspace{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckoutWorkspaceBranch(ctx, &b1pb.CheckoutWorkspaceBranchRequest{Name: plain.GetName(), Branch: "x", CreateIfNotExists: true}); code(err) != codes.FailedPrecondition {
		t.Fatalf("checkout without branch management: %v", err)
	}

	// DeleteRepositoryLongRunning refuses while workspaces exist.
	if _, err := c.DeleteRepositoryLongRunning(ctx, &b1pb.DeleteRepositoryLongRunningRequest{Name: repo.GetName(), Force: true}); code(err) != codes.FailedPrecondition {
		t.Fatalf("long-running delete with workspaces: %v", err)
	}
	for _, w := range []string{ws.GetName(), sh.GetName(), plain.GetName()} {
		if err := c.DeleteWorkspace(ctx, &b1pb.DeleteWorkspaceRequest{Name: w}); err != nil {
			t.Fatal(err)
		}
	}
	op, err := rc.DeleteRepositoryLongRunning(ctx, &b1pb.DeleteRepositoryLongRunningRequest{Name: repo.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	md, err := op.Metadata()
	if err != nil || md.GetState() != b1pb.DeleteRepositoryLongRunningMetadata_SUCCEEDED {
		t.Fatalf("metadata = %v, %v", md, err)
	}
	if _, err := c.GetRepository(ctx, &b1pb.GetRepositoryRequest{Name: repo.GetName()}); code(err) != codes.NotFound {
		t.Fatalf("after delete: %v", err)
	}

	f, err := c.CreateFolder(ctx, &b1pb.CreateFolderRequest{Parent: parent, FolderId: "team-a", Folder: &b1pb.Folder{DisplayName: "A"}})
	if err != nil || f.GetName() != parent+"/folders/team-a" {
		t.Fatalf("folder = %v, %v", f, err)
	}
	if _, err := c.CreateFolder(ctx, &b1pb.CreateFolderRequest{Parent: parent, FolderId: "team-a", Folder: &b1pb.Folder{DisplayName: "A"}}); code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate folder_id: %v", err)
	}
	tf, err := c.CreateTeamFolder(ctx, &b1pb.CreateTeamFolderRequest{Parent: parent, TeamFolderId: "tf", TeamFolder: &b1pb.TeamFolder{DisplayName: "T"}})
	if err != nil || tf.GetName() != parent+"/teamFolders/tf" {
		t.Fatalf("team folder = %v, %v", tf, err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestBetaTableUpdateTriggers(t *testing.T) {
	ctx := context.Background()
	wh := &fakeTables{mod: map[string]time.Time{}}
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	s, err := NewServer(Options{DataDir: t.TempDir(), Warehouse: wh, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, rc := betaClients(t, s)
	const parent = "projects/p/locations/us-central1"
	repo, err := c.CreateRepository(ctx, &b1pb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r", Repository: &b1pb.Repository{}})
	if err != nil {
		t.Fatal(err)
	}
	rel, err := c.CreateReleaseConfig(ctx, &b1pb.CreateReleaseConfigRequest{Parent: repo.GetName(), ReleaseConfigId: "rel",
		ReleaseConfig: &b1pb.ReleaseConfig{GitCommitish: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	// A compilation with no actions stands for the release's.
	comp := repo.GetName() + "/compilationResults/c1"
	s.mu.Lock()
	s.compilations[comp] = &compilation{C: &dfpb.CompilationResult{Name: comp}}
	s.releases[rel.GetName()].ReleaseCompilationResult = comp
	s.mu.Unlock()
	trig := func(tables ...string) *b1pb.WorkflowTriggerConfig {
		tc := &b1pb.WorkflowTriggerConfig{Condition: b1pb.WorkflowTriggerConfig_ALL, MaxWaitDuration: durationpb.New(time.Hour)}
		for _, tb := range tables {
			tc.WorkflowTriggers = append(tc.WorkflowTriggers, &b1pb.WorkflowTrigger{Trigger: &b1pb.WorkflowTrigger_TableUpdateTrigger{
				TableUpdateTrigger: &b1pb.TableUpdateTrigger{Table: &b1pb.Target{Schema: "d", Name: tb}}}})
		}
		return tc
	}
	if _, err := c.CreateWorkflowConfig(ctx, &b1pb.CreateWorkflowConfigRequest{Parent: repo.GetName(), WorkflowConfigId: "bad",
		WorkflowConfig: &b1pb.WorkflowConfig{ReleaseConfig: rel.GetName(), WorkflowTriggerConfig: &b1pb.WorkflowTriggerConfig{}}}); code(err) != codes.InvalidArgument {
		t.Fatalf("empty trigger: %v", err)
	}
	wc, err := c.CreateWorkflowConfig(ctx, &b1pb.CreateWorkflowConfigRequest{Parent: repo.GetName(), WorkflowConfigId: "wc",
		WorkflowConfig: &b1pb.WorkflowConfig{ReleaseConfig: rel.GetName(), WorkflowTriggerConfig: trig("t1", "t2")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(wc.GetWorkflowTriggerConfig().GetWorkflowTriggers()) != 2 {
		t.Fatalf("created = %v", wc)
	}
	invocations := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.invocations)
	}
	wh.mod["p.d.t1"] = now.Add(-time.Hour)
	s.evaluateTriggers(ctx, now) // first sight: t1 recorded, t2 absent
	wh.mod["p.d.t1"] = now
	now = now.Add(time.Minute)
	s.evaluateTriggers(ctx, now)
	if n := invocations(); n != 0 {
		t.Fatalf("ALL with one of two tables changed ran %d invocations", n)
	}
	wh.mod["p.d.t2"] = now
	now = now.Add(time.Minute)
	s.evaluateTriggers(ctx, now)
	if n := invocations(); n != 1 {
		t.Fatalf("ALL met: %d invocations", n)
	}
	s.wg.Wait()
	now = now.Add(time.Minute)
	s.evaluateTriggers(ctx, now)
	if n := invocations(); n != 1 {
		t.Fatalf("no change since: %d invocations", n)
	}
	got, err := rc.GetWorkflowConfig(ctx, &b1pb.GetWorkflowConfigRequest{Name: wc.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	tc := got.GetWorkflowTriggerConfig()
	if len(tc.GetRecentTriggerEvaluationRecords()) != 4 || tc.GetLastSuccessfulEvaluationTime() == nil ||
		tc.GetWorkflowTriggers()[1].GetTableUpdateTrigger().GetTriggerUpdateTime() == nil {
		t.Fatalf("trigger config = %v", tc)
	}
	// Only the trigger: ANY of t1.
	up, err := c.UpdateWorkflowConfig(ctx, &b1pb.UpdateWorkflowConfigRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"workflow_trigger_config"}},
		WorkflowConfig: &b1pb.WorkflowConfig{Name: wc.GetName(), WorkflowTriggerConfig: &b1pb.WorkflowTriggerConfig{
			Condition: b1pb.WorkflowTriggerConfig_ANY, WorkflowTriggers: trig("t1").GetWorkflowTriggers()}}})
	if err != nil {
		t.Fatal(err)
	}
	if up.GetWorkflowTriggerConfig().GetCondition() != b1pb.WorkflowTriggerConfig_ANY || up.GetReleaseConfig() != rel.GetName() {
		t.Fatalf("updated = %v", up)
	}
	wh.mod["p.d.t1"] = now
	now = now.Add(time.Minute)
	s.evaluateTriggers(ctx, now)
	s.wg.Wait()
	if n := invocations(); n != 2 {
		t.Fatalf("ANY met: %d invocations", n)
	}
	// Clearing it.
	up, err = c.UpdateWorkflowConfig(ctx, &b1pb.UpdateWorkflowConfigRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"workflow_trigger_config"}},
		WorkflowConfig: &b1pb.WorkflowConfig{Name: wc.GetName()}})
	if err != nil || up.GetWorkflowTriggerConfig() != nil {
		t.Fatalf("cleared = %v, %v", up, err)
	}
}
