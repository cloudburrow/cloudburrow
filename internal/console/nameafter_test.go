package console

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNameAfterAndThenReachThePage (#1853): a listing's NameAfter and an
// action's Then are sent to the page under the names console.js reads.
func TestNameAfterAndThenReachThePage(t *testing.T) {
	b, err := json.Marshal(Listing{Columns: []string{"Environment", "Region"}, NameAfter: "Environment",
		Actions: []Action{{ID: "write", Label: "Write a function", Then: "/functions/{name}?tab=source"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"nameAfter":"Environment"`, `"then":"/functions/{name}?tab=source"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("listing JSON %s lacks %s", b, want)
		}
	}
	b, _ = json.Marshal(Listing{})
	if strings.Contains(string(b), "nameAfter") {
		t.Errorf("an empty NameAfter is sent: %s", b)
	}
	js, err := assets.ReadFile("assets/console.js")
	if err != nil {
		t.Skip("console.js is not embedded as assets/console.js:", err)
	}
	for _, want := range []string{"data.nameAfter", "action.then", "Rename file"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("console.js does not read %s", want)
		}
	}
}
