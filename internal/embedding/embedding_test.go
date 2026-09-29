package embedding

import (
	"math"
	"testing"
)

func TestTruncateRenormalises(t *testing.T) {
	v := []float32{0.6, 0.8, 0, 0}
	got := Truncate([]float32{3, 4, 5, 6}, 2)
	for i := range got {
		if math.Abs(float64(got[i]-v[i])) > 1e-6 {
			t.Fatalf("got %v", got)
		}
	}
}

func TestPromptAndDims(t *testing.T) {
	p, err := Prompt(Input{Content: "x", TaskType: "RETRIEVAL_DOCUMENT"})
	if err != nil || p != "title: none | text: x" {
		t.Fatalf("%q %v", p, err)
	}
	if _, err := Prompt(Input{TaskType: "NOPE"}); err == nil {
		t.Fatal("unknown task type accepted")
	}
	if ValidateDim(300) == nil || ValidateDim(256) != nil {
		t.Fatal("dimension validation")
	}
}
