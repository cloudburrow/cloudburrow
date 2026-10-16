package bigqueryfront

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUnqualifiedSchemataLinks (#1392): SCHEMATA_LINKS with no region
// qualifier, bare or project-qualified, is read as region-us's; a
// region-qualified one is left to informationSchemaJobs, a
// dataset-qualified one to the dataset views (501).
func TestUnqualifiedSchemataLinks(t *testing.T) {
	for sql, n := range map[string]int{
		"SELECT * FROM INFORMATION_SCHEMA.SCHEMATA_LINKS":                   1,
		"SELECT * FROM `p-1`.INFORMATION_SCHEMA.SCHEMATA_LINKS AS l":        1,
		"SELECT * FROM `p-1.INFORMATION_SCHEMA.SCHEMATA_LINKS`":             1,
		"SELECT * FROM `region-us`.INFORMATION_SCHEMA.SCHEMATA_LINKS":       0,
		"SELECT * FROM `p-1`.`region-us`.INFORMATION_SCHEMA.SCHEMATA_LINKS": 0,
		"SELECT * FROM INFORMATION_SCHEMA.SCHEMATA":                         0,
		"SELECT 'INFORMATION_SCHEMA.SCHEMATA_LINKS'":                        0,
	} {
		refs := unqualifiedSchemataLinks(sql, "p-1")
		if len(refs) != n {
			t.Errorf("%s: %d references, want %d", sql, len(refs), n)
		}
		for _, r := range refs {
			if r.region != "region-us" || r.view != "SCHEMATA_LINKS" {
				t.Errorf("%s: %+v", sql, r)
			}
		}
	}
}

// TestJobsAncestry (#1392): the project's ancestry is read from Resource
// Manager's getAncestry; without one the views are 501, and a project
// Resource Manager does not have is in no folder.
func TestJobsAncestry(t *testing.T) {
	rm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects/p-1:getAncestry":
			_, _ = w.Write([]byte(`{"ancestor":[{"resourceId":{"type":"project","id":"p-1"}},{"resourceId":{"type":"folder","id":"2"}},` +
				`{"resourceId":{"type":"folder","id":"3"}},{"resourceId":{"type":"organization","id":"100000000001"}}]}`))
		default:
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
		}
	}))
	defer rm.Close()
	f := front{resourceManager: rm.URL}
	chain, st, _ := f.ancestry(context.Background(), "p-1")
	if st != 0 || len(chain) != 4 || chain[1].ResourceID.ID != "2" || chain[3].ResourceID.Type != "organization" {
		t.Errorf("ancestry = %v, %d", chain, st)
	}
	if chain, st, _ := f.ancestry(context.Background(), "gone"); st != 0 || len(chain) != 0 {
		t.Errorf("a project Resource Manager lacks = %v, %d", chain, st)
	}
	if _, st, _ := (front{}).ancestry(context.Background(), "p-1"); st != http.StatusNotImplemented {
		t.Errorf("no Resource Manager: %d, want 501", st)
	}
}
