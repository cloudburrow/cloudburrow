package bigqueryfront

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClientLoadIsPutBack (#998): a load from Cloud Storage reads back
// with the client's configuration.load, sourceUris and sourceColumnMatch
// too, and jobType LOAD.
func TestClientLoadIsPutBack(t *testing.T) {
	body := `{"configuration":{"load":{"sourceUris":["gs://b/a.csv"],"sourceColumnMatch":"NAME","sourceFormat":"CSV"}}}`
	r := httptest.NewRequest("POST", "/jobs", strings.NewReader(body))
	l := clientLoad(r, []string{"gs://b/a.csv"})
	if l == nil {
		t.Fatal("no load")
	}
	if clientLoad(r, nil) != nil {
		t.Error("a load with no sourceUris kept")
	}
	var job map[string]any
	_ = json.Unmarshal([]byte(`{"configuration":{"load":{"sourceFormat":"CSV"}}}`), &job)
	jobText{load: l}.patch(job)
	conf := job["configuration"].(map[string]any)
	load := conf["load"].(map[string]any)
	if load["sourceColumnMatch"] != "NAME" || load["sourceUris"] == nil || conf["jobType"] != "LOAD" {
		t.Errorf("patched: %v", job)
	}
	if saveJobText("k", jobText{load: l}).jobText().load == nil {
		t.Error("the load is not kept across a restart")
	}
}
