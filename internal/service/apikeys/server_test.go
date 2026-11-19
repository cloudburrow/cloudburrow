package apikeys

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func newTest(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer("", store.NewMemory(), nil)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func call(t *testing.T, ts *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const parent = "/v2/projects/p1/locations/global/keys"

func TestKeyLifecycle(t *testing.T) {
	s, ts := newTest(t)
	code, op := call(t, ts, "POST", parent+"?keyId=web-key", map[string]any{"displayName": "Web",
		"restrictions": map[string]any{"browserKeyRestrictions": map[string]any{"allowedReferrers": []string{"*.example.com/*"}},
			"apiTargets": []any{map[string]any{"service": "storage.googleapis.com"}}}})
	if code != 200 || op["done"] != true {
		t.Fatalf("create: %d %v", code, op)
	}
	resp := op["response"].(map[string]any)
	ks, _ := resp["keyString"].(string)
	if !regexp.MustCompile(`^AIza[A-Za-z0-9_-]{35}$`).MatchString(ks) {
		t.Errorf("keyString %q is not of Google's form", ks)
	}
	name := "projects/" + itoa(resource.ProjectNumber("p1")) + "/locations/global/keys/web-key"
	if resp["name"] != name {
		t.Errorf("name %v, want %s", resp["name"], name)
	}
	if code, got := call(t, ts, "GET", "/v2/"+op["name"].(string), nil); code != 200 || got["done"] != true {
		t.Errorf("operations.get: %d %v", code, got)
	}
	code, got := call(t, ts, "GET", parent+"/web-key", nil)
	if code != 200 || got["keyString"] != nil || got["displayName"] != "Web" {
		t.Errorf("get: %d %v", code, got)
	}
	if code, got := call(t, ts, "GET", parent+"/web-key/keyString", nil); code != 200 || got["keyString"] != ks {
		t.Errorf("getKeyString: %d %v", code, got)
	}
	if code, got := call(t, ts, "GET", "/v2/keys:lookupKey?keyString="+ks, nil); code != 200 || got["name"] != name {
		t.Errorf("lookupKey: %d %v", code, got)
	}
	if code, _ := call(t, ts, "POST", parent+"?keyId=web-key", map[string]any{}); code != 409 {
		t.Errorf("duplicate create: %d, want 409", code)
	}
	// patch with a mask changes only what it names.
	code, op = call(t, ts, "PATCH", parent+"/web-key?updateMask=displayName", map[string]any{"displayName": "Renamed", "restrictions": map[string]any{}})
	if code != 200 {
		t.Fatalf("patch: %d %v", code, op)
	}
	k, _ := s.Get("p1", "web-key")
	if k.DisplayName != "Renamed" || k.Restrictions == nil || k.Restrictions.BrowserKeyRestrictions == nil {
		t.Errorf("after patch: %+v", k)
	}
	if code, _ := call(t, ts, "PATCH", parent+"/web-key?updateMask=keyString", map[string]any{}); code != 400 {
		t.Errorf("patch of keyString: %d, want 400", code)
	}
	// delete, list, undelete.
	if code, _ := call(t, ts, "DELETE", parent+"/web-key", nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if _, got := call(t, ts, "GET", parent, nil); got["keys"] != nil {
		t.Errorf("list without showDeleted: %v", got)
	}
	if _, got := call(t, ts, "GET", parent+"?showDeleted=true", nil); len(got["keys"].([]any)) != 1 {
		t.Errorf("list with showDeleted: %v", got)
	}
	if _, _, ok := s.Lookup(ks); ok {
		t.Error("a deleted key still authorizes")
	}
	if code, _ := call(t, ts, "POST", parent+"/web-key:undelete", map[string]any{}); code != 200 {
		t.Fatalf("undelete: %d", code)
	}
	if _, _, ok := s.Lookup(ks); !ok {
		t.Error("an undeleted key does not authorize")
	}
	if code, _ := call(t, ts, "POST", parent+"/web-key:undelete", map[string]any{}); code != 400 {
		t.Errorf("undelete of a live key: %d, want 400", code)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestPurgeAfter30Days(t *testing.T) {
	s, ts := newTest(t)
	now := time.Now()
	s.Now = func() time.Time { return now }
	k, err := s.Create("p1", "old", "", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete("p1", "old"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(Retention + time.Hour)
	if code, _ := call(t, ts, "GET", parent+"/old", nil); code != 404 {
		t.Errorf("get of a purged key: %d, want 404", code)
	}
	code, got := call(t, ts, "GET", "/v2/keys:lookupKey?keyString="+k.KeyString, nil)
	if code != 200 || got["name"] != nil || !strings.HasSuffix(got["parent"].(string), "/locations/global") {
		t.Errorf("lookupKey of a purged key: %d %v", code, got)
	}
	if code, _ := call(t, ts, "POST", parent+"/old:undelete", map[string]any{}); code != 404 {
		t.Errorf("undelete of a purged key: %d, want 404", code)
	}
}

func TestValidation(t *testing.T) {
	_, ts := newTest(t)
	for name, body := range map[string]map[string]any{
		"two kinds": {"restrictions": map[string]any{
			"browserKeyRestrictions": map[string]any{"allowedReferrers": []string{"example.com"}},
			"serverKeyRestrictions":  map[string]any{"allowedIps": []string{"198.51.100.1"}}}},
		"mid wildcard":   {"restrictions": map[string]any{"browserKeyRestrictions": map[string]any{"allowedReferrers": []string{"my*.example.com"}}}},
		"bad ip":         {"restrictions": map[string]any{"serverKeyRestrictions": map[string]any{"allowedIps": []string{"300.1.1.1"}}}},
		"bad sha1":       {"restrictions": map[string]any{"androidKeyRestrictions": map[string]any{"allowedApplications": []any{map[string]any{"packageName": "com.example", "sha1Fingerprint": "abc"}}}}},
		"method wild":    {"restrictions": map[string]any{"apiTargets": []any{map[string]any{"service": "a.googleapis.com", "methods": []string{"G*t"}}}}},
		"long name":      {"displayName": strings.Repeat("x", 64)},
		"target service": {"restrictions": map[string]any{"apiTargets": []any{map[string]any{"methods": []string{"Get"}}}}},
	} {
		if code, got := call(t, ts, "POST", parent, body); code != 400 {
			t.Errorf("%s: %d %v, want 400", name, code, got)
		}
	}
	for _, id := range []string{"Upper", "1abc", "a-", "123e4567-e89b-12d3-a456-426614174000"} {
		if code, _ := call(t, ts, "POST", parent+"?keyId="+id, map[string]any{}); code != 400 {
			t.Errorf("keyId %q: %d, want 400", id, code)
		}
	}
	if code, _ := call(t, ts, "POST", "/v2/projects/p1/locations/us-east1/keys", map[string]any{}); code != 400 {
		t.Errorf("non-global location: %d, want 400", code)
	}
	// Colon-separated fingerprints are answered in plain hex.
	code, op := call(t, ts, "POST", parent, map[string]any{"restrictions": map[string]any{"androidKeyRestrictions": map[string]any{
		"allowedApplications": []any{map[string]any{"packageName": "com.example.app",
			"sha1Fingerprint": "DA:39:A3:EE:5E:6B:4B:0D:32:55:BF:EF:95:60:18:90:AF:D8:07:09"}}}}})
	if code != 200 {
		t.Fatalf("android create: %d %v", code, op)
	}
	b, _ := json.Marshal(op)
	if !strings.Contains(string(b), `"sha1Fingerprint":"DA39A3EE5E6B4B0D3255BFEF95601890AFD80709"`) {
		t.Errorf("fingerprint not normalized: %s", b)
	}
}

func TestListPaging(t *testing.T) {
	s, ts := newTest(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Create("p1", "", "", nil, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	_, got := call(t, ts, "GET", parent+"?pageSize=2", nil)
	if len(got["keys"].([]any)) != 2 || got["nextPageToken"] != "2" {
		t.Fatalf("page 1: %v", got)
	}
	_, got = call(t, ts, "GET", parent+"?pageSize=2&pageToken=2", nil)
	if len(got["keys"].([]any)) != 1 || got["nextPageToken"] != nil {
		t.Errorf("page 2: %v", got)
	}
}

func TestRotateAndReset(t *testing.T) {
	s, _ := newTest(t)
	old, _ := s.Create("p1", "old", "Old", &Restrictions{IosKeyRestrictions: &IosKeyRestrictions{AllowedBundleIds: []string{"com.example"}}}, nil, "")
	k, err := s.Rotate("p1", "old", "New")
	if err != nil {
		t.Fatal(err)
	}
	if k.PreviousKey != "old" || k.KeyString == old.KeyString || k.Restrictions.IosKeyRestrictions == nil {
		t.Errorf("rotated: %+v", k)
	}
	if _, _, ok := s.Lookup(old.KeyString); !ok {
		t.Error("the previous key stopped working on rotation")
	}
	if err := s.ResetProject("p1"); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.List("p1", true); len(keys) != 0 {
		t.Errorf("after reset: %d keys", len(keys))
	}
	if _, _, ok := s.Lookup(k.KeyString); ok {
		t.Error("a reset key still authorizes")
	}
}

// Every method in the table is routed: none answers the router's
// not-a-method 404 for a well-formed request.
func TestEveryMethodRouted(t *testing.T) {
	s, ts := newTest(t)
	k, _ := s.Create("p", "k", "", nil, nil, "")
	for _, m := range Methods {
		path := "/" + m.Path
		if m.ID == "apikeys.keys.lookupKey" {
			path += "?keyString=" + k.KeyString
		}
		if m.ID == "apikeys.operations.get" {
			op := s.done(k, false)
			path = "/v2/" + op["name"].(string)
		}
		var body any
		if m.HTTPMethod == "POST" || m.HTTPMethod == "PATCH" {
			body = map[string]any{}
		}
		code, got := call(t, ts, m.HTTPMethod, path, body)
		e, _ := got["error"].(map[string]any)
		if msg, _ := e["message"].(string); strings.Contains(msg, "is not an API Keys API v2") {
			t.Errorf("%s (%s %s): %d %s", m.ID, m.HTTPMethod, path, code, msg)
		}
	}
}
