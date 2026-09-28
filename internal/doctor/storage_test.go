package doctor

import (
	"bytes"
	"strings"
	"testing"
)

// The embedded storage row (#686): which Linux builds the CLI has, and
// whether one is the node's. Only a missing build for the node, with
// storage enabled, blocks `up`.
func TestEmbeddedStorage(t *testing.T) {
	both := map[string]string{"amd64": "", "arm64": ""}
	none := map[string]string{"amd64": "no linux/amd64 build", "arm64": "no linux/arm64 build"}
	for _, tt := range []struct {
		name    string
		enabled bool
		node    string
		builds  map[string]string
		level   Level
		detail  []string
	}{
		{"both present", true, "arm64", both, LevelOK, []string{"linux/amd64, linux/arm64 embedded", "matching the node (linux/arm64)"}},
		{"missing", true, "arm64", none, LevelFail, []string{"none embedded", "none for the node (linux/arm64): no linux/arm64 build", "built without the embedded storage server"}},
		{"placeholder", true, "amd64", map[string]string{"amd64": "the linux/amd64 build is empty", "arm64": ""},
			LevelFail, []string{"linux/arm64 embedded", "none for the node (linux/amd64): the linux/amd64 build is empty"}},
		{"only the node's", true, "amd64", map[string]string{"amd64": "", "arm64": "no linux/arm64 build"},
			LevelOK, []string{"linux/amd64 embedded, matching the node (linux/amd64); not linux/arm64"}},
		{"an architecture never embedded", true, "riscv64", both, LevelFail, []string{"no linux/riscv64 build is ever embedded"}},
		{"missing, storage disabled", false, "arm64", none, LevelOK, []string{"none embedded; not needed, none of Cloud Storage, BigQuery or Pub/Sub is enabled"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := EmbeddedStorage(tt.enabled, tt.node, tt.builds)
			if r.Name != "embedded storage" || r.Level != tt.level {
				t.Errorf("result = %+v, want level %s", r, tt.level)
			}
			for _, want := range tt.detail {
				if !strings.Contains(r.Detail, want) {
					t.Errorf("detail %q does not say %q", r.Detail, want)
				}
			}
			if r.Level == LevelFail {
				for _, fix := range []string{"make build", "make storage-binaries", "release", "--services without storage"} {
					if !strings.Contains(r.Remedy, fix) {
						t.Errorf("remedy %q does not name %q", r.Remedy, fix)
					}
				}
				rep := Report{Results: []Result{r}}
				var out bytes.Buffer
				rep.Write(&out)
				if !rep.Blocking() || !strings.Contains(out.String(), "FAIL    embedded storage") {
					t.Errorf("report:\n%s", out.String())
				}
			}
		})
	}
}
