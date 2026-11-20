package console

import (
	"encoding/json"
	"net/http"
	"testing"
)

// An info panel's Permissions tab (InfoTab.Permissions, #1847) reads the
// row's policy with Read: the section a Permissions tab draws, with its
// etag and note, and nothing set. A row with no policy is refused.
func TestPermissionsReadForTheInfoPanel(t *testing.T) {
	t.Parallel()
	sets := 0
	f := policyFake{fakeProvider: fakeProvider{id: "iam", title: "IAM fake"},
		policy: &Policy{Etag: "e1", Bindings: []Binding{{Role: "roles/viewer", Members: []string{"user:a@example.com"}}}}, sets: &sets}
	srv := serve(t, f)
	code, body := postPermissions(t, srv.URL, map[string]any{"Path": []string{"r"}, "Read": map[string]any{}})
	var sec Section
	if err := json.Unmarshal([]byte(body), &sec); code != http.StatusOK || err != nil {
		t.Fatalf("read = %d %v: %s", code, err, body)
	}
	if sec.Kind != KindPermissions || sec.Permissions == nil || sec.Permissions.Etag != "e1" ||
		sec.Permissions.Note != PermissionsNote || len(sec.Permissions.Bindings) != 1 {
		t.Errorf("read = %+v", sec)
	}
	if code, body := postPermissions(t, srv.URL, map[string]any{"Path": []string{"other"}, "Read": map[string]any{}}); code != http.StatusBadRequest {
		t.Errorf("a read of a row with no policy = %d %s; want 400", code, body)
	}
	if code, body := postPermissions(t, srv.URL, map[string]any{"Path": []string{"r"}, "Read": map[string]any{},
		"Test": map[string]any{"Permissions": []string{"a.get"}}}); code != http.StatusBadRequest {
		t.Errorf("a read with a test = %d %s; want 400", code, body)
	}
	if sets != 0 {
		t.Errorf("a read reached SetPolicy %d times", sets)
	}
}
