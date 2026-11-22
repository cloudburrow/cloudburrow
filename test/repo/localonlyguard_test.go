package repo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/unserved"
)

// TestEndpointInventoriesMatchThePins (#1975): the local-only guard sets
// every endpoint of the pinned tools from internal/unserved's inventories,
// so a pin moved in dependencies.json without them would leave the new
// tool's added endpoints at their Google defaults. A bump must rerun
// scripts/endpoint-inventory.sh, which writes the inventory files of the new
// versions; the compat test TestPinnedToolsGainNoUnmappedEndpoint then checks
// them against the real tools.
func TestEndpointInventoriesMatchThePins(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "dependencies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var deps struct {
		Components struct {
			RealTools map[string]json.RawMessage `json:"realTools"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &deps); err != nil {
		t.Fatal(err)
	}
	version := func(key string) string {
		var v struct{ Version string }
		_ = json.Unmarshal(deps.Components.RealTools[key], &v)
		return v.Version
	}
	provider, gcloud := version("hashicorpGoogleProvider"), version("googleCloudCli")
	if provider != unserved.TerraformProviderVersion {
		t.Errorf("dependencies.json pins hashicorp/google %s, but internal/unserved's inventory is of %s: run scripts/endpoint-inventory.sh",
			provider, unserved.TerraformProviderVersion)
	}
	if gcloud != unserved.GcloudVersion {
		t.Errorf("dependencies.json pins gcloud %s, but internal/unserved's inventory is of %s: run scripts/endpoint-inventory.sh",
			gcloud, unserved.GcloudVersion)
	}
	for _, f := range []string{"terraform-google-" + provider + ".txt", "terraform-google-beta-" + provider + ".txt", "gcloud-" + gcloud + ".txt"} {
		if _, err := os.Stat(filepath.Join("..", "..", "internal", "unserved", f)); err != nil {
			t.Errorf("no inventory for the pinned tool: %v", err)
		}
	}
	if len(unserved.TerraformEndpoints(false)) == 0 || len(unserved.TerraformEndpoints(true)) == 0 || len(unserved.GcloudOverrides()) == 0 {
		t.Error("an inventory is empty")
	}
}

// gcloudDownload matches a workflow's download of the Google Cloud CLI and
// the sha256sum check on the line after it.
var gcloudDownload = regexp.MustCompile(`google-cloud-cli-([0-9.]+)-linux-x86_64\.tar\.gz\n\s*echo "([0-9a-f]{64}) `)

// TestEveryWorkflowInstallsThePinnedGcloud (#2129): TestPinnedToolsGainNoUnmappedEndpoint
// compares the gcloud it is given with internal/unserved's inventory, which
// is of dependencies.json's pin. The Cloud SQL ephemeral nightly still
// installed 586.0.0 after ci.yml moved to 593.0.0, so the test found the four
// override properties 593.0.0 dropped and failed every night.
func TestEveryWorkflowInstallsThePinnedGcloud(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "dependencies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var deps struct {
		Components struct {
			RealTools struct {
				GoogleCloudCli struct {
					Version   string            `json:"version"`
					Checksums map[string]string `json:"checksums"`
				} `json:"googleCloudCli"`
			} `json:"realTools"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &deps); err != nil {
		t.Fatal(err)
	}
	pin := deps.Components.RealTools.GoogleCloudCli
	archive := "google-cloud-cli-" + pin.Version + "-linux-x86_64.tar.gz"
	sum := strings.TrimPrefix(pin.Checksums[archive], "sha256:")
	if pin.Version == "" || sum == "" {
		t.Fatalf("dependencies.json has no googleCloudCli version or no checksum for %s", archive)
	}
	files, err := filepath.Glob(filepath.Join(workflowsDir, "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range files {
		body := readWorkflow(t, filepath.Base(f))
		if n := strings.Count(body, "google-cloud-cli-"); n != len(gcloudDownload.FindAllString(body, -1)) {
			t.Errorf("%s: a google-cloud-cli download is not followed by its sha256sum check", filepath.Base(f))
		}
		for _, m := range gcloudDownload.FindAllStringSubmatch(body, -1) {
			found++
			if m[1] != pin.Version || m[2] != sum {
				t.Errorf("%s installs gcloud %s (sha256 %s), but dependencies.json pins %s (sha256 %s), the release internal/unserved's inventory is of",
					filepath.Base(f), m[1], m[2], pin.Version, sum)
			}
		}
	}
	if found == 0 {
		t.Error("no workflow installs the Google Cloud CLI; the pattern no longer matches")
	}
}
