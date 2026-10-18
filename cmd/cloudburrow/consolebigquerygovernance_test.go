package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bqv2 "google.golang.org/api/bigquery/v2"
)

// TestGovernanceTagsByField (#1456): each field's data governance tags, a
// nested field's by its dotted path, read from tables.get over REST.
func TestGovernanceTagsByField(t *testing.T) {
	tags := func(m map[string]string) *bqv2.TableFieldSchemaDataGovernanceTagsInfo {
		return &bqv2.TableFieldSchemaDataGovernanceTagsInfo{DataGovernanceTags: m}
	}
	got := governanceTagsByField([]*bqv2.TableFieldSchema{
		{Name: "phone", DataGovernanceTagsInfo: tags(map[string]string{"p1/data_class": "private"})},
		{Name: "plain"},
		{Name: "rec", Fields: []*bqv2.TableFieldSchema{{Name: "x", DataGovernanceTagsInfo: tags(map[string]string{"b/k": "2", "a/k": "1"})}}},
		{Name: "cleared", DataGovernanceTagsInfo: tags(nil)},
	}, "")
	want := map[string]string{"phone": "p1/data_class: private", "rec.x": "a/k: 1; b/k: 2"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bigquery/v2/projects/p/datasets/d/tables/t" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"schema":{"fields":[{"name":"phone","type":"STRING",` +
			`"dataGovernanceTagsInfo":{"dataGovernanceTags":{"p1/data_class":"private"}}},{"name":"n","type":"INTEGER"}]}}`))
	}))
	defer srv.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(srv.URL, "http://"), project: "p"}
	read, err := p.readGovernanceTags(context.Background(), "d", "t")
	if err != nil || len(read) != 1 || read["phone"] != "p1/data_class: private" {
		t.Errorf("readGovernanceTags = %v, %v", read, err)
	}
	if _, err := p.readGovernanceTags(context.Background(), "d", "missing"); err == nil {
		t.Error("a table tables.get does not find: no error")
	}
	if _, err := (bigqueryProvider{}).readGovernanceTags(context.Background(), "d", "t"); err == nil {
		t.Error("no endpoint: no error")
	}
}
