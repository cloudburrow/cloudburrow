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

	if a := p.ListActions(ctx, "other"); a != nil {
		t.Errorf("another project's list offers %+v", a)
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
	if err := p.ActAt(ctx, "other", nil, actUndeleteDataset, map[string]string{"dataset": "gone"}); err == nil {
		t.Error("an undelete on a project the emulator does not serve was sent")
	}
	if len(calls) != 2 {
		t.Errorf("the refused undeletes reached the API: %v", calls)
	}
}
