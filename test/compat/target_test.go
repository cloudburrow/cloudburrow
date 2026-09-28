package compat

import (
	"encoding/json"
	"strings"
	"testing"
)

// The unit tests for the target check (#927). They carry no build tag, so
// `go test ./...` runs them without an instance.

// targetFixture is what `env` and `status --format json` print for an
// instance "mine" on ports 57xxx, and the harness variables compat-env.sh
// would set for it.
func targetFixture(t *testing.T) (harness map[string]string, envJSON, statusJSON []byte) {
	t.Helper()
	env := map[string]string{
		"GOOGLE_APPLICATION_CREDENTIALS":     "/state/mine/credentials.json",
		"GCE_METADATA_HOST":                  "127.0.0.1:57005",
		"STORAGE_EMULATOR_HOST":              "http://127.0.0.1:57003",
		"PUBSUB_EMULATOR_HOST":               "127.0.0.1:57004",
		"CLOUDBURROW_TASKS_ENDPOINT":         "127.0.0.1:57006",
		"CLOUDBURROW_SECRETMANAGER_ENDPOINT": "127.0.0.1:57007",
		"REDIS_HOST":                         "127.0.0.1",
		"REDIS_PORT":                         "57016",
	}
	var err error
	if envJSON, err = json.Marshal(env); err != nil {
		t.Fatal(err)
	}
	statusJSON = []byte(`{"instance": "mine", "control_url": "http://127.0.0.1:57001", "console_url": "http://127.0.0.1:57090"}`)
	harness = map[string]string{
		"CREDENTIALS": "/state/mine/credentials.json",
		"CONTROL":     "127.0.0.1:57001",
		"CONSOLE":     "127.0.0.1:57090",
		"METADATA":    "127.0.0.1:57005",
		"STORAGE":     "http://127.0.0.1:57003",
		"PUBSUB":      "localhost:57004",
		"TASKS":       "127.0.0.1:57006",
		"SECRETS":     "127.0.0.1:57007",
		"MEMORYSTORE": "127.0.0.1:57016",
	}
	return harness, envJSON, statusJSON
}

func TestCheckTargetPassesForTheHarnessInstance(t *testing.T) {
	harness, envJSON, statusJSON := targetFixture(t)
	if err := checkTarget(harness, []string{"--name", "mine", "--state-dir", "/state"}, envJSON, statusJSON); err != nil {
		t.Fatal(err)
	}
}

