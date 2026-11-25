//go:build compat

package compat

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	cloudbuildv2 "cloud.google.com/go/cloudbuild/apiv2"
	cb2pb "cloud.google.com/go/cloudbuild/apiv2/cloudbuildpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/google/uuid"
	cb2rest "google.golang.org/api/cloudbuild/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.devtools.cloudbuild.v2.RepositoryManager/CreateConnection, google.devtools.cloudbuild.v2.RepositoryManager/GetConnection, google.devtools.cloudbuild.v2.RepositoryManager/ListConnections, google.devtools.cloudbuild.v2.RepositoryManager/UpdateConnection, google.devtools.cloudbuild.v2.RepositoryManager/DeleteConnection, google.devtools.cloudbuild.v2.RepositoryManager/CreateRepository, google.devtools.cloudbuild.v2.RepositoryManager/BatchCreateRepositories, google.devtools.cloudbuild.v2.RepositoryManager/GetRepository, google.devtools.cloudbuild.v2.RepositoryManager/ListRepositories, google.devtools.cloudbuild.v2.RepositoryManager/DeleteRepository, google.devtools.cloudbuild.v2.RepositoryManager/FetchReadWriteToken, google.devtools.cloudbuild.v2.RepositoryManager/FetchReadToken, google.devtools.cloudbuild.v2.RepositoryManager/FetchLinkableRepositories, google.devtools.cloudbuild.v2.RepositoryManager/FetchGitRefs
