package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	bqv2 "google.golang.org/api/bigquery/v2"
)

// A column's data governance tags on the table page's Schema tab (#1456).
//
// The table resource keeps them as a field's
// dataGovernanceTagsInfo.dataGovernanceTags, a map of tag key to value
// (TableFieldSchema). The official Go client's FieldSchema has no field for
// them, so the page reads tables.get over REST into the generated client's
// Table, which has. Google's console shows them and has no form for them:
// "You can use the Google Cloud console to view data governance tags on
// columns, but not to bind or unbind them"
// (https://docs.cloud.google.com/bigquery/docs/tags#data-governance-tags,
// docs re-read 2026-10-17). So the page shows them, and its note names the
// statement that binds them.

// governanceTagsColumn is the Schema tab's column of a field's tags.
const governanceTagsColumn = "Data governance tags"

// governanceTagsNote is the Schema tab's note when a field has tags.
const governanceTagsNote = "Google's console shows data governance tags but has no form to bind them: run ALTER TABLE … " +
	"ALTER COLUMN … SET OPTIONS (data_governance_tags = [('parent-id/key', 'value')]) in the query editor's " +
	"Read-write mode (NULL removes them), or send tables.patch."

// readGovernanceTags is the tags of each field of dataset.table, by its
// dotted path, as governanceTagsByField gives them.
func (p bigqueryProvider) readGovernanceTags(ctx context.Context, datasetID, tableID string) (map[string]string, error) {
	if p.endpoint == "" {
		return nil, errors.New("no BigQuery endpoint")
	}
	u := "http://" + p.endpoint + "/bigquery/v2/projects/" + url.PathEscape(p.project) + "/datasets/" +
		url.PathEscape(datasetID) + "/tables/" + url.PathEscape(tableID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach BigQuery: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tables.get answered %d", resp.StatusCode)
	}
	var t bqv2.Table
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("reading the table: %w", err)
	}
	if t.Schema == nil {
		return nil, nil
	}
	return governanceTagsByField(t.Schema.Fields, ""), nil
}

// governanceTagsByField is each field of fields (and of their nested
// fields) that has data governance tags, by its dotted path, its tags as
// "key: value", sorted by key and joined by "; ".
func governanceTagsByField(fields []*bqv2.TableFieldSchema, prefix string) map[string]string {
	out := map[string]string{}
	for _, f := range fields {
		if f == nil {
			continue
		}
		if info := f.DataGovernanceTagsInfo; info != nil && len(info.DataGovernanceTags) > 0 {
			keys := make([]string, 0, len(info.DataGovernanceTags))
			for k := range info.DataGovernanceTags {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var tags []string
			for _, k := range keys {
				tags = append(tags, k+": "+info.DataGovernanceTags[k])
			}
			out[prefix+f.Name] = strings.Join(tags, "; ")
		}
		for k, v := range governanceTagsByField(f.Fields, prefix+f.Name+".") {
			out[k] = v
		}
	}
	return out
}
