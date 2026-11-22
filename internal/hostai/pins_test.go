package hostai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The host runtimes' pins are dependencies.json's (#2155): the archive's
// SHA-256 and tag, and the MLX lock's versions.
func TestPinsMatchTheInventory(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "dependencies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inv struct {
		Components struct {
			LocalAI map[string]struct {
				Version   string            `json:"version"`
				Checksums map[string]string `json:"checksums"`
			} `json:"localai"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatal(err)
	}
	llama := inv.Components.LocalAI["hostLlamaCpp"]
	asset := LlamaCppArchive[strings.LastIndexByte(LlamaCppArchive, '/')+1:]
	if llama.Version != LlamaCppTag || llama.Checksums[asset] != "sha256:"+LlamaCppSHA256 {
		t.Errorf("dependencies.json hostLlamaCpp = %+v; the code pins %s %s sha256:%s", llama, LlamaCppTag, asset, LlamaCppSHA256)
	}
	mlx := inv.Components.LocalAI["hostMLX"]
	for _, v := range strings.Split(mlx.Version, ", ") {
		name, ver, _ := strings.Cut(v, " ")
		if !strings.Contains(MLXLock, "\n"+name+"=="+ver+" ") {
			t.Errorf("dependencies.json hostMLX pins %s %s; mlx-lock.txt does not", name, ver)
		}
	}
}
