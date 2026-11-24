package run

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	cb2pb "cloud.google.com/go/cloudbuild/apiv2/cloudbuildpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type cb2Secrets map[string]string

func (f cb2Secrets) AccessSecretVersion(_ context.Context, name string) ([]byte, error) {
	v, ok := f[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "secret version %s not found", name)
	}
	return []byte(v), nil
}

// TestCloudBuildV2ConnectionsAndRepositories (#2197): a GitLab connection
// to this instance's git server is installed once its secrets are read;
// one to another host, or with a missing secret, is refused; a github.com
// connection waits for authorization; repositories link the server's
// repositories by git URL; refs and tokens are the repository's; a
// 2nd-gen trigger must be in the repository's region; deleting the
// connection deletes its repositories.
func TestCloudBuildV2ConnectionsAndRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := UseSourceManagerRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s := NewServer(&Knative{}, "t", time.Minute)
	var srv *httptest.Server
	ssm := s.SecureSourceManager(func() string { return srv.URL })
	srv = httptest.NewServer(SourceManagerHandlerOr(ssm, nil, func(g grpc.ServiceRegistrar) { ssm.Register(g) }, nil))
	defer srv.Close()
	ctx := context.Background()
	parent := "projects/cb2-test/locations/us-central1"
	if _, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "i"}); err != nil {
		t.Fatal(err)
	}
	op, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "app",
		Repository: &ssmpb.Repository{Instance: parent + "/instances/i"}})
	repo := opResult(t, op, err, &ssmpb.Repository{})
	work := t.TempDir()
	git(t, work, "clone", "-q", repo.GetUris().GetGitHttps(), ".")
	git(t, work, "checkout", "-q", "-b", "main")
	git(t, work, "commit", "-q", "--allow-empty", "-m", "first")
	git(t, work, "tag", "v1")
	git(t, work, "push", "-q", "origin", "main", "v1")

	secrets := cb2Secrets{"projects/cb2-test/secrets/token/versions/1": "glpat-x", "projects/cb2-test/secrets/hook/versions/1": "h"}
	m := s.Builds(nil, nil).WithBuildSecrets(secrets, nil).RepositoryManager()
	gitlab := func(host, token string) *cb2pb.Connection {
		return &cb2pb.Connection{ConnectionConfig: &cb2pb.Connection_GitlabConfig{GitlabConfig: &cb2pb.GitLabConfig{
			HostUri: host, WebhookSecretSecretVersion: "projects/cb2-test/secrets/hook/versions/1",
			ReadAuthorizerCredential: &cb2pb.UserCredential{UserTokenSecretVersion: token},
			AuthorizerCredential:     &cb2pb.UserCredential{UserTokenSecretVersion: "projects/cb2-test/secrets/token/versions/1"}}}}
	}
	for name, tc := range map[string]struct {
		parent string
		c      *cb2pb.Connection
		code   codes.Code
		want   string
	}{
		"global":     {"projects/cb2-test/locations/global", gitlab(srv.URL, "projects/cb2-test/secrets/token/versions/1"), codes.InvalidArgument, "cannot be global"},
		"no config":  {parent, &cb2pb.Connection{}, codes.InvalidArgument, "connection config is required"},
		"gitlab.com": {parent, gitlab("", "projects/cb2-test/secrets/token/versions/1"), codes.FailedPrecondition, "not reachable"},
		"bad secret": {parent, gitlab(srv.URL, "token"), codes.InvalidArgument, "not a secret version"},
		"no secret":  {parent, gitlab(srv.URL, "projects/cb2-test/secrets/nope/versions/1"), codes.FailedPrecondition, "cannot access"},
	} {
		_, err := m.CreateConnection(ctx, &cb2pb.CreateConnectionRequest{Parent: tc.parent, ConnectionId: "x", Connection: tc.c})
		if status.Code(err) != tc.code || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v; want %v with %q", name, err, tc.code, tc.want)
		}
	}

	op, err = m.CreateConnection(ctx, &cb2pb.CreateConnectionRequest{Parent: parent, ConnectionId: "gl",
		Connection: gitlab(strings.Replace(srv.URL, "127.0.0.1", "localhost", 1), "projects/cb2-test/secrets/token/versions/1")})
	conn := opResult(t, op, err, &cb2pb.Connection{})
	if conn.GetInstallationState().GetStage() != cb2pb.InstallationState_COMPLETE || conn.GetEtag() == "" {
		t.Fatalf("connection = %v", conn)
	}
	op, err = m.CreateConnection(ctx, &cb2pb.CreateConnectionRequest{Parent: parent, ConnectionId: "gh",
		Connection: &cb2pb.Connection{ConnectionConfig: &cb2pb.Connection_GithubConfig{GithubConfig: &cb2pb.GitHubConfig{}}}})
	gh := opResult(t, op, err, &cb2pb.Connection{})
	if gh.GetInstallationState().GetStage() != cb2pb.InstallationState_PENDING_USER_OAUTH {
		t.Errorf("github.com connection stage = %v", gh.GetInstallationState())
	}
	if _, err := m.FetchLinkableRepositories(ctx, &cb2pb.FetchLinkableRepositoriesRequest{Connection: gh.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("linkable repositories of a pending connection: %v", err)
	}

	linkable, err := m.FetchLinkableRepositories(ctx, &cb2pb.FetchLinkableRepositoriesRequest{Connection: conn.GetName()})
	if err != nil || len(linkable.GetRepositories()) != 1 || linkable.GetRepositories()[0].GetRemoteUri() != repo.GetUris().GetGitHttps() {
		t.Fatalf("linkable = %v, %v", linkable, err)
	}
	if _, err := m.CreateRepository(ctx, &cb2pb.CreateRepositoryRequest{Parent: conn.GetName(), RepositoryId: "r",
		Repository: &cb2pb.Repository{RemoteUri: "https://gitlab.com/a/b.git"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a repository on another host: %v", err)
	}
	op, err = m.CreateRepository(ctx, &cb2pb.CreateRepositoryRequest{Parent: conn.GetName(), RepositoryId: "app",
		Repository: &cb2pb.Repository{RemoteUri: repo.GetUris().GetGitHttps()}})
	linked := opResult(t, op, err, &cb2pb.Repository{})
	if _, err := m.CreateRepository(ctx, &cb2pb.CreateRepositoryRequest{Parent: conn.GetName(), RepositoryId: "again",
		Repository: &cb2pb.Repository{RemoteUri: repo.GetUris().GetGitHttps()}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("linking a remote twice: %v", err)
	}
	for typ, want := range map[cb2pb.FetchGitRefsRequest_RefType]string{cb2pb.FetchGitRefsRequest_BRANCH: "main", cb2pb.FetchGitRefsRequest_TAG: "v1"} {
		refs, err := m.FetchGitRefs(ctx, &cb2pb.FetchGitRefsRequest{Repository: linked.GetName(), RefType: typ})
		if err != nil || len(refs.GetRefNames()) != 1 || refs.GetRefNames()[0] != want {
			t.Errorf("refs %v = %v, %v", typ, refs, err)
		}
	}
	tok, err := m.FetchReadToken(ctx, &cb2pb.FetchReadTokenRequest{Repository: linked.GetName()})
	if err != nil || !strings.HasPrefix(tok.GetToken(), dcTokenPrefix) || tok.GetExpirationTime() == nil {
		t.Fatalf("read token = %v, %v", tok, err)
	}
	clone := t.TempDir()
	bearer := "http.extraHeader=Authorization: Bearer " + tok.GetToken()
	git(t, clone, "-c", bearer, "clone", "-q", repo.GetUris().GetGitHttps(), ".")
	cmd := exec.Command("git", "-C", clone, "-c", bearer, "push", "-q", "origin", "HEAD:refs/heads/other")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("a read token pushed: %s", out)
	}

	if err := checkRepositoryTrigger(&cbpb.BuildTrigger{RepositoryEventConfig: &cbpb.RepositoryEventConfig{Repository: linked.GetName()}}, "europe-west1"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a trigger in another region: %v", err)
	}
	trig := &cbpb.BuildTrigger{RepositoryEventConfig: &cbpb.RepositoryEventConfig{Repository: linked.GetName()}}
	if err := checkRepositoryTrigger(trig, "us-central1"); err != nil || trig.GetRepositoryEventConfig().GetRepositoryType() != cbpb.RepositoryEventConfig_GITLAB_ENTERPRISE {
		t.Errorf("trigger check = %v, type %v", err, trig.GetRepositoryEventConfig().GetRepositoryType())
	}
	if got, err := devConnectLinkRepo(linked.GetName()); err != nil || got != repo.GetName() {
		t.Errorf("the linked repository names %q, %v", got, err)
	}

	// Disabling the connection stops its repositories' calls.
	op, err = m.UpdateConnection(ctx, &cb2pb.UpdateConnectionRequest{Connection: &cb2pb.Connection{Name: conn.GetName(), Disabled: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}})
	disabled := opResult(t, op, err, &cb2pb.Connection{})
	if !disabled.GetDisabled() || disabled.GetEtag() == conn.GetEtag() {
		t.Errorf("disabled = %v", disabled)
	}
	if _, err := devConnectLinkRepo(linked.GetName()); err == nil {
		t.Error("a disabled connection's repository still resolves")
	}
	if _, err := m.DeleteConnection(ctx, &cb2pb.DeleteConnectionRequest{Name: conn.GetName(), Etag: conn.GetEtag()}); status.Code(err) != codes.Aborted {
		t.Errorf("a stale etag: %v", err)
	}
	if _, err := m.DeleteConnection(ctx, &cb2pb.DeleteConnectionRequest{Name: conn.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetRepository(ctx, &cb2pb.GetRepositoryRequest{Name: linked.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("a deleted connection's repository: %v", err)
	}
}

func TestIsInstanceGitHost(t *testing.T) {
	ssmGitBase.Store(func() string { return "http://127.0.0.1:25080" })
	defer ssmGitBase.Store(func() string { return "" })
	for host, want := range map[string]bool{"http://127.0.0.1:25080": true, "http://localhost:25080/": true, "https://[::1]:25080": true,
		"http://127.0.0.1:25081": false, "https://gitlab.com": false, "http://127.0.0.1:25080/x": false} {
		if got := isInstanceGitHost(host); got != want {
			t.Errorf("isInstanceGitHost(%s) = %v", host, got)
		}
	}
}
