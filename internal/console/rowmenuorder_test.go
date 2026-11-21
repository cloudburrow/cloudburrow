package console

import (
	"net/http"
	"strings"
	"testing"
)

type orderedProvider struct{ fakeProvider }

func (orderedProvider) RowMenuOrder() []string {
	return []string{"view", RowMenuEdit, "disable", RowMenuDelete}
}

// TestRowMenuOrderCapability (#1863): a RowMenuOrderer's order reaches the
// client as the service's rowMenuOrder, and a provider without one sends
// none.
func TestRowMenuOrderCapability(t *testing.T) {
	srv := serve(t, orderedProvider{fakeProvider{id: "ordered", title: "Ordered"}}, fakeProvider{id: "plain", title: "Plain"})
	code, body := get(t, srv, "/api/services", nil)
	if code != http.StatusOK {
		t.Fatalf("services = %d %s", code, body)
	}
	if !strings.Contains(body, `"rowMenuOrder":["view","edit","disable","delete"]`) {
		t.Errorf("the order is not sent: %s", body)
	}
	if !strings.Contains(body, `"rowMenuOrder":null,"scope":"","title":"Plain"`) {
		t.Errorf("a provider without an order sent one: %s", body)
	}
}
