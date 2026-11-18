package bigqueryfront

import (
	"errors"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/tfconvert"
)

// Each converter refusal answers as BigQuery documents the limit, and what
// CloudBurrow cannot run is 501, never a result (#1775).
func TestTensorFlowRefusal(t *testing.T) {
	for kind, want := range map[string]struct {
		code int
		text string
	}{
		"format":      {400, "SavedModel format"},
		"signature":   {400, "SavedModel format"},
		"custom":      {400, "only core TensorFlow operations are supported"},
		"limit":       {400, "450 MB"},
		"flex":        {501, "SELECT_TF_OPS"},
		"convert":     {501, "could not lower"},
		"unavailable": {501, "cannot run"},
	} {
		code, msg := tensorflowRefusal("gs://b/m/", &tfconvert.Refusal{Kind: kind, Message: "why"})
		if code != want.code || !strings.Contains(msg, want.text) || !strings.Contains(msg, "why") && kind != "limit" {
			t.Errorf("%s: %d %q", kind, code, msg)
		}
	}
	if code, _ := tensorflowRefusal("gs://b/m/", errors.New("boom")); code != 501 {
		t.Errorf("a plain error: %d", code)
	}
	if !(&tfconvert.Refusal{Kind: "flex"}).Unsupported() || (&tfconvert.Refusal{Kind: "format"}).Unsupported() {
		t.Error("Unsupported")
	}
}

// Without the CLI's converter every TENSORFLOW model is 501.
func TestTensorFlowWithoutConverter(t *testing.T) {
	_, _, code, msg := front{}.importTensorFlow(nil, modelDDL{onnxPath: "gs://b/m/*"})
	if code != 501 || !strings.Contains(msg, "--tfconvert") {
		t.Fatalf("%d %q", code, msg)
	}
}
