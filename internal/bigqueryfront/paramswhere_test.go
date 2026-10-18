package bigqueryfront

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParamsWhere: the parameters a span of the query names (#1458).
func TestParamsWhere(t *testing.T) {
	names := func(raw json.RawMessage) string {
		var ps []struct {
			Name           string `json:"name"`
			ParameterValue struct {
				Value string `json:"value"`
			} `json:"parameterValue"`
		}
		if err := json.Unmarshal(raw, &ps); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		var out []string
		for _, p := range ps {
			out = append(out, p.Name+"="+p.ParameterValue.Value)
		}
		return strings.Join(out, ",")
	}
	param := func(name, v string) string {
		return `{"name":"` + name + `","parameterType":{"type":"STRING"},"parameterValue":{"value":"` + v + `"}}`
	}
	for _, c := range []struct {
		query, params, span, want string
	}{
		{"SELECT * FROM t FOR SYSTEM_TIME AS OF @at WHERE x = @pt", "[" + param("at", "1") + "," + param("pt", "2") + "]", "@at", "at=1"},
		{"SELECT * FROM t FOR SYSTEM_TIME AS OF @AT WHERE x = @at", "[" + param("at", "1") + "]", "@AT", "at=1"},
		{"SELECT * FROM t FOR SYSTEM_TIME AS OF ? WHERE x = ?", "[" + param("", "1") + "," + param("", "2") + "]", "? WHERE", "=1"},
		{"SELECT @@time_zone, @a FROM t", "[" + param("a", "1") + "]", "@@time_zone", ""},
	} {
		q := queryOptions{Query: c.query, QueryParameters: json.RawMessage(c.params)}
		from := strings.Index(c.query, c.span)
		to := from + len(c.span) - len(" WHERE")*strings.Count(c.span, " WHERE")
		in := func(pos int) bool { return pos >= from && pos < to }
		if got := names(paramsWhere(q, in)); got != c.want {
			t.Errorf("%s: in %q: %q, want %q", c.query, c.span, got, c.want)
		}
		out := func(pos int) bool { return !in(pos) }
		gotOut := names(paramsWhere(q, out))
		if c.query == "SELECT * FROM t FOR SYSTEM_TIME AS OF @at WHERE x = @pt" && gotOut != "pt=2" ||
			strings.Contains(c.query, "?") && gotOut != "=2" {
			t.Errorf("%s: outside %q: %q", c.query, c.span, gotOut)
		}
	}
}
