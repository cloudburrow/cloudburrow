//go:build compat

package compat

import (
	"net/http"
	"net/url"
	"testing"
)

// TestConsoleBigQueryNamesTheServiceAccount (#1270): the BigQuery page's
// summary names the service account the official generated client's
// projects.getServiceAccount answers, the account a customer-managed
// encryption key is granted to.
//
// covers: bigquery.projects.getServiceAccount
func TestConsoleBigQueryNamesTheServiceAccount(t *testing.T) {
	h := New(t)
	_, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	addr := consoleAddr(t, h)

	want, err := svc.Projects.GetServiceAccount(project).Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("projects.getServiceAccount: %v", err)
	}
	var list struct {
		Unavailable string `json:"unavailable"`
		Summary     []struct {
			Properties []struct {
				Label string `json:"label"`
				Value string `json:"value"`
			} `json:"properties"`
		} `json:"summary"`
	}
	consoleJSON(t, addr, http.MethodGet, "/api/resources/bigquery?project="+url.QueryEscape(project), "", &list)
	got := ""
	for _, g := range list.Summary {
		for _, p := range g.Properties {
			if p.Label == "BigQuery service account" {
				got = p.Value
			}
		}
	}
	if list.Unavailable != "" || got != want.Email {
		t.Errorf("the BigQuery page names %q (unavailable %q), want %q, what the client reads", got, list.Unavailable, want.Email)
	}
}
