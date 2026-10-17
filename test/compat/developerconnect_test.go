//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	developerconnect "cloud.google.com/go/developerconnect/apiv1"
	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	cbrest "google.golang.org/api/cloudbuild/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.developerconnect.v1.DeveloperConnect/CreateConnection, google.cloud.developerconnect.v1.DeveloperConnect/GetConnection, google.cloud.developerconnect.v1.DeveloperConnect/ListConnections, google.cloud.developerconnect.v1.DeveloperConnect/UpdateConnection, google.cloud.developerconnect.v1.DeveloperConnect/DeleteConnection, google.cloud.developerconnect.v1.DeveloperConnect/CreateGitRepositoryLink, google.cloud.developerconnect.v1.DeveloperConnect/GetGitRepositoryLink, google.cloud.developerconnect.v1.DeveloperConnect/ListGitRepositoryLinks, google.cloud.developerconnect.v1.DeveloperConnect/DeleteGitRepositoryLink, google.cloud.developerconnect.v1.DeveloperConnect/FetchLinkableGitRepositories, google.cloud.developerconnect.v1.DeveloperConnect/FetchGitRefs
//
// TestDeveloperConnectSourceManagerTrigger (#1546), with the official
// Developer Connect, Secure Source Manager and Cloud Build clients over
// gRPC on the Run port, as Google's "Connect to Secure Source Manager"
// documents it: a connection with secureSourceManagerInstanceConfig names
// an instance of this instance's Secure Source Manager (in its region;
// another region is INVALID_ARGUMENT, a githubConfig UNIMPLEMENTED), is
// installed (COMPLETE), read, listed and updated; the instance's
// repositories are linkable, and a link to one by its git URL is made,
// read and listed. A push of a .cloudbuild/triggers.yaml whose trigger
// names the link in devConnectGitRepositoryLink runs its build; a trigger
// naming a missing link is skipped. FetchGitRefs lists the pushed branch.
// A connection with links is not deleted; an update or delete with a
// stale etag is ABORTED (#1595); the link, then the connection, are. The JSON API's BuildTrigger developerConnectEventConfig, sent with
// the official REST client, is UNIMPLEMENTED with the reason.
func TestDeveloperConnectSourceManagerTrigger(t *testing.T) {
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
	dc, err := developerconnect.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	cb := cloudBuildClient(ctx, t, h)
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "dc-" + suffix})
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
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	if _, err := dc.CreateConnection(ctx, &dcpb.CreateConnectionRequest{Parent: parent, ConnectionId: "gh-" + suffix,
		Connection: &dcpb.Connection{ConnectionConfig: &dcpb.Connection_GithubConfig{GithubConfig: &dcpb.GitHubConfig{}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateConnection with githubConfig: %v; want Unimplemented", err)
	}
	ssmConfig := &dcpb.Connection_SecureSourceManagerInstanceConfig{
		SecureSourceManagerInstanceConfig: &dcpb.SecureSourceManagerInstanceConfig{Instance: inst.GetName()}}
	if _, err := dc.CreateConnection(ctx, &dcpb.CreateConnectionRequest{Parent: "projects/" + h.Project() + "/locations/europe-west1",
		ConnectionId: "far-" + suffix, Connection: &dcpb.Connection{ConnectionConfig: ssmConfig}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateConnection in another region than the instance's: %v; want InvalidArgument", err)
	}
	cop, err := dc.CreateConnection(ctx, &dcpb.CreateConnectionRequest{Parent: parent, ConnectionId: "ssm-" + suffix,
		Connection: &dcpb.Connection{ConnectionConfig: ssmConfig}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	conn, err := cop.Wait(ctx)
	if err != nil || conn.GetInstallationState().GetStage() != dcpb.InstallationState_COMPLETE {
		t.Fatalf("the connection = %v, %v; want installed", conn, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for it := dc.ListGitRepositoryLinks(ctx, &dcpb.ListGitRepositoryLinksRequest{Parent: conn.GetName()}); ; {
			l, err := it.Next()
			if err != nil {
				break
			}
			if op, err := dc.DeleteGitRepositoryLink(ctx, &dcpb.DeleteGitRepositoryLinkRequest{Name: l.GetName()}); err == nil {
				_ = op.Wait(ctx)
			}
		}
		if op, err := dc.DeleteConnection(ctx, &dcpb.DeleteConnectionRequest{Name: conn.GetName()}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	if got, err := dc.GetConnection(ctx, &dcpb.GetConnectionRequest{Name: conn.GetName()}); err != nil ||
		got.GetSecureSourceManagerInstanceConfig().GetInstance() != inst.GetName() {
		t.Errorf("GetConnection = %v, %v", got, err)
	}
	listed := false
	for it := dc.ListConnections(ctx, &dcpb.ListConnectionsRequest{Parent: parent}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		listed = listed || x.GetName() == conn.GetName()
	}
	if !listed {
		t.Error("ListConnections does not list the connection")
	}
	uop, err := dc.UpdateConnection(ctx, &dcpb.UpdateConnectionRequest{Connection: &dcpb.Connection{Name: conn.GetName(),
		Labels: map[string]string{"team": "build"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		t.Fatalf("UpdateConnection: %v", err)
	}
	if got, err := uop.Wait(ctx); err != nil || got.GetLabels()["team"] != "build" {
		t.Errorf("UpdateConnection = %v, %v", got, err)
	}

	linkable := false
	for it := dc.FetchLinkableGitRepositories(ctx, &dcpb.FetchLinkableGitRepositoriesRequest{Connection: conn.GetName()}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		linkable = linkable || x.GetCloneUri() == repo.GetUris().GetGitHttps()
	}
	if !linkable {
		t.Error("FetchLinkableGitRepositories does not list the repository")
	}
	if _, err := dc.CreateGitRepositoryLink(ctx, &dcpb.CreateGitRepositoryLinkRequest{Parent: conn.GetName(), GitRepositoryLinkId: "bad",
		GitRepositoryLink: &dcpb.GitRepositoryLink{CloneUri: "https://github.com/octocat/hello.git"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateGitRepositoryLink to a hosted repository: %v; want InvalidArgument", err)
	}
	lop, err := dc.CreateGitRepositoryLink(ctx, &dcpb.CreateGitRepositoryLinkRequest{Parent: conn.GetName(), GitRepositoryLinkId: "link",
		GitRepositoryLink: &dcpb.GitRepositoryLink{CloneUri: repo.GetUris().GetGitHttps()}})
	if err != nil {
		t.Fatalf("CreateGitRepositoryLink: %v", err)
	}
	link, err := lop.Wait(ctx)
	if err != nil || link.GetCloneUri() != repo.GetUris().GetGitHttps() {
		t.Fatalf("the link = %v, %v", link, err)
	}
	if got, err := dc.GetGitRepositoryLink(ctx, &dcpb.GetGitRepositoryLinkRequest{Name: link.GetName()}); err != nil || got.GetUid() != link.GetUid() {
		t.Errorf("GetGitRepositoryLink = %v, %v", got, err)
	}
	linked := false
	for it := dc.ListGitRepositoryLinks(ctx, &dcpb.ListGitRepositoryLinksRequest{Parent: conn.GetName()}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		linked = linked || x.GetName() == link.GetName()
	}
	if !linked {
		t.Error("ListGitRepositoryLinks does not list the link")
	}

	work := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sa := "projects/" + h.Project() + "/serviceAccounts/builder@" + h.Project() + ".iam.gserviceaccount.com"
	git("clone", "--quiet", repo.GetUris().GetGitHttps(), ".")
	write(".cloudbuild/triggers.yaml", `triggers:
- name: linked
  eventType: push
  serviceAccount: `+sa+`
  devConnectGitRepositoryLink: `+link.GetName()+`
  substitutions:
    _VIA: devconnect
- name: unlinked
  eventType: push
  serviceAccount: `+sa+`
  devConnectGitRepositoryLink: `+conn.GetName()+`/gitRepositoryLinks/missing
`)
	write(".cloudbuild/cloudbuild.yaml", `steps:
- name: gcr.io/cloud-builders/docker
  args: [build, -t, 'us-central1-docker.pkg.dev/$PROJECT_ID/dc/${REPO_NAME}:${SHORT_SHA}-${_VIA}-${TRIGGER_NAME}', .]
`)
	write("Dockerfile", "FROM scratch\nCOPY hello.txt /hello.txt\n")
	write("hello.txt", "hello\n")
	git("checkout", "--quiet", "-b", "main")
	git("add", ".")
	git("commit", "--quiet", "-m", "first")
	git("push", "--quiet", "origin", "main")
	sha := git("rev-parse", "HEAD")

	byCommit := func(rev string) []*cbpb.Build {
		var out []*cbpb.Build
		it := cb.ListBuilds(ctx, &cbpb.ListBuildsRequest{Parent: parent, ProjectId: h.Project()})
		for {
			b, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("ListBuilds: %v", err)
			}
			if b.GetSource().GetGitSource().GetRevision() == rev {
				out = append(out, b)
			}
		}
	}
	var builds []*cbpb.Build
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if builds = byCommit(sha); len(builds) > 0 && builds[0].GetFinishTime() != nil {
			break
		}
	}
	want := "us-central1-docker.pkg.dev/" + h.Project() + "/dc/r-" + suffix + ":" + sha[:7] + "-devconnect-linked"
	if len(builds) != 1 || builds[0].GetStatus() != cbpb.Build_SUCCESS || builds[0].GetSteps()[0].GetArgs()[2] != want {
		t.Fatalf("the push's builds = %v; want one SUCCESS building %s (the trigger with a missing link skipped)", builds, want)
	}

	refs, err := dc.FetchGitRefs(ctx, &dcpb.FetchGitRefsRequest{GitRepositoryLink: link.GetName(), RefType: dcpb.FetchGitRefsRequest_BRANCH}).Next()
	if err != nil || refs != "main" {
		t.Errorf("FetchGitRefs BRANCH = %q, %v; want main", refs, err)
	}

	// A BuildTrigger over a link, through the JSON API's official client.
	svc, err := cbrest.NewService(ctx, option.WithEndpoint("http://"+h.Endpoint(EnvRun)+"/"), option.WithoutAuthentication(),
		option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Projects.Locations.Triggers.Create(parent, &cbrest.BuildTrigger{Name: "dc-" + suffix,
		DeveloperConnectEventConfig: &cbrest.DeveloperConnectEventConfig{GitRepositoryLink: link.GetName(),
			Push: &cbrest.PushFilter{Branch: "^main$"}},
		Build: &cbrest.Build{Steps: []*cbrest.BuildStep{{Name: "ubuntu", Args: []string{"true"}}}}}).Context(ctx).Do()
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusNotImplemented || !strings.Contains(gerr.Message, "devConnectGitRepositoryLink") {
		t.Errorf("CreateBuildTrigger with developerConnectEventConfig: %v; want 501 naming devConnectGitRepositoryLink", err)
	}

	if _, err := dc.DeleteConnection(ctx, &dcpb.DeleteConnectionRequest{Name: conn.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteConnection with a link: %v; want FailedPrecondition", err)
	}
	// An etag that is not the resource's is ABORTED, as
	// developer_connect.proto documents (#1595); reads carry none.
	if link.GetEtag() != "" || conn.GetEtag() != "" {
		t.Errorf("etags: link %q, connection %q; want none (Terraform's schema, #1304)", link.GetEtag(), conn.GetEtag())
	}
	if _, err := dc.DeleteGitRepositoryLink(ctx, &dcpb.DeleteGitRepositoryLinkRequest{Name: link.GetName(), Etag: "stale"}); status.Code(err) != codes.Aborted {
		t.Errorf("DeleteGitRepositoryLink with a stale etag: %v; want Aborted", err)
	}
	if _, err := dc.DeleteConnection(ctx, &dcpb.DeleteConnectionRequest{Name: conn.GetName(), Etag: "stale"}); status.Code(err) != codes.Aborted {
		t.Errorf("DeleteConnection with a stale etag: %v; want Aborted", err)
	}
	if _, err := dc.UpdateConnection(ctx, &dcpb.UpdateConnectionRequest{Connection: &dcpb.Connection{Name: conn.GetName(), Etag: "stale", Disabled: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}}); status.Code(err) != codes.Aborted {
		t.Errorf("UpdateConnection with a stale etag: %v; want Aborted", err)
	}
	dop, err := dc.DeleteGitRepositoryLink(ctx, &dcpb.DeleteGitRepositoryLinkRequest{Name: link.GetName(), Etag: link.GetEtag()})
	if err != nil {
		t.Fatalf("DeleteGitRepositoryLink: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteGitRepositoryLink: %v", err)
	}
	if _, err := dc.GetGitRepositoryLink(ctx, &dcpb.GetGitRepositoryLinkRequest{Name: link.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetGitRepositoryLink after the delete: %v; want NotFound", err)
	}
	xop, err := dc.DeleteConnection(ctx, &dcpb.DeleteConnectionRequest{Name: conn.GetName()})
	if err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}
	if err := xop.Wait(ctx); err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}
	var left []string
	for it := dc.ListConnections(ctx, &dcpb.ListConnectionsRequest{Parent: parent}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		left = append(left, x.GetName())
	}
	if slices.Contains(left, conn.GetName()) {
		t.Error("ListConnections lists the deleted connection")
	}
}

// TestGcloudDeveloperConnect (#1546): `gcloud developer-connect connections
// create --secure-source-manager-instance-config`, as Google's "Connect to
// Secure Source Manager" gives it, then describe and list, and
// `git-repository-links create --clone-uri`, describe, list and delete,
// and the connection's delete, through the configuration `cloudburrow
// gcloud-setup` writes (its developerconnect override, on the Run port),
// egress blocked; the official client reads what gcloud made.
func TestGcloudDeveloperConnect(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	ssm := ssmClient(t, h)
	dc, err := developerconnect.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	parent := "projects/" + g.project + "/locations/us-central1"
	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "gdc-" + suffix})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r-" + suffix,
		Repository: &ssmpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn := "c-" + suffix
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := dc.DeleteGitRepositoryLink(ctx, &dcpb.DeleteGitRepositoryLinkRequest{Name: parent + "/connections/" + conn + "/gitRepositoryLinks/l"}); err == nil {
			_ = op.Wait(ctx)
		}
		if op, err := dc.DeleteConnection(ctx, &dcpb.DeleteConnectionRequest{Name: parent + "/connections/" + conn}); err == nil {
			_ = op.Wait(ctx)
		}
	})

	g.must("developer-connect", "connections", "create", conn, "--location=us-central1",
		"--secure-source-manager-instance-config="+inst.GetName())
	if out := g.must("developer-connect", "connections", "describe", conn, "--location=us-central1",
		"--format=value(installationState.stage)"); lastLine(out) != "COMPLETE" {
		t.Errorf("connections describe: stage = %q", out)
	}
	if out := g.must("developer-connect", "connections", "list", "--location=us-central1", "--format=value(name)"); !strings.Contains(out, conn) {
		t.Errorf("connections list does not list the connection:\n%s", out)
	}
	g.must("developer-connect", "connections", "git-repository-links", "create", "l", "--connection="+conn,
		"--location=us-central1", "--clone-uri="+repo.GetUris().GetGitHttps())
	if out := g.must("developer-connect", "connections", "git-repository-links", "describe", "l", "--connection="+conn,
		"--location=us-central1", "--format=value(cloneUri)"); lastLine(out) != repo.GetUris().GetGitHttps() {
		t.Errorf("git-repository-links describe: cloneUri = %q", out)
	}
	if out := g.must("developer-connect", "connections", "git-repository-links", "list", "--connection="+conn,
		"--location=us-central1", "--format=value(name)"); !strings.Contains(out, "/gitRepositoryLinks/l") {
		t.Errorf("git-repository-links list does not list the link:\n%s", out)
	}
	if l, err := dc.GetGitRepositoryLink(ctx, &dcpb.GetGitRepositoryLinkRequest{Name: parent + "/connections/" + conn + "/gitRepositoryLinks/l"}); err != nil ||
		l.GetCloneUri() != repo.GetUris().GetGitHttps() {
		t.Errorf("the link gcloud made = %v, %v", l, err)
	}
	g.must("developer-connect", "connections", "git-repository-links", "delete", "l", "--connection="+conn, "--location=us-central1", "--quiet")
	g.must("developer-connect", "connections", "delete", conn, "--location=us-central1", "--quiet")
	if _, err := dc.GetConnection(ctx, &dcpb.GetConnectionRequest{Name: parent + "/connections/" + conn}); status.Code(err) != codes.NotFound {
		t.Errorf("GetConnection after connections delete: %v; want NotFound", err)
	}
}
