package spannerfront

import (
	"net/url"
	"strings"
	"testing"
)

// TestQueryMask: a mask sent as a query parameter, by its JSON or its proto
// name, as Terraform's google_spanner_instance_partition sends
// ?field_mask=display_name,node_count (#1304).
func TestQueryMask(t *testing.T) {
	for q, want := range map[string]string{
		"field_mask=display_name,node_count": "display_name,node_count",
		"fieldMask=displayName,nodeCount":    "display_name,node_count",
		"other=x":                            "",
	} {
		v, _ := url.ParseQuery(q)
		got := ""
		if m := (restReq{q: v}).queryMask("fieldMask", "field_mask"); m != nil {
			got = strings.Join(m.GetPaths(), ",")
		}
		if got != want {
			t.Errorf("queryMask(%s) = %q, want %q", q, got, want)
		}
	}
}
