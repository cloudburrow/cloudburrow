package bigqueryfront

import (
	"encoding/json"
	"testing"
)

// TestInformationSchemaJobsReferencesAreFound: #1275.
func TestInformationSchemaJobsReferencesAreFound(t *testing.T) {
	for _, c := range []struct {
		sql   string
		views []string
	}{
		{"SELECT * FROM `region-us`.INFORMATION_SCHEMA.JOBS", []string{"JOBS"}},
		{"SELECT job_id FROM `p`.`region-us`.INFORMATION_SCHEMA.JOBS_BY_PROJECT j", []string{"JOBS_BY_PROJECT"}},
		{"SELECT * FROM `p.region-eu.INFORMATION_SCHEMA.JOBS_BY_USER`", []string{"JOBS_BY_USER"}},
		{"SELECT * FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_ORGANIZATION", []string{"JOBS_BY_ORGANIZATION"}},
		{"SELECT * FROM ds.INFORMATION_SCHEMA.JOBS", nil},
		// #1329: the region-qualified dataset views and TABLE_STORAGE.
		{"SELECT * FROM `region-us`.INFORMATION_SCHEMA.TABLES", []string{"TABLES"}},
		{"SELECT * FROM `region-us`.INFORMATION_SCHEMA.TABLE_STORAGE", []string{"TABLE_STORAGE"}},
		{"SELECT * FROM `region-us`.INFORMATION_SCHEMA.KEY_COLUMN_USAGE", nil},
		{"SELECT '`region-us`.INFORMATION_SCHEMA.JOBS'", nil},
	} {
		refs := informationSchemaJobs(c.sql)
		if len(refs) != len(c.views) {
			t.Fatalf("%s: %+v", c.sql, refs)
		}
		for i, ref := range refs {
			if ref.view != c.views[i] {
				t.Errorf("%s: ref %d %+v", c.sql, i, ref)
			}
		}
	}
}

// TestJobRowHasEveryColumn: #1275.
func TestJobRowHasEveryColumn(t *testing.T) {
	var job map[string]any
	if err := json.Unmarshal([]byte(`{"jobReference":{"jobId":"j1","projectId":"p"},
		"configuration":{"jobType":"QUERY","labels":{"k":"v"},"query":{"query":"SELECT 1","useLegacySql":false,
		"destinationTable":{"projectId":"p","datasetId":"d","tableId":"t"}}},
		"statistics":{"creationTime":"1792085926120","startTime":"1792085926120","endTime":"1792085926131",
		"query":{"statementType":"SELECT","totalBytesProcessed":"1","totalBytesBilled":"1"}},
		"status":{"state":"DONE","errorResult":{"reason":"invalidQuery","message":"bad"}}}`), &job); err != nil {
		t.Fatal(err)
	}
	row := jobRow("p", job, nil)
	want := len(splitTop(mustLex(t, jobsColumns[len("STRUCT<"):len(jobsColumns)-1])))
	if len(row) != want {
		t.Fatalf("%d values for %d columns: %v", len(row), want, row)
	}
	if row[7] != "'j1'" || row[8] != "'QUERY'" || row[21] != "'DONE'" || row[22] != "'SELECT'" || row[24] != "1" ||
		row[5] != "('invalidQuery', NULL, NULL, 'bad')" || row[32] != "'GOOGLE_SQL'" {
		t.Errorf("row %v", row)
	}
}

// TestJobRowQueryDialect (#1595): a query job that asked no dialect is
// DEFAULT_GOOGLE_SQL when it was recorded as defaulting to GoogleSQL by
// its region's default_sql_dialect_option, else DEFAULT_LEGACY_SQL.
func TestJobRowQueryDialect(t *testing.T) {
	job := func(id string) map[string]any {
		return map[string]any{"jobReference": map[string]any{"jobId": id, "projectId": "p"},
			"configuration": map[string]any{"jobType": "QUERY", "query": map[string]any{"query": "SELECT 1"}}}
	}
	settings := &projectOptions{}
	settings.markGoogleDefault("p", "g")
	for id, want := range map[string]string{"g": "'DEFAULT_GOOGLE_SQL'", "l": "'DEFAULT_LEGACY_SQL'"} {
		if got := jobRow("p", job(id), settings)[32]; got != want {
			t.Errorf("job %s: query_dialect %s, want %s", id, got, want)
		}
	}
	if got := jobRow("p", job("g"), nil)[32]; got != "'DEFAULT_LEGACY_SQL'" {
		t.Errorf("no settings: query_dialect %s", got)
	}
}
