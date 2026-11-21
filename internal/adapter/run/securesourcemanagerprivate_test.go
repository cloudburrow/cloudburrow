package run

import (
	"context"
	"strings"
	"testing"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSourceManagerPrivateInstance: a private instance is stored with its
// CA pool and custom domains and answers its service attachments (#1856).
func TestSourceManagerPrivateInstance(t *testing.T) {
	if err := UseSourceManagerRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s := NewServer(&Knative{}, "t", time.Minute)
	m := s.SecureSourceManager(func() string { return "http://127.0.0.1:1" })
	ctx := context.Background()
	parent := "projects/ssm-priv/locations/us-central1"

	for _, bad := range []*ssmpb.Instance_PrivateConfig{
		{IsPrivate: true, CaPool: "pool-1"},
		{IsPrivate: false, CaPool: "projects/p/locations/us-central1/caPools/c"},
		{IsPrivate: true, CustomHostConfig: &ssmpb.Instance_PrivateConfig_CustomHostConfig{Html: "not a host"}},
	} {
		_, err := m.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "bad",
			Instance: &ssmpb.Instance{PrivateConfig: bad}})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("private_config %v: %v; want InvalidArgument", bad, err)
		}
	}

	pc := &ssmpb.Instance_PrivateConfig{IsPrivate: true, CaPool: "projects/p/locations/us-central1/caPools/c",
		HttpServiceAttachment: "sent-by-caller",
		CustomHostConfig:      &ssmpb.Instance_PrivateConfig_CustomHostConfig{Html: "source.example.com", GitHttp: "git.source.example.com"}}
	if _, err := m.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "p1",
		Instance: &ssmpb.Instance{PrivateConfig: pc}}); err != nil {
		t.Fatal(err)
	}
	in, err := m.GetInstance(ctx, &ssmpb.GetInstanceRequest{Name: parent + "/instances/p1"})
	if err != nil {
		t.Fatal(err)
	}
	got := in.GetPrivateConfig()
	if !got.GetIsPrivate() || got.GetCaPool() != pc.CaPool || got.GetCustomHostConfig().GetHtml() != "source.example.com" {
		t.Fatalf("private_config = %v", got)
	}
	for _, sa := range []string{got.GetHttpServiceAttachment(), got.GetSshServiceAttachment()} {
		if !strings.HasPrefix(sa, "projects/ssm-tp-") || !strings.Contains(sa, "/regions/us-central1/serviceAttachments/p1-") {
			t.Fatalf("service attachment %q", sa)
		}
	}
	if in.GetHostConfig().GetGitHttp() != "127.0.0.1:1" {
		t.Fatalf("a private instance is served on this port: host_config %v", in.GetHostConfig())
	}

	// Without a CA pool, Google's managed certificate.
	if _, err := m.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "p2",
		Instance: &ssmpb.Instance{PrivateConfig: &ssmpb.Instance_PrivateConfig{IsPrivate: true}}}); err != nil {
		t.Fatal(err)
	}
}
