package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGcloudSetupWritesOnlyItsOwnConfiguration(t *testing.T) {
	testGcloudSetup(t, t.TempDir(), t.TempDir())
}

// The configuration holds file paths (credential_file_override) beside the
// endpoints, and a temporary directory's random name can contain a port's
// digits, as ".../TestGcloudSetupWritesOnlyItsOwnConfiguration2432190042/"
// did (#921). The endpoint checks compare parsed values, so such a path
// neither fails nor satisfies them.
func TestGcloudSetupEndpointChecksIgnorePortDigitsInPaths(t *testing.T) {
	gdir := filepath.Join(t.TempDir(), "gcloud-9004")
	stateDir := filepath.Join(t.TempDir(), "state-9004-9001")
	conf := testGcloudSetup(t, gdir, stateDir)
	// The paths really do carry the digits, so a substring check on "9004"
	// would have failed here.
	if !strings.Contains(conf, "9004") {
		t.Fatalf("the configuration does not contain the state dir path:\n%s", conf)
	}
}

// parseGcloudConfig reads gcloud's INI format into section -> key -> value.
func parseGcloudConfig(t *testing.T, conf string) map[string]map[string]string {
	t.Helper()
	sections := map[string]map[string]string{}
	var cur map[string]string
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			name := line[1 : len(line)-1]
			if sections[name] == nil {
				sections[name] = map[string]string{}
			}
			cur = sections[name]
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || cur == nil {
				t.Fatalf("unparsable configuration line %q:\n%s", line, conf)
			}
			cur[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return sections
}

// testGcloudSetup runs gcloud-setup and teardown with gcloud's configuration
// in gdir and the instance state in stateDir, and returns the configuration
// it wrote.
func testGcloudSetup(t *testing.T, gdir, stateDir string) string {
	t.Helper()
	t.Setenv("CLOUDSDK_CONFIG", gdir)
	// A user's existing gcloud state, which must not change.
	_ = os.MkdirAll(filepath.Join(gdir, "configurations"), 0o755)
	def := filepath.Join(gdir, "configurations", "config_default")
	_ = os.WriteFile(def, []byte("[core]\nproject = my-real-project\n"), 0o644)
	active := filepath.Join(gdir, "active_config")
	_ = os.WriteFile(active, []byte("default"), 0o644)

	args := []string{"--name", "gcs", "--state-dir", stateDir, "--services", "storage,pubsub,tasks,secretmanager,scheduler,run"}
	var out, errOut bytes.Buffer
	if err := runGcloudSetup(args, &out, &errOut); err != nil {
		t.Fatalf("gcloud-setup: %v\n%s", err, errOut.String())
	}
	if got := out.String(); got != "export CLOUDSDK_ACTIVE_CONFIG_NAME=cloudburrow-gcs\n" {
		t.Errorf("stdout = %q", got)
	}
	b, err := os.ReadFile(filepath.Join(gdir, "configurations", "config_cloudburrow-gcs"))
	if err != nil {
		t.Fatal(err)
	}
	conf := string(b)
	sections := parseGcloudConfig(t, conf)
	// Exactly the Verified gcloud endpoints: Cloud Run is enabled and not
	// written, since gcloud run calls the v1 API (#591); Resource Manager is
	// always served, so written whatever --services selects (#682).
	wantOverrides := map[string]string{
		"storage":              "http://127.0.0.1:9001/storage/v1/",
		"pubsub":               "http://127.0.0.1:9002/",
		"cloudtasks":           "http://127.0.0.1:9003/",
		"secretmanager":        "http://127.0.0.1:9006/",
		"cloudresourcemanager": "http://127.0.0.1:9007/",
		"cloudscheduler":       "http://127.0.0.1:9008/",
	}
	if got := sections["api_endpoint_overrides"]; !reflect.DeepEqual(got, wantOverrides) {
		t.Errorf("api_endpoint_overrides = %v, want %v:\n%s", got, wantOverrides, conf)
	}
	if p := sections["core"]["project"]; p == "" {
		t.Errorf("configuration sets no core/project:\n%s", conf)
	}
	auth := sections["auth"]
	if auth["disable_credentials"] != "true" {
		t.Errorf("auth/disable_credentials = %q:\n%s", auth["disable_credentials"], conf)
	}
	if f := auth["credential_file_override"]; !strings.HasPrefix(f, stateDir+string(filepath.Separator)) {
		t.Errorf("auth/credential_file_override = %q, want a file under %s", f, stateDir)
	}
	if d, _ := os.ReadFile(def); string(d) != "[core]\nproject = my-real-project\n" {
		t.Errorf("the default configuration changed: %q", d)
	}
	if a, _ := os.ReadFile(active); string(a) != "default" {
		t.Errorf("active_config changed: %q", a)
	}

	// Teardown removes it, and a second teardown is harmless.
	for i := 0; i < 2; i++ {
		out.Reset()
		if err := runGcloudTeardown(args, &out, &errOut); err != nil {
			t.Fatalf("teardown %d: %v", i+1, err)
		}
		if out.String() != "unset CLOUDSDK_ACTIVE_CONFIG_NAME\n" {
			t.Errorf("teardown stdout = %q", out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(gdir, "configurations", "config_cloudburrow-gcs")); !os.IsNotExist(err) {
		t.Errorf("the configuration survived teardown: %v", err)
	}
	if _, err := os.Stat(def); err != nil {
		t.Errorf("teardown touched the default configuration: %v", err)
	}
	return conf
}

// A configuration of the same name the user wrote themselves is never
// overwritten or deleted.
func TestGcloudSetupLeavesAForeignConfigurationAlone(t *testing.T) {
	gdir := t.TempDir()
	t.Setenv("CLOUDSDK_CONFIG", gdir)
	p := filepath.Join(gdir, "configurations", "config_cloudburrow-mine")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("[core]\nproject = hand-made\n"), 0o644)
	args := []string{"--name", "mine", "--state-dir", t.TempDir()}
	var out, errOut bytes.Buffer
	if err := runGcloudSetup(args, &out, &errOut); err == nil {
		t.Error("setup overwrote a configuration it did not write")
	}
	if err := runGcloudTeardown(args, &out, &errOut); err == nil {
		t.Error("teardown removed a configuration it did not write")
	}
	if b, _ := os.ReadFile(p); string(b) != "[core]\nproject = hand-made\n" {
		t.Errorf("the hand-made configuration changed: %q", b)
	}
}
