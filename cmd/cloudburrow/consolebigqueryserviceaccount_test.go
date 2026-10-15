package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// TestBigQueryListNamesTheServiceAccount (#1270): the BigQuery page's
// summary names the project and the service account projects.getServiceAccount
// answers through the front, so a developer setting up a customer-managed key
// reads the account to grant from the console. Another project gets the
// prompt and no summary.
func TestBigQueryListNamesTheServiceAccount(t *testing.T) {
	emulator := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/datasets") {
			_, _ = io.WriteString(w, `{"kind":"bigquery#datasetList","datasets":[]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":404,"message":"not found"}}`)
	})
	srv := httptest.NewServer(bigqueryfront.Wrap(emulator))
	t.Cleanup(srv.Close)
	p := bigqueryProvider{endpoint: strings.TrimPrefix(srv.URL, "http://"), project: "served-project"}

	list, err := p.List(context.Background(), "served-project")
	if err != nil || list.Unavailable != "" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	want := "bq-" + strconv.FormatInt(resource.ProjectNumber("served-project"), 10) + "@bigquery-encryption.iam.gserviceaccount.com"
	got := map[string]string{}
	for _, g := range list.Summary {
		for _, prop := range g.Properties {
			got[prop.Label] = prop.Value
		}
	}
	if got["Project ID"] != "served-project" || got["BigQuery service account"] != want {
		t.Errorf("summary = %v, want the project and %s", got, want)
	}

	other, err := p.List(context.Background(), "other-project")
	if err != nil || other.Prompt == "" || len(other.Summary) != 0 {
		t.Errorf("another project = %+v, %v; want the prompt and no summary", other, err)
	}
}
