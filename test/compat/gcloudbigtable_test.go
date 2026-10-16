//go:build compat

package compat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGcloudBigtableAppProfiles (#1440), in the emulators shard: gcloud
// bigtable app-profiles, configured by `cloudburrow gcloud-setup` alone
// (its bigtableadmin override), creates, lists, describes, updates and
// deletes an app profile on an instance the official instance admin client
// made, egress to Google blocked. gcloud calls the bigtableadmin v2 JSON
// API, which the Bigtable front serves beside gRPC; after each change the
// official gRPC client reads the profile back.
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/CreateAppProfile, google.bigtable.admin.v2.BigtableInstanceAdmin/ListAppProfiles, google.bigtable.admin.v2.BigtableInstanceAdmin/GetAppProfile, google.bigtable.admin.v2.BigtableInstanceAdmin/UpdateAppProfile, google.bigtable.admin.v2.BigtableInstanceAdmin/DeleteAppProfile
func TestGcloudBigtableAppProfiles(t *testing.T) {
	h := New(t)
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	iac := bigtableInstanceAdmin(t, h) // skips without CLOUDBURROW_TEST_BIGTABLE
	ctx := h.Context()
	gdir := t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	if conf, _ := os.ReadFile(filepath.Join(gdir, "configurations", "config_"+name)); !strings.Contains(string(conf), "bigtableadmin = http://") {
		t.Fatalf("the configuration has no bigtableadmin override:\n%s", conf)
	}
	gc := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gcloud, append(args, "--project", h.Project(), "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}

	const id, cluster = "gc-bt-profiles", "gc-bt-profiles-c1"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "gcloud profiles",
		ClusterId: cluster, Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })

	gc("bigtable", "app-profiles", "create", "gc-profile", "--instance", id, "--route-to", cluster,
		"--transactional-writes", "--description", "from gcloud")
	p, err := iac.GetAppProfile(ctx, id, "gc-profile")
	if err != nil {
		t.Fatalf("the profile gcloud created: %v", err)
	}
	if p.GetDescription() != "from gcloud" || p.GetSingleClusterRouting().GetClusterId() != cluster || !p.GetSingleClusterRouting().GetAllowTransactionalWrites() {
		t.Errorf("the profile gcloud created = %v", p)
	}
	if got := gc("bigtable", "app-profiles", "list", "--instance", id, "--format=value(name)"); !strings.Contains(got, "gc-profile") || !strings.Contains(got, "default") {
		t.Errorf("app-profiles list does not show gc-profile and default:\n%s", got)
	}
	if got := gc("bigtable", "app-profiles", "describe", "gc-profile", "--instance", id, "--format=value(description)"); strings.TrimSpace(got) != "from gcloud" {
		t.Errorf("app-profiles describe = %q", got)
	}

	gc("bigtable", "app-profiles", "update", "gc-profile", "--instance", id, "--route-any", "--description", "updated by gcloud", "--force")
	p, err = iac.GetAppProfile(ctx, id, "gc-profile")
	if err != nil {
		t.Fatalf("the profile gcloud updated: %v", err)
	}
	if p.GetDescription() != "updated by gcloud" || p.GetMultiClusterRoutingUseAny() == nil {
		t.Errorf("the profile gcloud updated = %v", p)
	}

	gc("bigtable", "app-profiles", "delete", "gc-profile", "--instance", id, "--force")
	if _, err := iac.GetAppProfile(ctx, id, "gc-profile"); status.Code(err) != codes.NotFound {
		t.Errorf("GetAppProfile after gcloud's delete: %v; want NotFound", err)
	}
}
