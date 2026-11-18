package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestConsoleBigQueryUndeleteDataset (#1552): the BigQuery list screen
// offers Undelete dataset on the project the emulator serves, and nowhere
// else; it sends datasets.undelete for the ID typed, and a refusal (a
// dataset never deleted) comes back in the API's own words.
func TestConsoleBigQueryUndeleteDataset(t *testing.T) {
	t.Parallel()
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/projects/p/datasets/gone:undelete"):
			fmt.Fprint(w, `{"datasetReference":{"projectId":"p","datasetId":"gone"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":undelete"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":404,"message":"Not found: Dataset p:never was not deleted"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	ctx := context.Background()

	// Another project's list offers it too (#1764), and with none chosen,
	// nothing.
	if a := p.ListActions(ctx, "other"); len(a) != 1 {
		t.Errorf("another project's list offers %+v", a)
	}
	if a := p.ListActions(ctx, ""); a != nil {
		t.Errorf("no project's list offers %+v", a)
	}
	a := p.ListActions(ctx, "p")
	if len(a) != 1 || a[0].ID != actUndeleteDataset || len(a[0].Fields) != 1 || !a[0].Fields[0].Required {
		t.Fatalf("the BigQuery list offers %+v; want Undelete dataset with a required Dataset ID", a)
	}
	if err := p.ActAt(ctx, "p", nil, actUndeleteDataset, map[string]string{"dataset": " gone "}); err != nil {
		t.Fatalf("Undelete dataset: %v", err)
	}
	if len(calls) != 1 || !strings.HasSuffix(calls[0], "/projects/p/datasets/gone:undelete") || !strings.HasPrefix(calls[0], "POST ") {
		t.Errorf("Undelete dataset sent %v; want one datasets.undelete of gone", calls)
	}
	err := p.ActAt(ctx, "p", nil, actUndeleteDataset, map[string]string{"dataset": "never"})
	if err == nil || err.Error() != "Not found: Dataset p:never was not deleted" {
		t.Errorf("an undelete of a dataset never deleted = %v; want the API's own words", err)
	}
	if err := p.ActAt(ctx, "p", nil, actUndeleteDataset, map[string]string{"dataset": " "}); err == nil {
		t.Error("an undelete with no dataset ID was sent")
	}
	// In another project, it is that project's (#1764): the fake has no
	// such deleted dataset.
	if err := p.ActAt(ctx, "other", nil, actUndeleteDataset, map[string]string{"dataset": "gone"}); err == nil {
		t.Error("an undelete in another project of a dataset it never deleted succeeded")
	}
	if len(calls) != 3 || !strings.HasSuffix(calls[2], "/projects/other/datasets/gone:undelete") {
		t.Errorf("the undeletes sent %v; want the last in project other", calls)
	}
}
