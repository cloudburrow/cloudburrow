package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
)

// The table a load makes (#1457).
//
// A load's configuration names properties of the table it makes, when the
// table is new (JobConfigurationLoad,
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobConfigurationLoad):
// timePartitioning and rangePartitioning ("partitioning specification for
// the destination table"), clustering ("Clustering specification for the
// destination table"), destinationEncryptionConfiguration ("Custom
// encryption configuration"), and destinationTableProperties
// ("Properties with which to create the destination table if it is new":
// description, friendlyName, labels). The pinned emulator drops every one
// (measured: a JSON load with each into a new table read back with none).
//
// So when a load would make its table and names any of them, the front
// makes the table first, by tables.insert (createTable), with the load's
// schema (given, or detected: jsondetect.go, csvdetect.go) and those
// properties, and the emulator's load then appends to it (WRITE_TRUNCATE
// and WRITE_TRUNCATE_DATA are an append to a new table). A load that fails
// leaves no table: the front deletes the one it made. A Parquet or Avro
// load the front carries out makes its table with them (pqPlan.props). A
// table that exists keeps its own properties, as destinationTableProperties
// says; a load into a partition (a decorator) is not changed.

// loadTableJob is the part of a load's job naming its table's properties.
type loadTableJob struct {
	Configuration struct {
		Load struct {
			TimePartitioning                   json.RawMessage `json:"timePartitioning"`
			RangePartitioning                  json.RawMessage `json:"rangePartitioning"`
			Clustering                         json.RawMessage `json:"clustering"`
			DestinationEncryptionConfiguration json.RawMessage `json:"destinationEncryptionConfiguration"`
			DestinationTableProperties         *struct {
				Description  *string           `json:"description"`
				FriendlyName *string           `json:"friendlyName"`
				Labels       map[string]string `json:"labels"`
			} `json:"destinationTableProperties"`
			Schema            json.RawMessage `json:"schema"`
			CreateDisposition string          `json:"createDisposition"`
		} `json:"load"`
	} `json:"configuration"`
}

// loadTableProps are the properties, as a Table resource's, that the load
// in r's body names for a new table, or nil.
func loadTableProps(r *http.Request) (map[string]any, loadTableJob) {
	var job loadTableJob
	if !decodeJob(r, &job) {
		return nil, job
	}
	l := job.Configuration.Load
	props := map[string]any{}
	set := func(key string, raw json.RawMessage) {
		if len(raw) > 0 && string(raw) != "null" {
			props[key] = raw
		}
	}
	set("timePartitioning", l.TimePartitioning)
	set("rangePartitioning", l.RangePartitioning)
	set("clustering", l.Clustering)
	set("encryptionConfiguration", l.DestinationEncryptionConfiguration)
	if p := l.DestinationTableProperties; p != nil {
		if p.Description != nil {
			props["description"] = *p.Description
		}
		if p.FriendlyName != nil {
			props["friendlyName"] = *p.FriendlyName
		}
		if len(p.Labels) > 0 {
			props["labels"] = p.Labels
		}
	}
	if len(props) == 0 {
		return nil, job
	}
	return props, job
}

// makeLoadTable makes the table of a load with a schema in its job (above)
// when it is new and the load names properties for it, and returns a
// function that deletes it again, to be called if the load fails, or nil
// when it made none. A table it cannot make is answered on w, and ok is
// false.
func (f front) makeLoadTable(w http.ResponseWriter, r *http.Request, dest *tableRef) (undo func(), ok bool) {
	if dest == nil || strings.Contains(dest.TableID, "$") || (dest.ProjectID != "" && dest.ProjectID != projectOf(f.base)) {
		return nil, true
	}
	props, job := loadTableProps(r)
	l := job.Configuration.Load
	if props == nil || len(l.Schema) == 0 || string(l.Schema) == "null" || strings.EqualFold(l.CreateDisposition, "CREATE_NEVER") {
		return nil, true
	}
	if status, _ := f.get(r, tablePath(dest.DatasetID, dest.TableID)); status != http.StatusNotFound {
		return nil, true
	}
	var schema map[string]any
	if json.Unmarshal(l.Schema, &schema) != nil {
		return nil, true
	}
	table := map[string]any{
		"tableReference": map[string]string{"projectId": projectOf(f.base), "datasetId": dest.DatasetID, "tableId": dest.TableID},
		"schema":         schema,
	}
	for k, v := range props {
		table[k] = v
	}
	status, got := f.createTable(r, dest.DatasetID, table)
	switch {
	case status == http.StatusNotFound:
		return nil, true // no dataset: the load's own answer stands
	case status != http.StatusOK:
		writeError(w, status, "invalid", "The load's destination table "+dest.DatasetID+"."+dest.TableID+
			" could not be made with its timePartitioning, rangePartitioning, clustering or destinationTableProperties: "+
			errorMessage(got, status))
		return nil, false
	}
	editJob(r, func(j map[string]any) bool {
		conf, _ := j["configuration"].(map[string]any)
		load, _ := conf["load"].(map[string]any)
		if load == nil {
			return false
		}
		switch strings.ToUpper(asString(load["writeDisposition"])) {
		case "WRITE_TRUNCATE", "WRITE_TRUNCATE_DATA":
			load["writeDisposition"] = "WRITE_APPEND"
			return true
		}
		return false
	})
	return func() { f.send(r, http.MethodDelete, tablePath(dest.DatasetID, dest.TableID), nil) }, true
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// loadFailed reports whether rec, the emulator's answer to a load's
// jobs.insert, is a failure: an error, or a job with an errorResult.
func loadFailed(rec *recorder) bool {
	if rec.status != 0 && rec.status != http.StatusOK {
		return true
	}
	var job struct {
		Status struct {
			ErrorResult json.RawMessage `json:"errorResult"`
		} `json:"status"`
	}
	if json.Unmarshal(rec.body.Bytes(), &job) != nil {
		return false
	}
	return len(job.Status.ErrorResult) > 0 && string(job.Status.ErrorResult) != "null"
}

// loadNotHere is why the front does not carry out the load in r's body,
// or "" (#1457). The pinned emulator answered an ORC and a
// DATASTORE_BACKUP load 500 "runtime error: invalid memory address or nil
// pointer dereference" (measured), so each is 501 with its reason. A load
// with hivePartitioningOptions or jsonExtension GEOJSON, which it loaded
// as if neither were given, is read by the front (#1547, loadvariants.go).
func loadNotHere(r *http.Request) string {
	var job struct {
		Configuration struct {
			Load struct {
				SourceFormat    string `json:"sourceFormat"`
				FileSetSpecType string `json:"fileSetSpecType"`
			} `json:"load"`
		} `json:"configuration"`
	}
	if !decodeJob(r, &job) {
		return ""
	}
	l := job.Configuration.Load
	switch {
	case strings.EqualFold(l.SourceFormat, "ORC"):
		return "an ORC load: CloudBurrow has no ORC reader (no Go module it builds with reads ORC files; Apache Arrow's Go " +
			"module reads Parquet and Avro, not ORC)"
	case strings.EqualFold(l.SourceFormat, "DATASTORE_BACKUP"):
		return "a DATASTORE_BACKUP load: CloudBurrow does not read Datastore or Firestore managed exports"
	case l.FileSetSpecType != "" && !strings.EqualFold(l.FileSetSpecType, "FILE_SET_SPEC_TYPE_FILE_SYSTEM_MATCH"):
		return "a load with fileSetSpecType " + l.FileSetSpecType + ": CloudBurrow reads the sourceUris as the files " +
			"themselves, not as manifests"
	}
	return ""
}