//
// TestCloudBuildRepositories2ndGen (#2197), with Google's official clients
// (cloud.google.com/go/cloudbuild/apiv2 over gRPC, google.golang.org/api/cloudbuild/v2
// over JSON) on the Run port. A GitLab connection whose host is this
// instance's git server, its tokens and webhook secret in Secret Manager,
// is installed (COMPLETE); a missing secret is FAILED_PRECONDITION, a
// global connection INVALID_ARGUMENT, gitlab.com FAILED_PRECONDITION, and a
// github.com connection waits for authorization (PENDING_USER_OAUTH). The
// server's Secure Source Manager repositories are linkable; one is linked,
// another batch-linked, both read and listed; FetchGitRefs lists the
// branch and tag; FetchReadToken clones but cannot push and
// FetchReadWriteToken pushes. A 2nd-gen trigger on the repository
// (repositoryEventConfig) runs its build on a push to the server, at the
// pushed commit, with the repository's substitutions; one in another
// region is INVALID_ARGUMENT. The connection's IAM policy is set and read
// back, a stale etag is ABORTED, and deleting the connection deletes its
// repositories.
func TestCloudBuildRepositories2ndGen(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	for _, tool := range []string{"docker", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	ssm, err := securesourcemanager.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ssm.Close() })
	rm, err := cloudbuildv2.NewRepositoryManagerClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rm.Close() })
	cb := cloudBuildClient(ctx, t, h)
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "cb2-" + suffix})
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
	newRepo := func(id string) *ssmpb.Repository {
		op, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id,
			Repository: &ssmpb.Repository{Instance: inst.GetName()}})
		if err != nil {
			t.Fatalf("CreateRepository: %v", err)
		}
		r, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateRepository: %v", err)
		}
		return r
	}
	app, lib := newRepo("app-"+suffix), newRepo("lib-"+suffix)
	u, err := url.Parse(app.GetUris().GetGitHttps())
	if err != nil {
		t.Fatal(err)
	}
	host := u.Scheme + "://" + u.Host

	project := h.Project()
	token := "projects/" + project + "/secrets/" + buildSecret(ctx, t, h, project, "glpat-api") + "/versions/1"
	readToken := "projects/" + project + "/secrets/" + buildSecret(ctx, t, h, project, "glpat-read") + "/versions/1"
	hook := "projects/" + project + "/secrets/" + buildSecret(ctx, t, h, project, "webhook-secret") + "/versions/1"
	gitlab := func(hostURI, read string) *cb2pb.Connection {
		return &cb2pb.Connection{ConnectionConfig: &cb2pb.Connection_GitlabConfig{GitlabConfig: &cb2pb.GitLabConfig{
			HostUri: hostURI, WebhookSecretSecretVersion: hook,
			ReadAuthorizerCredential: &cb2pb.UserCredential{UserTokenSecretVersion: read},
			AuthorizerCredential:     &cb2pb.UserCredential{UserTokenSecretVersion: token}}}}
	}
	for name, tc := range map[string]struct {
		parent string
		c      *cb2pb.Connection
		code   codes.Code
	}{
		"global":         {"projects/" + project + "/locations/global", gitlab(host, readToken), codes.InvalidArgument},
		"gitlab.com":     {parent, gitlab("", readToken), codes.FailedPrecondition},
		"missing secret": {parent, gitlab(host, "projects/"+project+"/secrets/missing-"+suffix+"/versions/1"), codes.FailedPrecondition},
		"no config":      {parent, &cb2pb.Connection{}, codes.InvalidArgument},
	} {
		if _, err := rm.CreateConnection(ctx, &cb2pb.CreateConnectionRequest{Parent: tc.parent, ConnectionId: "bad-" + suffix,
			Connection: tc.c}); status.Code(err) != tc.code {
			t.Errorf("CreateConnection, %s: %v; want %v", name, err, tc.code)
		}
	}
	ghop, err := rm.CreateConnection(ctx, &cb2pb.CreateConnectionRequest{Parent: parent, ConnectionId: "gh-" + suffix,
		Connection: &cb2pb.Connection{ConnectionConfig: &cb2pb.Connection_GithubConfig{GithubConfig: &cb2pb.GitHubConfig{}}}})
	if err != nil {
		t.Fatalf("CreateConnection githubConfig: %v", err)
	}
	gh, err := ghop.Wait(ctx)
	if err != nil || gh.GetInstallationState().GetStage() != cb2pb.InstallationState_PENDING_USER_OAUTH {
		t.Errorf("the github.com connection = %v, %v; want PENDING_USER_OAUTH", gh, err)
	}
	if gh != nil {
		if _, err := rm.FetchLinkableRepositories(ctx, &cb2pb.FetchLinkableRepositoriesRequest{Connection: gh.GetName()}).Next(); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("FetchLinkableRepositories of a pending connection: %v; want FailedPrecondition", err)
		}
		if op, err := rm.DeleteConnection(ctx, &cb2pb.DeleteConnectionRequest{Name: gh.GetName()}); err != nil || op.Wait(ctx) != nil {
			t.Errorf("DeleteConnection: %v", err)
		}
	}

	cop, err := rm.CreateConnection(ctx, &cb2pb.CreateConnectionRequest{Parent: parent, ConnectionId: "gl-" + suffix,
		Connection: gitlab(host, readToken)})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	conn, err := cop.Wait(ctx)
	if err != nil || conn.GetInstallationState().GetStage() != cb2pb.InstallationState_COMPLETE {
		t.Fatalf("the connection = %v, %v; want COMPLETE", conn, err)
	}
	t.Cleanup(func() {
		if op, err := rm.DeleteConnection(context.Background(), &cb2pb.DeleteConnectionRequest{Name: conn.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if got, err := rm.GetConnection(ctx, &cb2pb.GetConnectionRequest{Name: conn.GetName()}); err != nil || got.GetGitlabConfig().GetHostUri() != host {
		t.Errorf("GetConnection = %v, %v", got, err)
	}
	listed := false
	for it := rm.ListConnections(ctx, &cb2pb.ListConnectionsRequest{Parent: parent}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		listed = listed || x.GetName() == conn.GetName()
	}
	if !listed {
		t.Error("ListConnections does not list the connection")
	}
	uop, err := rm.UpdateConnection(ctx, &cb2pb.UpdateConnectionRequest{Connection: &cb2pb.Connection{Name: conn.GetName(),
		Annotations: map[string]string{"team": "build"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"annotations"}}})
	if err != nil {
		t.Fatalf("UpdateConnection: %v", err)
	}
	updated, err := uop.Wait(ctx)
	if err != nil || updated.GetAnnotations()["team"] != "build" || updated.GetEtag() == conn.GetEtag() {
		t.Errorf("UpdateConnection = %v, %v", updated, err)
	}
	if _, err := rm.DeleteConnection(ctx, &cb2pb.DeleteConnectionRequest{Name: conn.GetName(), Etag: conn.GetEtag()}); status.Code(err) != codes.Aborted {
		t.Errorf("DeleteConnection with a stale etag: %v; want Aborted", err)
	}

	linkable := map[string]bool{}
	for it := rm.FetchLinkableRepositories(ctx, &cb2pb.FetchLinkableRepositoriesRequest{Connection: conn.GetName()}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("FetchLinkableRepositories: %v", err)
		}
		linkable[x.GetRemoteUri()] = true
	}
	if !linkable[app.GetUris().GetGitHttps()] || !linkable[lib.GetUris().GetGitHttps()] {
		t.Errorf("FetchLinkableRepositories = %v; want both repositories", linkable)
	}
	if _, err := rm.CreateRepository(ctx, &cb2pb.CreateRepositoryRequest{Parent: conn.GetName(), RepositoryId: "hosted",
		Repository: &cb2pb.Repository{RemoteUri: "https://gitlab.com/octocat/hello.git"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateRepository on another host: %v; want InvalidArgument", err)
	}
	rop, err := rm.CreateRepository(ctx, &cb2pb.CreateRepositoryRequest{Parent: conn.GetName(), RepositoryId: "app",
		Repository: &cb2pb.Repository{RemoteUri: app.GetUris().GetGitHttps()}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil || repo.GetRemoteUri() != app.GetUris().GetGitHttps() {
		t.Fatalf("the repository = %v, %v", repo, err)
	}
	bop, err := rm.BatchCreateRepositories(ctx, &cb2pb.BatchCreateRepositoriesRequest{Parent: conn.GetName(),
		Requests: []*cb2pb.CreateRepositoryRequest{{RepositoryId: "lib", Repository: &cb2pb.Repository{RemoteUri: lib.GetUris().GetGitHttps()}}}})
	if err != nil {
		t.Fatalf("BatchCreateRepositories: %v", err)
	}
	if got, err := bop.Wait(ctx); err != nil || len(got.GetRepositories()) != 1 || got.GetRepositories()[0].GetName() != conn.GetName()+"/repositories/lib" {
		t.Errorf("BatchCreateRepositories = %v, %v", got, err)
	}
	if got, err := rm.GetRepository(ctx, &cb2pb.GetRepositoryRequest{Name: repo.GetName()}); err != nil || got.GetRemoteUri() != repo.GetRemoteUri() {
		t.Errorf("GetRepository = %v, %v", got, err)
	}
	var names []string
	for it := rm.ListRepositories(ctx, &cb2pb.ListRepositoriesRequest{Parent: conn.GetName()}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		names = append(names, x.GetName())
	}
	if len(names) != 2 {
		t.Errorf("ListRepositories = %v; want app and lib", names)
	}

	// A 2nd-gen trigger on the repository, and one in another region.
	// The Compute Engine default account, which needs no IAM on this shard.
	sa := "projects/" + project + "/serviceAccounts/" + strconv.FormatInt(resource.ProjectNumber(project), 10) + "-compute@developer.gserviceaccount.com"
	build := &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: "busybox", Args: []string{"sh", "-c", "test -f hello.txt && echo $_VIA $REPO_NAME $BRANCH_NAME"}}},
		Substitutions: map[string]string{"_VIA": "v2"}, Options: &cbpb.BuildOptions{Logging: cbpb.BuildOptions_CLOUD_LOGGING_ONLY}}
	if _, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: "projects/" + project + "/locations/europe-west1",
		ProjectId: project, Trigger: &cbpb.BuildTrigger{Name: "far-" + suffix, ServiceAccount: sa, BuildTemplate: &cbpb.BuildTrigger_Build{Build: build},
			RepositoryEventConfig: &cbpb.RepositoryEventConfig{Repository: repo.GetName(),
				Filter: &cbpb.RepositoryEventConfig_Push{Push: &cbpb.PushFilter{GitRef: &cbpb.PushFilter_Branch{Branch: "^main$"}}}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateBuildTrigger in another region: %v; want InvalidArgument", err)
	}
	trig, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: project,
		Trigger: &cbpb.BuildTrigger{Name: "v2-" + suffix, ServiceAccount: sa, BuildTemplate: &cbpb.BuildTrigger_Build{Build: build},
			RepositoryEventConfig: &cbpb.RepositoryEventConfig{Repository: repo.GetName(),
				Filter: &cbpb.RepositoryEventConfig_Push{Push: &cbpb.PushFilter{GitRef: &cbpb.PushFilter_Branch{Branch: "^main$"}}}}}})
	if err != nil {
		t.Fatalf("CreateBuildTrigger: %v", err)
	}
	t.Cleanup(func() {
		_ = cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{Name: trig.GetResourceName(), ProjectId: project})
	})
	if trig.GetRepositoryEventConfig().GetRepositoryType() != cbpb.RepositoryEventConfig_GITLAB_ENTERPRISE {
		t.Errorf("the trigger's repositoryType = %v; want GITLAB_ENTERPRISE", trig.GetRepositoryEventConfig().GetRepositoryType())
	}

	// Push to the server with a read-write token; a read token cannot.
	rw, err := rm.FetchReadWriteToken(ctx, &cb2pb.FetchReadWriteTokenRequest{Repository: repo.GetName()})
	if err != nil || rw.GetToken() == "" {
		t.Fatalf("FetchReadWriteToken = %v, %v", rw, err)
	}
	work := t.TempDir()
	git := func(bearer string, args ...string) (string, error) {
		pre := []string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "http.extraHeader=Authorization: Bearer " + bearer}
		out, err := exec.CommandContext(ctx, "git", append(pre, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(bearer string, args ...string) string {
		t.Helper()
		out, err := git(bearer, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return out
	}
	must(rw.GetToken(), "clone", "--quiet", app.GetUris().GetGitHttps(), ".")
	if err := os.WriteFile(work+"/hello.txt", []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(rw.GetToken(), "checkout", "--quiet", "-b", "main")
	must(rw.GetToken(), "add", ".")
	must(rw.GetToken(), "commit", "--quiet", "-m", "first")
	must(rw.GetToken(), "tag", "v1")
	must(rw.GetToken(), "push", "--quiet", "origin", "main", "v1")
	sha := must(rw.GetToken(), "rev-parse", "HEAD")
	rd, err := rm.FetchReadToken(ctx, &cb2pb.FetchReadTokenRequest{Repository: repo.GetName()})
	if err != nil || rd.GetToken() == "" || rd.GetExpirationTime() == nil {
		t.Fatalf("FetchReadToken = %v, %v", rd, err)
	}
	if _, err := git(rd.GetToken(), "fetch", "--quiet", "origin"); err != nil {
		t.Errorf("a fetch with the read token: %v", err)
	}
	if out, err := git(rd.GetToken(), "push", "--quiet", "origin", "HEAD:refs/heads/other"); err == nil {
		t.Errorf("a push with the read token succeeded: %s", out)
	}

	for typ, want := range map[cb2pb.FetchGitRefsRequest_RefType]string{cb2pb.FetchGitRefsRequest_BRANCH: "main", cb2pb.FetchGitRefsRequest_TAG: "v1"} {
		refs, err := rm.FetchGitRefs(ctx, &cb2pb.FetchGitRefsRequest{Repository: repo.GetName(), RefType: typ})
		if err != nil || len(refs.GetRefNames()) != 1 || refs.GetRefNames()[0] != want {
			t.Errorf("FetchGitRefs %v = %v, %v; want %s", typ, refs, err, want)
		}
	}

	var got *cbpb.Build
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline) && (got == nil || got.GetFinishTime() == nil); time.Sleep(time.Second) {
		it := cb.ListBuilds(ctx, &cbpb.ListBuildsRequest{Parent: parent, ProjectId: project})
		for {
			b, err := it.Next()
			if err != nil {
				break
			}
			if b.GetBuildTriggerId() == trig.GetId() && b.GetSource().GetGitSource().GetRevision() == sha {
				got = b
			}
		}
	}
	// The step's arguments carry the substitutions, built-in ones too.
	if got == nil || got.GetStatus() != cbpb.Build_SUCCESS || len(got.GetSteps()) != 1 ||
		!strings.HasSuffix(got.GetSteps()[0].GetArgs()[2], "echo v2 app-"+suffix+" main") || got.GetSource().GetGitSource().GetUrl() != repo.GetRemoteUri() {
		t.Fatalf("the push's build = %v; want SUCCESS at %s with the repository's substitutions", got, sha)
	}

	// The JSON API, through Google's REST client: the connection, its IAM
	// policy and its repositories.
	svc, err := cb2rest.NewService(ctx, option.WithEndpoint("http://"+h.Endpoint(EnvRun)+"/"), option.WithoutAuthentication(),
		option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	jc, err := svc.Projects.Locations.Connections.Get(conn.GetName()).Context(ctx).Do()
	if err != nil || jc.GitlabConfig == nil || jc.GitlabConfig.HostUri != host || jc.InstallationState.Stage != "COMPLETE" {
		t.Errorf("GET connection = %+v, %v", jc, err)
	}
	jr, err := svc.Projects.Locations.Connections.Repositories.List(conn.GetName()).Context(ctx).Do()
	if err != nil || len(jr.Repositories) != 2 {
		t.Errorf("GET repositories = %+v, %v", jr, err)
	}
	member := "user:builder@example.com"
	if _, err := svc.Projects.Locations.Connections.SetIamPolicy(conn.GetName(), &cb2rest.SetIamPolicyRequest{
		Policy: &cb2rest.Policy{Bindings: []*cb2rest.Binding{{Role: "roles/cloudbuild.tokenAccessor", Members: []string{member}}}}}).Context(ctx).Do(); err != nil {
		t.Errorf("setIamPolicy: %v", err)
	}
	pol, err := rm.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: conn.GetName()})
	if err != nil || len(pol.GetBindings()) != 1 || pol.GetBindings()[0].GetMembers()[0] != member {
		t.Errorf("GetIamPolicy = %v, %v", pol, err)
	}

	dop, err := rm.DeleteRepository(ctx, &cb2pb.DeleteRepositoryRequest{Name: conn.GetName() + "/repositories/lib"})
	if err != nil || dop.Wait(ctx) != nil {
		t.Errorf("DeleteRepository: %v", err)
	}
	cdop, err := rm.DeleteConnection(ctx, &cb2pb.DeleteConnectionRequest{Name: conn.GetName()})
	if err != nil || cdop.Wait(ctx) != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}
	if _, err := rm.GetRepository(ctx, &cb2pb.GetRepositoryRequest{Name: repo.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after the connection's deletion: %v; want NotFound", err)
	}
}
