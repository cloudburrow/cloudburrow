//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// consoleListing is the part of a console listing these tests read.
type consoleListing struct {
	NameColumn  string   `json:"nameColumn"`
	Columns     []string `json:"columns"`
	Unavailable string   `json:"unavailable"`
	Prompt      string   `json:"prompt"`
	Items       []struct {
		Name   string            `json:"name"`
		Fields map[string]string `json:"fields"`
	} `json:"items"`
}

// consoleDetail is the part of a console detail page these tests read.
type consoleDetail struct {
	Unavailable string `json:"unavailable"`
	Prompt      string `json:"prompt"`
	Sections    []struct {
		ID      string         `json:"id"`
		Listing consoleListing `json:"listing"`
	} `json:"sections"`
}

func (d consoleDetail) section(t *testing.T, id string) consoleListing {
	t.Helper()
	for _, s := range d.Sections {
		if s.ID == id {
			return s.Listing
		}
	}
	t.Fatalf("the page has no %q section: %+v", id, d)
	return consoleListing{}
}

func consoleJSON(t *testing.T, addr, method, path, body string, into any) {
	t.Helper()
	code, out := consoleDo(t, addr, method, path, body)
	if code != http.StatusOK {
		t.Fatalf("%s %s = %d: %s", method, path, code, out)
	}
	if err := json.Unmarshal([]byte(out), into); err != nil {
		t.Fatalf("%s %s: %v: %s", method, path, err, out)
	}
}

func datasetIDs(t *testing.T, h *Harness, c *bigquery.Client) []string {
	t.Helper()
	var ids []string
	it := c.Datasets(h.Context())
	for {
		ds, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return ids
		}
		if err != nil {
			t.Fatalf("list datasets: %v", err)
		}
		ids = append(ids, ds.DatasetID)
	}
}

// TestConsoleBigQueryDatasetsSchemaAndQuery.
//
// The BigQuery screen (#698), in the emulators shard. A dataset and table the
// official Go client created, with four streamed rows, are listed on the
// screen; the table's page shows its schema as the client created it and its
// four rows; the query editor runs a SELECT against the dataset on the page
// and returns the inserted rows, leaving no dataset behind; a DELETE is
// refused and deletes nothing. Another project shows a prompt naming the one
// project the emulator serves, not an error and not an empty table.
func TestConsoleBigQueryDatasetsSchemaAndQuery(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ds, _ := seedOrders(t, h, c)
	q := "?project=" + url.QueryEscape(project)

	var list consoleListing
	consoleJSON(t, addr, http.MethodGet, "/api/resources/bigquery"+q, "", &list)
	if list.Unavailable != "" || list.Prompt != "" {
		t.Fatalf("the served project's datasets did not list: %+v", list)
	}
	found := false
	for _, it := range list.Items {
		if it.Name == ds.DatasetID {
			found = true
			if it.Fields["Tables"] != "1" {
				t.Errorf("dataset %s lists %q tables, want 1", ds.DatasetID, it.Fields["Tables"])
			}
		}
	}
	if !found {
		t.Fatalf("the console does not list the SDK-created dataset %s: %+v", ds.DatasetID, list.Items)
	}

	var dsPage consoleDetail
	consoleJSON(t, addr, http.MethodGet, "/api/detail/bigquery"+q+"&name="+ds.DatasetID, "", &dsPage)
	tables := dsPage.section(t, "tables")
	if len(tables.Items) != 1 || tables.Items[0].Name != "orders" || tables.Items[0].Fields["Rows"] != "4" {
		t.Errorf("the dataset's page lists %+v, want the one table orders with 4 rows", tables.Items)
	}

	var tablePage consoleDetail
	consoleJSON(t, addr, http.MethodGet, "/api/detail/bigquery"+q+"&name="+ds.DatasetID+"&name=orders", "", &tablePage)
	var schema []string
	for _, f := range tablePage.section(t, "schema").Items {
		schema = append(schema, f.Name+":"+f.Fields["Type"])
	}
	if strings.Join(schema, ",") != "id:INTEGER,region:STRING,amount:FLOAT" {
		t.Errorf("the schema tab shows %v, want the schema the client created", schema)
	}
	if preview := tablePage.section(t, "preview"); len(preview.Items) != 4 {
		t.Errorf("the preview shows %d rows, want the 4 inserted", len(preview.Items))
	}

	before := datasetIDs(t, h, c)
	body, _ := json.Marshal(map[string]any{
		"Path":      []string{ds.DatasetID, "orders"},
		"Statement": "SELECT id, region, amount FROM orders ORDER BY id",
	})
	var result struct{ Listing consoleListing }
	consoleJSON(t, addr, http.MethodPost, "/api/query/bigquery"+q, string(body), &result)
	var rows []string
	for _, r := range result.Listing.Items {
		rows = append(rows, r.Name+" "+r.Fields["region"]+" "+r.Fields["amount"])
	}
	if want := "1 eu 10|2 eu 5.5|3 us 7|4 us 1"; strings.Join(rows, "|") != want {
		t.Errorf("the query returned %q, want %q", strings.Join(rows, "|"), want)
	}
	if result.Listing.NameColumn != "id" || strings.Join(result.Listing.Columns, ",") != "region,amount" {
		t.Errorf("the result's columns are %q + %v, want the query's own", result.Listing.NameColumn, result.Listing.Columns)
	}
	if after := datasetIDs(t, h, c); len(after) != len(before) {
		t.Errorf("a console query changed the datasets from %v to %v", before, after)
	}

	body, _ = json.Marshal(map[string]any{
		"Path": []string{ds.DatasetID}, "Statement": "DELETE FROM orders WHERE true",
	})
	code, out := consoleDo(t, addr, http.MethodPost, "/api/query/bigquery"+q, string(body))
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "read-only") {
		t.Errorf("a DELETE = %d %s, want 400 naming the editor read-only", code, out)
	}
	md, err := ds.Table("orders").Metadata(h.Context())
	if err != nil {
		t.Fatalf("table metadata: %v", err)
	}
	if md.NumRows != 4 {
		t.Errorf("after a refused DELETE the table has %d rows, want 4", md.NumRows)
	}

	// Another project: the harness's own, which the emulator does not serve.
	other := h.Project()
	if other == project {
		t.Fatalf("the harness project %q is the served one; the other-project check needs another", other)
	}
	var elsewhere consoleListing
	consoleJSON(t, addr, http.MethodGet, "/api/resources/bigquery?project="+url.QueryEscape(other), "", &elsewhere)
	if elsewhere.Unavailable != "" || len(elsewhere.Items) != 0 || !strings.Contains(elsewhere.Prompt, `"`+project+`"`) {
		t.Errorf("another project showed %+v, want a prompt naming %q", elsewhere, project)
	}
	var elsewherePage consoleDetail
	consoleJSON(t, addr, http.MethodGet, "/api/detail/bigquery?project="+url.QueryEscape(other)+"&name="+ds.DatasetID, "", &elsewherePage)
	if elsewherePage.Unavailable != "" || elsewherePage.Prompt == "" || len(elsewherePage.Sections) != 0 {
		t.Errorf("a dataset opened in another project showed %+v, want the prompt", elsewherePage)
	}
}
