package components_test

import (
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryimage"
	"github.com/cloudburrow/cloudburrow/internal/components"
)

// The supervisor starts BigQueryEntrypoint (#1091), and the locally built
// image puts the emulator at bigqueryimage.Path (#1061): if they drift
// apart, the emulator's container runs nothing.
func TestBigQueryEntrypointIsWhereTheImagePutsTheEmulator(t *testing.T) {
	if components.BigQueryEntrypoint != bigqueryimage.Path {
		t.Fatalf("components.BigQueryEntrypoint = %q, bigqueryimage.Path = %q", components.BigQueryEntrypoint, bigqueryimage.Path)
	}
}
