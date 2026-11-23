// Package spannerimage names CloudBurrow's patched build of Google's Cloud
// Spanner emulator (#2029), and finds it in Docker.
//
// scripts/spanner-emulator-image.sh builds it from the emulator's source at
// the tag third_party/spanner-emulator/sources.json pins, with CloudBurrow's
// patches (a GQL RETURN ... ORDER BY's order, PROPERTY_EXISTS and the other
// documented graph predicates, COUNT of a group variable, ANY CHEAPEST),
// tagged with a hash of the patches. When Docker has it for the cluster's
// architecture, up loads it into the cluster and the Spanner pod runs it;
// otherwise the pod runs Google's image, pinned by digest, without them.
package spannerimage

import (
	"context"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/images"
)

// Upstream is the emulator release the patches apply to.
const Upstream = "1.5.58"

// PatchHash is the first 12 hex digits of the SHA-256 of the patches'
// bytes, concatenated in sources.json's order (emulator's, then
// GoogleSQL's), as the build script computes it.
const PatchHash = "7c2530f5a64e"

// Ref is the patched image's reference, local to Docker and kind.
const Ref = images.LocalPrefix + "cloudburrow-spanner-emulator:" + Upstream + "-cb." + PatchHash

// Available says whether Docker has the patched image for linux/arch.
func Available(ctx context.Context, r images.Runner, arch string) bool {
	out, err := r.Run(ctx, "docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", Ref)
	return err == nil && strings.TrimSpace(out) == "linux/"+arch
}
