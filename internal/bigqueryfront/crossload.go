package bigqueryfront

import (
	"encoding/json"
	"net/http"
)

// A load into another project's table than the job's (#1767). "Load jobs
// ... the destination table can be in a different project" (BigQuery runs
// the job in the job's project and writes the table in its own,
// https://cloud.google.com/bigquery/docs/resource-hierarchy): the front
// reads the table's schema, and writes its rows, in the table's project,
// and records the job in the job's. dataFront is the front the table is
// read and written through: f itself for a table of the job's project,
// else f with its base in the table's project, whose scratch dataset (the
// hidden one every project's carried-out writes use) is made first.
func (f front) dataFront(r *http.Request, dest *tableRef) front {
	if dest == nil || dest.ProjectID == "" || dest.ProjectID == projectOf(f.base) {
		return f
	}
	df := f.inProject(dest.ProjectID)
	body, _ := json.Marshal(map[string]any{"datasetReference": map[string]string{"projectId": dest.ProjectID, "datasetId": resultsDataset}})
	df.send(r, http.MethodPost, "/datasets", body) // 409 when it is there
	return df
}