// CLI_ARGS that resolve the default instance: its default ports and
// directory, while the harness names "mine".
func TestCheckTargetFailsForAnotherInstance(t *testing.T) {
	harness, _, _ := targetFixture(t)
	envJSON := []byte(`{"GOOGLE_APPLICATION_CREDENTIALS": "/home/dev/.cloudburrow/default/credentials.json",
		"GCE_METADATA_HOST": "127.0.0.1:9005", "STORAGE_EMULATOR_HOST": "http://127.0.0.1:9000",
		"PUBSUB_EMULATOR_HOST": "127.0.0.1:9002", "CLOUDBURROW_TASKS_ENDPOINT": "127.0.0.1:9003",
		"CLOUDBURROW_SECRETMANAGER_ENDPOINT": "127.0.0.1:9006", "REDIS_HOST": "127.0.0.1", "REDIS_PORT": "9016"}`)
	statusJSON := []byte(`{"instance": "default", "control_url": "http://127.0.0.1:9080", "console_url": "http://127.0.0.1:9091"}`)
	err := checkTarget(harness, []string{"--name"}, envJSON, statusJSON)
	if err == nil {
		t.Fatal("the default instance passed the check for the harness's")
	}
	for _, want := range []string{`names instance "default"`, "CLOUDBURROW_TEST_CONTROL=127.0.0.1:57001", "127.0.0.1:9080",
		"CLOUDBURROW_TEST_PUBSUB=localhost:57004", "CLOUDBURROW_TEST_CREDENTIALS", "CLOUDBURROW_TEST_MEMORYSTORE", "nothing ran"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
}

// One endpoint off is enough, whichever it is.
func TestCheckTargetFailsOnASingleMismatch(t *testing.T) {
	for _, v := range []string{"CONTROL", "CONSOLE", "STORAGE", "PUBSUB", "TASKS", "SECRETS", "METADATA", "MEMORYSTORE"} {
		harness, envJSON, statusJSON := targetFixture(t)
		harness[v] = "127.0.0.1:1"
		err := checkTarget(harness, nil, envJSON, statusJSON)
		if err == nil || !strings.Contains(err.Error(), "CLOUDBURROW_TEST_"+v+"=127.0.0.1:1") {
			t.Errorf("%s on another port: %v", v, err)
		}
	}
}

// An endpoint the harness has and the CLI's instance does not serve names
// another instance too.
func TestCheckTargetFailsOnAnEndpointTheCLIDoesNotReport(t *testing.T) {
	harness, envJSON, statusJSON := targetFixture(t)
	harness["KMS"] = "127.0.0.1:57018"
	err := checkTarget(harness, nil, envJSON, statusJSON)
	if err == nil || !strings.Contains(err.Error(), "CLOUDBURROW_TEST_KMS=127.0.0.1:57018, but the CLI's instance reports no such endpoint") {
		t.Errorf("an unreported KMS endpoint: %v", err)
	}
}

// With no endpoint to compare, nothing ties the CLI to the harness.
func TestCheckTargetFailsWithNothingToCompare(t *testing.T) {
	_, envJSON, statusJSON := targetFixture(t)
	err := checkTarget(map[string]string{"RUN_STORAGE": "http://127.0.0.1:57003"}, nil, envJSON, statusJSON)
	if err == nil || !strings.Contains(err.Error(), "no CLOUDBURROW_TEST_ endpoint is set") {
		t.Errorf("nothing to compare: %v", err)
	}
}

func TestCheckTargetFailsOnOutputThatIsNotJSON(t *testing.T) {
	harness, envJSON, statusJSON := targetFixture(t)
	if err := checkTarget(harness, nil, []byte("export A=b\n"), statusJSON); err == nil {
		t.Error("env output that is not JSON passed")
	}
	if err := checkTarget(harness, nil, envJSON, nil); err == nil {
		t.Error("empty status output passed")
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:9000/storage/v1/": "127.0.0.1:9000",
		"localhost:9000":                    "127.0.0.1:9000",
		"[::1]:9000":                        "127.0.0.1:9000",
		"0.0.0.0:9000":                      "127.0.0.1:9000",
		" https://127.0.0.1:9000/ ":         "127.0.0.1:9000",
		"10.0.0.1:9000":                     "10.0.0.1:9000",
	} {
		if got := normalizeEndpoint(in); got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

const gcloudSetupConf = `# Written by ` + "`cloudburrow gcloud-setup`" + `; ` + "`cloudburrow gcloud-teardown`" + ` removes it.
# Instance "mine".
[core]
project = mine-local
disable_usage_reporting = true

[api_endpoint_overrides]
storage = http://127.0.0.1:57003/storage/v1/
pubsub = http://127.0.0.1:57004/
cloudresourcemanager = http://127.0.0.1:57008/

[auth]
disable_credentials = true
`

// The overrides the harness vouches for are kept, the others dropped, and
// the rest of the file is untouched.
func TestGcloudRestrictConfigKeepsOnlyTheHarnessEndpoints(t *testing.T) {
	kept, problems := gcloudRestrictConfig(gcloudSetupConf, map[string]string{
		"STORAGE": "http://127.0.0.1:57003", "PUBSUB": "127.0.0.1:57004"})
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	want := strings.Replace(gcloudSetupConf, "cloudresourcemanager = http://127.0.0.1:57008/\n", "", 1)
	if kept != want {
		t.Errorf("kept:\n%s\nwant:\n%s", kept, want)
	}
}

// An override naming another endpoint than the harness's fails.
func TestGcloudRestrictConfigFailsOnAnotherEndpoint(t *testing.T) {
	_, problems := gcloudRestrictConfig(gcloudSetupConf, map[string]string{
		"STORAGE": "http://127.0.0.1:9000", "PUBSUB": "127.0.0.1:57004", "RESOURCEMANAGER": "127.0.0.1:57008"})
	if len(problems) != 1 || !strings.Contains(problems[0], "storage = http://127.0.0.1:57003/storage/v1/") {
		t.Errorf("problems = %q", problems)
	}
}

// Nothing in the caller's environment can point gcloud elsewhere.
func TestGcloudTargetEnvDropsInheritedGcloudSettings(t *testing.T) {
	got := gcloudTargetEnv([]string{
		"PATH=/usr/bin", "HOME=/home/dev",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE=http://127.0.0.1:9000/storage/v1/",
		"CLOUDSDK_CORE_PROJECT=demo-local", "CLOUDSDK_ACTIVE_CONFIG_NAME=cloudburrow-demo", "CLOUDSDK_CONFIG=/home/dev/.config/gcloud",
		"STORAGE_EMULATOR_HOST=http://127.0.0.1:9000", "GOOGLE_CLOUD_PROJECT=demo-local", "GCE_METADATA_HOST=127.0.0.1:9005",
		"BOTO_CONFIG=/home/dev/.boto",
	})
	if strings.Join(got, " ") != "PATH=/usr/bin HOME=/home/dev" {
		t.Errorf("gcloudTargetEnv kept %q", got)
	}
}
