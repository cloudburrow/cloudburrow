package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestConnections (#1755): the Connection API's create, get, list, patch
// and delete, kept across a restart.
func TestConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	c := &bqConnections{}
	c.keep(path, nil)
	numbers := &projectNumbers{}
	do := func(c *bqConnections, method, target, body string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		if !c.serve(w, httptest.NewRequest(method, target, strings.NewReader(body)), numbers) {
			t.Fatalf("%s %s not served", method, target)
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	base := "/v1/projects/p/locations/US/connections"
	code, res := do(c, http.MethodPost, base+"?connectionId=ai", `{"friendlyName":"a","cloudResource":{}}`)
	sa, _ := res["cloudResource"].(map[string]any)["serviceAccountId"].(string)
	if code != 200 || !strings.HasSuffix(res["name"].(string), "/locations/us/connections/ai") || !strings.HasPrefix(sa, "bqcx-") {
		t.Fatalf("create: %d %v", code, res)
	}
	if code, _ := do(c, http.MethodPost, base+"?connectionId=ai", `{}`); code != 409 {
		t.Errorf("second create: %d", code)
	}
	if code, _ := do(c, http.MethodPost, base+"?connectionId=bad.id", `{}`); code != 400 {
		t.Errorf("bad id: %d", code)
	}
	code, res = do(c, http.MethodPost, base+"?connectionId=sql", `{"cloudSql":{"database":"d","credential":{"username":"u","password":"x"}}}`)
	if code != 200 || res["hasCredential"] != true || res["cloudSql"].(map[string]any)["credential"] != nil {
		t.Errorf("cloudSql: %d %v", code, res)
	}
	if code, _ := do(c, http.MethodPost, base+"?connectionId=two", `{"cloudSql":{},"cloudResource":{}}`); code != 400 {
		t.Errorf("two types: %d", code)
	}
	if code, _ := do(c, http.MethodPatch, base+"/ai", `{"friendlyName":"b"}`); code != 400 {
		t.Errorf("patch without a mask: %d", code)
	}
	if code, res := do(c, http.MethodPatch, base+"/ai?updateMask=friendly_name", `{"friendlyName":"b"}`); code != 200 || res["friendlyName"] != "b" {
		t.Errorf("patch: %d %v", code, res)
	}
	code, res = do(c, http.MethodGet, base+"?pageSize=1", "")
	if code != 200 || len(res["connections"].([]any)) != 1 || res["nextPageToken"] != "sql" {
		t.Errorf("list: %d %v", code, res)
	}
	// Restored from the state file.
	c2 := &bqConnections{}
	c2.keep(path, nil)
	if code, res := do(c2, http.MethodGet, base+"/ai", ""); code != 200 || res["friendlyName"] != "b" {
		t.Errorf("after a restart: %d %v", code, res)
	}
	if code, _ := do(c2, http.MethodDelete, base+"/ai", ""); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := do(c2, http.MethodGet, base+"/ai", ""); code != 404 {
		t.Errorf("get after delete: %d", code)
	}
}
