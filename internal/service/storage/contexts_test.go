package storage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseContextFilter(t *testing.T) {
	o := objectRecord{Contexts: map[string]objectContext{"team": {Value: "blue"}}}
	for _, c := range []struct {
		filter string
		match  bool
	}{
		{`contexts."team":*`, true},
		{`contexts."team"="blue"`, true},
		{`contexts."team"="red"`, false},
		{`contexts."zone":*`, false},
		{`-contexts."team":*`, false},
		{`NOT contexts."team"="red"`, true},
		{` contexts."team" = "blue" `, true},
	} {
		f, err := parseContextFilter(c.filter)
		if err != nil {
			t.Fatalf("%s: %v", c.filter, err)
		}
		if got := f.match(o); got != c.match {
			t.Errorf("%s matched %v, want %v", c.filter, got, c.match)
		}
	}
	for _, bad := range []string{`contexts."a":* AND contexts."b":*`, `contexts."a":* OR contexts."b":*`, `name="x"`,
		`contexts.a:*`, `contexts."a"`, `contexts."a"=blue`, `contexts."a":x`, `contexts."a"="b"c"`} {
		if _, err := parseContextFilter(bad); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
	if f, err := parseContextFilter(""); f != nil || err != nil {
		t.Errorf("empty filter: %v, %v", f, err)
	}
}

func TestApplyContexts(t *testing.T) {
	t0, t1 := time.Unix(100, 0), time.Unix(200, 0)
	o := objectRecord{}
	body := map[string]any{"custom": map[string]any{"a": map[string]any{"value": "1"}, "b": map[string]any{"value": "2"}}}
	if err := checkContextsBody(body); err != nil {
		t.Fatal(err)
	}
	applyContexts(&o, body, false, t0)
	patch := map[string]any{"custom": map[string]any{"a": map[string]any{"value": "1"}, "b": nil, "c": map[string]any{}, "d": map[string]any{"value": "4"}}}
	if err := checkContextsBody(patch); err != nil {
		t.Fatal(err)
	}
	applyContexts(&o, patch, false, t1)
	if len(o.Contexts) != 2 || o.Contexts["a"].Updated != t0 || o.Contexts["d"].Created != t1 {
		t.Errorf("after patch: %+v", o.Contexts)
	}
	applyContexts(&o, map[string]any{"custom": nil}, false, t1)
	if o.Contexts != nil {
		t.Errorf("custom null kept %+v", o.Contexts)
	}
	for _, bad := range []any{
		"x", map[string]any{"other": nil},
		map[string]any{"custom": map[string]any{"google:k": map[string]any{"value": "v"}}},
		map[string]any{"custom": map[string]any{"_k": map[string]any{"value": "v"}}},
		map[string]any{"custom": map[string]any{"k": map[string]any{"value": "a/b"}}},
		map[string]any{"custom": map[string]any{"k": map[string]any{"value": ""}}},
		map[string]any{"custom": map[string]any{"k": map[string]any{"value": 3}}},
	} {
		if err := checkContextsBody(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// objects.viewFullContext returns one context as an ObjectFullContext, with
// no extendedData, since no context here has any; extendedDataTypeUrl in a
// patch is the server's to set and is ignored (#1700).
func TestViewFullContext(t *testing.T) {
	h := newTestServer(t)
	raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"vfc"}`)
	if code, body := raw(t, "POST", h.URL+"/upload/storage/v1/b/vfc/o?uploadType=media&name=a%2Fb.txt", "hi"); code != 200 {
		t.Fatalf("upload = %d %s", code, body)
	}
	if code, body := raw(t, "PATCH", h.URL+"/storage/v1/b/vfc/o/a%2Fb.txt",
		`{"contexts":{"custom":{"team":{"value":"blue","extendedDataTypeUrl":"type.googleapis.com/google.protobuf.Struct"}}}}`); code != 200 ||
		strings.Contains(body, "extendedDataTypeUrl") {
		t.Fatalf("patch = %d %s", code, body)
	}
	code, body := raw(t, "GET", h.URL+"/storage/v1/b/vfc/o/a%2Fb.txt/viewFullContext?contextKey=team", "")
	var got map[string]any
	if code != 200 || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("viewFullContext = %d %s", code, body)
	}
	if got["kind"] != "storage#objectFullContext" || got["type"] != "CUSTOM" || got["key"] != "team" || got["value"] != "blue" ||
		got["createTime"] == nil || got["updateTime"] == nil || got["extendedData"] != nil {
		t.Errorf("viewFullContext = %s", body)
	}
	for path, want := range map[string]int{
		"/storage/v1/b/vfc/o/a%2Fb.txt/viewFullContext":                              400,
		"/storage/v1/b/vfc/o/a%2Fb.txt/viewFullContext?contextKey=zone":              404,
		"/storage/v1/b/vfc/o/nope/viewFullContext?contextKey=team":                   404,
		"/storage/v1/b/vfc/o/a%2Fb.txt/viewFullContext?contextKey=team&generation=1": 404,
	} {
		if code, body := raw(t, "GET", h.URL+path, ""); code != want {
			t.Errorf("GET %s = %d %s, want %d", path, code, body, want)
		}
	}
}
