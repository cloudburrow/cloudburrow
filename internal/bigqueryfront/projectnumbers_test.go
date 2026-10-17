package bigqueryfront

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// TestServiceAgentNumberIsTheRecords (#1551): the service agent names the
// number the host sent for the project, and the derived one for a project
// it sent none for; a restart of the front keeps the numbers.
func TestServiceAgentNumberIsTheRecords(t *testing.T) {
	dir := t.TempDir()
	h := Wrap(&oneDatasetEmulator{}, func(o *options) { o.stateDir = dir })
	if code, _ := do(t, h, http.MethodPost, ProjectNumbersPath, `{"p": 123456789012}`); code != http.StatusNoContent {
		t.Fatalf("POST %s = %d, want 204", ProjectNumbersPath, code)
	}
	want := "bq-123456789012@bigquery-encryption.iam.gserviceaccount.com"
	for _, h := range []http.Handler{h, Wrap(&oneDatasetEmulator{}, func(o *options) { o.stateDir = dir })} {
		if _, got := do(t, h, http.MethodGet, base+"/serviceAccount", ""); got["email"] != want {
			t.Errorf("getServiceAccount = %v, want %s", got, want)
		}
	}
	other := "bq-" + strconv.FormatInt(resource.DerivedProjectNumber("q"), 10) + "@bigquery-encryption.iam.gserviceaccount.com"
	if _, got := do(t, h, http.MethodGet, "/bigquery/v2/projects/q/serviceAccount", ""); got["email"] != other {
		t.Errorf("getServiceAccount of a project with no number sent = %v, want %s", got, other)
	}
	if code, _ := do(t, h, http.MethodGet, ProjectNumbersPath, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET %s = %d, want 405", ProjectNumbersPath, code)
	}
}
