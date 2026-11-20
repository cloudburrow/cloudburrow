package repo

import (
	"encoding/json"
	"os"
	"path/filepath"
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
