//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.securesourcemanager.v1.SecureSourceManager/CreateInstance, google.cloud.securesourcemanager.v1.SecureSourceManager/GetInstance
//
// TestSecureSourceManagerPrivateInstance (#1856), with the official client
// over gRPC on the Run port, as Google's "Create a Private Service Connect
// instance" page documents it: private_config.is_private with a ca_pool
// and a custom_host_config is created ACTIVE, and GetInstance answers them
// with its httpServiceAttachment and sshServiceAttachment; one without a
// ca_pool (a Google-managed certificate) is created too; a ca_pool that is
// not a CA pool's name, or one without is_private, is INVALID_ARGUMENT.
// The instance is served on the Run port as any other: a repository on it
// is created.
func TestSecureSourceManagerPrivateInstance(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	c, err := securesourcemanager.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	project := strings.Split(parent, "/")[1]
	pool := "projects/" + project + "/locations/" + strings.Split(parent, "/")[3] + "/caPools/source"

	for _, bad := range []*ssmpb.Instance_PrivateConfig{{IsPrivate: true, CaPool: "source"}, {CaPool: pool}} {
		_, err := c.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "bad-" + suffix,
			Instance: &ssmpb.Instance{PrivateConfig: bad}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateInstance with private_config %v: %v; want InvalidArgument", bad, err)
		}
	}

	create := func(id string, pc *ssmpb.Instance_PrivateConfig) *ssmpb.Instance {
		t.Helper()
		op, err := c.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &ssmpb.Instance{PrivateConfig: pc}})
		if err != nil {
			t.Fatalf("CreateInstance %s: %v", id, err)
		}
		in, err := op.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if op, err := c.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: in.GetName(), Force: true}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
		return in
	}
	in := create("p-"+suffix, &ssmpb.Instance_PrivateConfig{IsPrivate: true, CaPool: pool,
		CustomHostConfig: &ssmpb.Instance_PrivateConfig_CustomHostConfig{Html: "source.example.com", Api: "api.source.example.com",
			GitHttp: "git.source.example.com", GitSsh: "ssh.source.example.com"}})
	got, err := c.GetInstance(ctx, &ssmpb.GetInstanceRequest{Name: in.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	pc := got.GetPrivateConfig()
	if got.GetState() != ssmpb.Instance_ACTIVE || !pc.GetIsPrivate() || pc.GetCaPool() != pool ||
		pc.GetCustomHostConfig().GetGitSsh() != "ssh.source.example.com" {
		t.Fatalf("GetInstance of a private instance = %v", got)
	}
	for _, sa := range []string{pc.GetHttpServiceAttachment(), pc.GetSshServiceAttachment()} {
		if !strings.Contains(sa, "/serviceAttachments/") || pc.GetHttpServiceAttachment() == pc.GetSshServiceAttachment() {
			t.Errorf("service attachments %q and %q", pc.GetHttpServiceAttachment(), pc.GetSshServiceAttachment())
		}
	}
	if got.GetHostConfig().GetGitHttp() == "" {
		t.Errorf("a private instance has no host_config: %v", got.GetHostConfig())
	}
	op, err := c.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r-" + suffix,
		Repository: &ssmpb.Repository{Instance: in.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository on a private instance: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	managed := create("m-"+suffix, &ssmpb.Instance_PrivateConfig{IsPrivate: true})
	if !managed.GetPrivateConfig().GetIsPrivate() || managed.GetPrivateConfig().GetCaPool() != "" || managed.GetPrivateConfig().GetHttpServiceAttachment() == "" {
		t.Errorf("a private instance with a Google-managed certificate = %v", managed.GetPrivateConfig())
	}
}
