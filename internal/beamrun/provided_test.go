package beamrun

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestReadProvided: the build's generated metadata files are read by
// their template's name (internalName), its main class, staging options
// and streaming flag kept; a build missing one of ProvidedNames fails.
func TestReadProvided(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, n := range ProvidedNames {
		b := fmt.Sprintf(`{"name": "Display %d", "internalName": %q, "mainClass": "x.Main%d", "runtimeParameters": {"useSubscription": "true"}, "streaming": %v}`, i, n, i, i%2 == 0)
		if err := os.WriteFile(filepath.Join(dir, "metadata", fmt.Sprintf("t%d-generated-metadata.json", i)), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readProvided(dir)
	if err != nil {
		t.Fatal(err)
	}
	wc := got["Word_Count"]
	if wc.MainClass == "" || wc.RuntimeParameters["useSubscription"] != "true" || len(wc.Metadata) == 0 || len(got) != len(ProvidedNames) {
		t.Errorf("readProvided = %+v", got)
	}
	if err := os.Remove(filepath.Join(dir, "metadata", "t0-generated-metadata.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := readProvided(dir); err == nil {
		t.Error("readProvided of a build missing a template succeeded")
	}
}
