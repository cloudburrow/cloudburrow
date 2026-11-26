package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/spannerimage"
)

// When this CLI embeds no patched Spanner emulator for the nodes, up runs
// Google's image and warns, naming the GQL ORDER BY bug it has (#2029).
func TestSpannerFallbackWarningNamesTheOrderByBug(t *testing.T) {
	_, err := spannerimage.Build(context.Background(), nil, "riscv64", "w")
	w := spannerFallbackWarning("arm64", err)
	for _, want := range []string{"warning:", components.SpannerImage, "RETURN ... ORDER BY", "out of order",
		"PROPERTY_EXISTS", "ANY CHEAPEST", "make spanner-binaries", "linux/arm64"} {
		if !strings.Contains(w, want) {
			t.Errorf("the warning does not name %q:\n%s", want, w)
		}
	}
}
