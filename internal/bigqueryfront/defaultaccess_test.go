package bigqueryfront

import (
	"encoding/json"
	"testing"
)

// A datasets.insert body naming no access list gets BigQuery's default one
// (#1304); one naming a list, even an empty one, is left as it is.
func TestWithDefaultAccess(t *testing.T) {
	b, ok := withDefaultAccess([]byte(`{"datasetReference":{"datasetId":"d"},"location":"US"}`))
	if !ok {
		t.Fatal("a body without access was left alone")
	}
	var got struct {
		Location string `json:"location"`
		Access   []struct {
			Role, SpecialGroup string
		} `json:"access"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"projectWriters": "WRITER", "projectOwners": "OWNER", "projectReaders": "READER"}
	if got.Location != "US" || len(got.Access) != len(want) {
		t.Fatalf("body = %s", b)
	}
	for _, a := range got.Access {
		if want[a.SpecialGroup] != a.Role {
			t.Errorf("entry %+v; want %v", a, want)
		}
	}
	for _, body := range []string{`{"access":[]}`, `{"access":[{"role":"READER","userByEmail":"a@example.com"}]}`, `[]`, `x`} {
		if _, ok := withDefaultAccess([]byte(body)); ok {
			t.Errorf("withDefaultAccess(%s) rewrote it", body)
		}
	}
	if _, ok := withDefaultAccess([]byte(`{"access":null}`)); !ok {
		t.Error(`withDefaultAccess({"access":null}) kept the null`)
	}
}
