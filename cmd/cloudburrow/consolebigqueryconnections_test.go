package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBigQueryConnectionsSummary (#1755): the datasets list names the
// project's connections, their types and service accounts.
func TestBigQueryConnectionsSummary(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/p/locations/us/connections" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"connections":[{"name":"projects/1/locations/us/connections/ai","cloudResource":{"serviceAccountId":"bqcx-1-ab@gcp-sa-bigquery-condel.iam.gserviceaccount.com"}}]}`)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	g := p.connectionsSummary(context.Background(), "p")
	if len(g.Properties) != 1 || g.Properties[0].Label != "ai" || !strings.Contains(g.Properties[0].Value, "cloudResource, service account bqcx-1-ab@") {
		t.Errorf("summary %+v", g)
	}
	if g := p.connectionsSummary(context.Background(), "other"); len(g.Properties) != 1 || !strings.HasPrefix(g.Properties[0].Value, "unavailable") {
		t.Errorf("a failed list %+v", g)
	}
}
