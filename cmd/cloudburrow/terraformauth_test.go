package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

func terraformAuthConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Name = "tfauth"
	cfg.BindAddress = "127.0.0.1"
	cfg.Endpoints.Metadata = 24321
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// TestTerraformAuthWithoutEnforcementIsUnchanged: the provider blocks still
// present "owner", and an override still clears credentials.
func TestTerraformAuthWithoutEnforcementIsUnchanged(t *testing.T) {
	cfg := config.Default()
	set, _ := terraformSettings(cfg)
	for _, override := range []bool{false, true} {
		a := terraformProviderBlock(cfg, "google", override, set)
		b := terraformProviderBlockAuth(cfg, "google", override, set, terraformOwnerAuth)
		if a != b || !strings.Contains(a, `access_token = "owner"`) {
			t.Fatalf("override=%v: block changed or lost the owner token:\n%s", override, a)
		}
	}
}

// TestTerraformEnforcedAuth: under enforcement the provider signs in with
// the user's access token, else a local service account key, else the
// instance's own fixture; keys that would sign in at Google, other file
// types and impersonation are refused.
func TestTerraformEnforcedAuth(t *testing.T) {
	cfg := terraformAuthConfig(t)
	dir := t.TempDir()
	local := `{"type":"service_account","client_email":"tf@p.iam.gserviceaccount.com","token_uri":"http://127.0.0.1:24321/token"}`
	keyPath := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(keyPath, []byte(local), 0o600); err != nil {
		t.Fatal(err)
	}

	a, done, err := terraformEnforcedAuth(cfg, dir, envOf(map[string]string{"GOOGLE_OAUTH_ACCESS_TOKEN": "cbl_x", "GOOGLE_CREDENTIALS": keyPath}))
	if err != nil || a.accessToken != "cbl_x" || a.credentials != "" {
		t.Fatalf("access token: %+v %v", a, err)
	}
	done()

	a, done, err = terraformEnforcedAuth(cfg, dir, envOf(map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": keyPath}))
	if err != nil || a.credentials != keyPath {
		t.Fatalf("key path: %+v %v", a, err)
	}
	done()
	set, _ := terraformSettings(cfg)
	block := terraformProviderBlockAuth(cfg, "google", true, set, a)
	if !strings.Contains(block, `credentials  = "`+keyPath+`"`) || !strings.Contains(block, "access_token = null") || strings.Contains(block, `"owner"`) {
		t.Fatalf("override block with a key:\n%s", block)
	}

	a, done, err = terraformEnforcedAuth(cfg, dir, envOf(map[string]string{"GOOGLE_CREDENTIALS": local}))
	if err != nil || !strings.HasPrefix(a.credentials, cfg.InstanceDir()) {
		t.Fatalf("key JSON: %+v %v", a, err)
	}
	if b, _ := os.ReadFile(a.credentials); string(b) != local {
		t.Fatalf("key JSON file holds %q", b)
	}
	done()
	if _, err := os.Stat(a.credentials); !os.IsNotExist(err) {
		t.Fatalf("key JSON file left behind: %v", err)
	}

	a, done, err = terraformEnforcedAuth(cfg, dir, envOf(nil))
	if err != nil || a.credentials == "" {
		t.Fatalf("fixture: %+v %v", a, err)
	}
	done()
	b, err := os.ReadFile(a.credentials)
	if err != nil || checkLocalKeyFile(cfg, b) != nil {
		t.Fatalf("the fixture is not a local key: %v %v", err, checkLocalKeyFile(cfg, b))
	}

	for name, body := range map[string]string{
		"google token_uri": `{"type":"service_account","token_uri":"https://oauth2.googleapis.com/token"}`,
		"another port":     `{"type":"service_account","token_uri":"http://127.0.0.1:9/token"}`,
		"authorized_user":  `{"type":"authorized_user","refresh_token":"r"}`,
		"external_account": `{"type":"external_account","token_url":"https://sts.googleapis.com/v1/token"}`,
		"impersonated":     `{"type":"impersonated_service_account"}`,
	} {
		if _, _, err := terraformEnforcedAuth(cfg, dir, envOf(map[string]string{"GOOGLE_CREDENTIALS": body})); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, _, err := terraformEnforcedAuth(cfg, dir, envOf(map[string]string{"GOOGLE_IMPERSONATE_SERVICE_ACCOUNT": "sa@p.iam.gserviceaccount.com"})); err == nil {
		t.Error("GOOGLE_IMPERSONATE_SERVICE_ACCOUNT was accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("provider \"google\" {\n  impersonate_service_account = \"sa@p.iam.gserviceaccount.com\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := terraformEnforcedAuth(cfg, dir, envOf(nil)); err == nil {
		t.Error("a module's impersonate_service_account was accepted")
	}
}
