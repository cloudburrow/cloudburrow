package doctor

import (
	"strings"
	"testing"
)

// The embedded BigQuery row (#1061): only a missing build for the node,
// with BigQuery enabled, blocks `up`.
func TestEmbeddedBigQuery(t *testing.T) {
	both := map[string]string{"amd64": "", "arm64": ""}
	none := map[string]string{"amd64": "no linux/amd64 build", "arm64": "no linux/arm64 build"}
	for _, tt := range []struct {
		name    string
		enabled bool
		node    string
		builds  map[string]string
		level   Level
		detail  string
	}{
		{"both present", true, "arm64", both, LevelOK, "linux/amd64, linux/arm64 embedded, matching the node (linux/arm64)"},
		{"missing", true, "amd64", none, LevelFail, "none for the node (linux/amd64): no linux/amd64 build"},
		{"only the other", true, "amd64", map[string]string{"amd64": "not gzip", "arm64": ""}, LevelFail, "linux/arm64 embedded; none for the node (linux/amd64): not gzip"},
		{"missing, disabled", false, "arm64", none, LevelOK, "none embedded; not needed, BigQuery is not enabled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := EmbeddedBigQuery(tt.enabled, tt.node, tt.builds)
			if r.Name != "embedded bigquery" || r.Level != tt.level || !strings.Contains(r.Detail, tt.detail) {
				t.Errorf("result = %+v, want level %s and %q", r, tt.level, tt.detail)
			}
			if r.Level == LevelFail {
				for _, fix := range []string{"make build", "make bigquery-binaries", "release", "--services without bigquery"} {
					if !strings.Contains(r.Remedy, fix) {
						t.Errorf("remedy %q does not name %q", r.Remedy, fix)
					}
				}
			}
		})
	}
}
