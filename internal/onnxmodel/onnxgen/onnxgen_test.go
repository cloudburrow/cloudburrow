package onnxgen

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.onnx")

// TestCheckedInModels: the checked-in models are exactly this package's
// output, so their provenance is onnxgen.go.
func TestCheckedInModels(t *testing.T) {
	for name, b := range map[string][]byte{"linear.onnx": Linear(), "sequence.onnx": SequenceOutput()} {
		p := filepath.Join("testdata", name)
		if *update {
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, b) {
			t.Errorf("%s differs from onnxgen's output; run go test ./internal/onnxmodel/onnxgen -update", p)
		}
	}
}
