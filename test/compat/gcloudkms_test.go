//go:build compat

package compat

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
)

// gcloudKMS configures an isolated gcloud with `cloudburrow gcloud-setup` and
// returns a runner that sets only CLOUDSDK_ACTIVE_CONFIG_NAME, so everything
// else comes from that configuration, and the instance's project. It skips
// without gcloud, the CLI or the KMS endpoint.
func gcloudKMS(t *testing.T, h *Harness) (func(args ...string) (string, error), string) {
	t.Helper()
	gcloud, err := exec.LookPath("gcloud")
	if err != nil {
		t.Skip("gcloud is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	h.Endpoint(EnvKMS) // skips without CLOUDBURROW_TEST_KMS
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	out, _ := exec.Command(cli, append([]string{"status", "--format", "json"}, flags...)...).Output()
	var st struct{ Project string }
	if err := json.Unmarshal(out, &st); err != nil || st.Project == "" {
		t.Fatalf("status gave no project: %s", out)
	}
	gdir := t.TempDir()
	setup := exec.Command(cli, append([]string{"gcloud-setup"}, flags...)...)
	setup.Env = append(os.Environ(), "CLOUDSDK_CONFIG="+gdir)
	b, err := setup.Output()
	if err != nil {
		t.Fatalf("gcloud-setup: %v", err)
	}
	name, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "export CLOUDSDK_ACTIVE_CONFIG_NAME=")
	if !ok {
		t.Fatalf("gcloud-setup printed %q", b)
	}
	if conf, _ := os.ReadFile(filepath.Join(gdir, "configurations", "config_"+name)); !strings.Contains(string(conf), "cloudkms = http://") {
		t.Fatalf("the configuration has no cloudkms override:\n%s", conf)
	}
	return func(args ...string) (string, error) {
		cmd := exec.Command(gcloud, append(args, "--quiet")...)
		cmd.Env = append(os.Environ(), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}, st.Project
}

// TestGcloudKMS (#426): gcloud kms, configured by `cloudburrow gcloud-setup`
// alone, creates and lists key rings, keys and versions, moves versions
// through disable, enable, destroy and restore, and sets the primary. gcloud
// uses the cloudkms/v1 JSON API for every kms command. After each mutating
// command, the official gRPC client checks the state.
func TestGcloudKMS(t *testing.T) {
	h := New(t)
	run, project := gcloudKMS(t, h)
	c := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	state := func(v string) *kmspb.CryptoKeyVersion {
		t.Helper()
		got, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: v})
		if err != nil {
			t.Fatalf("GetCryptoKeyVersion %s: %v", v, err)
		}
		return got
	}

	ringID := "gc-" + h.Project()
	ring := "projects/" + project + "/locations/global/keyRings/" + ringID
	key := ring + "/cryptoKeys/k"
	v1, v2 := key+"/cryptoKeyVersions/1", key+"/cryptoKeyVersions/2"
	loc := []string{"--location", "global"}
	kr := append([]string{"--keyring", ringID}, loc...)

	gc(append([]string{"kms", "keyrings", "create", ringID}, loc...)...)
	if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring}); err != nil {
		t.Fatalf("the ring gcloud created: %v", err)
	}
	if got := gc(append([]string{"kms", "keyrings", "list", "--format=value(name)"}, loc...)...); !strings.Contains(got, ring) {
		t.Errorf("keyrings list does not show %s:\n%s", ring, got)
	}

	gc(append([]string{"kms", "keys", "create", "k", "--purpose", "encryption", "--labels", "env=local"}, kr...)...)
	if got := gc(append([]string{"kms", "keys", "list", "--format=value(name)"}, kr...)...); !strings.Contains(got, key) {
		t.Errorf("keys list does not show %s:\n%s", key, got)
	}
	var desc struct {
		Purpose         string            `json:"purpose"`
		Labels          map[string]string `json:"labels"`
		VersionTemplate struct {
			Algorithm string `json:"algorithm"`
		} `json:"versionTemplate"`
	}
	if err := json.Unmarshal([]byte(gc(append([]string{"kms", "keys", "describe", "k", "--format=json"}, kr...)...)), &desc); err != nil {
		t.Fatalf("keys describe is not JSON: %v", err)
	}
	if desc.Purpose != "ENCRYPT_DECRYPT" || desc.VersionTemplate.Algorithm != "GOOGLE_SYMMETRIC_ENCRYPTION" || desc.Labels["env"] != "local" {
		t.Errorf("keys describe = %+v", desc)
	}

	kk := append([]string{"--key", "k"}, kr...)
	gc(append([]string{"kms", "keys", "versions", "create"}, kk...)...)
	if got := gc(append([]string{"kms", "keys", "versions", "list", "--format=value(name)"}, kk...)...); !strings.Contains(got, v1) || !strings.Contains(got, v2) {
		t.Errorf("versions list does not show versions 1 and 2:\n%s", got)
	}
	if k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); err != nil || k.GetPrimary().GetName() != v1 {
		t.Errorf("primary after versions create = %v, %v; want version 1", k.GetPrimary().GetName(), err)
	}

	gc(append([]string{"kms", "keys", "versions", "disable", "2"}, kk...)...)
	if s := state(v2).GetState(); s != kmspb.CryptoKeyVersion_DISABLED {
		t.Errorf("after disable: %v", s)
	}
	gc(append([]string{"kms", "keys", "versions", "enable", "2"}, kk...)...)
	if s := state(v2).GetState(); s != kmspb.CryptoKeyVersion_ENABLED {
		t.Errorf("after enable: %v", s)
	}
	gc(append([]string{"kms", "keys", "versions", "destroy", "2"}, kk...)...)
	if v := state(v2); v.GetState() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED || v.GetDestroyTime() == nil {
		t.Errorf("after destroy: %v, destroy_time %v", v.GetState(), v.GetDestroyTime())
	}
	gc(append([]string{"kms", "keys", "versions", "restore", "2"}, kk...)...)
	if s := state(v2).GetState(); s != kmspb.CryptoKeyVersion_DISABLED {
		t.Errorf("after restore: %v, want DISABLED", s)
	}
	gc(append([]string{"kms", "keys", "set-primary-version", "k", "--version", "1"}, kr...)...)
	if k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); err != nil || k.GetPrimary().GetName() != v1 {
		t.Errorf("primary after set-primary-version 1 = %v, %v", k.GetPrimary().GetName(), err)
	}
}
