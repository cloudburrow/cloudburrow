package bigqueryfront

import (
	"encoding/json"
	"testing"
)

// TestSelectedFields (#1038): tabledata.list's selectedFields keeps the
// cells named, top-level and nested, in the schema's order; a name the
// schema lacks is refused.
func TestSelectedFields(t *testing.T) {
	fields := []field{
		{Name: "a", Type: "INT64"},
		{Name: "s", Type: "RECORD", Fields: []field{{Name: "x", Type: "INT64"}, {Name: "y", Type: "STRING"}}},
		{Name: "r", Type: "RECORD", Mode: "REPEATED", Fields: []field{{Name: "x", Type: "INT64"}, {Name: "y", Type: "STRING"}}},
		{Name: "b", Type: "STRING"},
	}
	row := `{"f":[{"v":"1"},{"v":{"f":[{"v":"2"},{"v":"u"}]}},{"v":[{"v":{"f":[{"v":"3"},{"v":"w"}]}}]},{"v":"z"}]}`
	for _, c := range []struct{ param, want string }{
		{"b,A", `{"f":[{"v":"1"},{"v":"z"}]}`},
		{"s.y", `{"f":[{"v":{"f":[{"v":"u"}]}}]}`},
		{"r.x,s", `{"f":[{"v":{"f":[{"v":"2"},{"v":"u"}]}},{"v":[{"v":{"f":[{"v":"3"}]}}]}]}`},
	} {
		sel, err := parseSelectedFields(fields, c.param)
		if err != nil {
			t.Fatalf("%s: %v", c.param, err)
		}
		got := selectRows(fields, sel, []json.RawMessage{json.RawMessage(row)})
		if string(got[0]) != c.want {
			t.Errorf("%s: %s, want %s", c.param, got[0], c.want)
		}
	}
	for _, bad := range []string{"nope", "a.x", "s.nope"} {
		if _, err := parseSelectedFields(fields, bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}
