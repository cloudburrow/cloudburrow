package doctor

import (
	"fmt"
	"slices"
	"strings"
)

// EmbeddedBigQueryFix is how to get a CLI with the BigQuery emulator in it,
// or start without BigQuery.
const EmbeddedBigQueryFix = "build with `make build` (or `make bigquery-binaries` before `go build`), " +
	"install a release (docs/install.md), or pass --services without bigquery"

// EmbeddedBigQuery reports whether this CLI embeds the patched BigQuery
// emulator (#1061), for which Linux architectures, and whether one is the
// node's. missing maps each architecture a build could embed to "" when it
// is present, or to why it is not; nodeArch is the Docker daemon's, which a
// kind node shares. As with the storage server, only the node's
// architecture decides, and with BigQuery disabled nothing blocks.
func EmbeddedBigQuery(enabled bool, nodeArch string, missing map[string]string) Result {
	const name = "embedded bigquery"
	var present, absent []string
	for arch, why := range missing {
		if why == "" {
			present = append(present, "linux/"+arch)
		} else {
			absent = append(absent, "linux/"+arch)
		}
	}
	slices.Sort(present)
	slices.Sort(absent)
	has := "none embedded"
	if len(present) > 0 {
		has = strings.Join(present, ", ") + " embedded"
	}
	why, known := missing[nodeArch]
	if !known {
		why = "no linux/" + nodeArch + " build is ever embedded"
	}
	switch {
	case why == "":
		detail := fmt.Sprintf("%s, matching the node (linux/%s)", has, nodeArch)
		if len(absent) > 0 {
			detail += "; not " + strings.Join(absent, ", ")
		}
		return Result{Name: name, Level: LevelOK, Detail: detail}
	case !enabled:
		return Result{Name: name, Level: LevelOK, Detail: has + "; not needed, BigQuery is not enabled"}
	default:
		return Result{Name: name, Level: LevelFail,
			Detail: fmt.Sprintf("%s; none for the node (linux/%s): %s. This CLI was built without the "+
				"embedded BigQuery emulator, so `up` would refuse to start BigQuery", has, nodeArch, why),
			Remedy: EmbeddedBigQueryFix}
	}
}
