package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// FLOAT columns (#1000).
//
// In BigQuery a field of type FLOAT and one of type FLOAT64 are the same
// 64-bit type ("FLOAT (or FLOAT64)",
// https://cloud.google.com/bigquery/docs/reference/rest/v2/tables#TableFieldSchema.FIELDS.type),
// and the official Go client sends FLOAT for every float field
// (bigquery.FloatFieldType). The emulator makes a column its REST API is
// given as FLOAT as its engine's 32-bit FLOAT (its types.Type.TypeKind:
// FLOAT is zsqltypes.FLOAT, FLOAT64 zsqltypes.DOUBLE), into which the
// engine then refuses a FLOAT64 value. Measured against the pinned image
// through the front: after tables.insert of a FLOAT column f, `INSERT INTO
// ds.t (f) VALUES (CAST(0.1 AS FLOAT64))` failed 400 "Value has type
// DOUBLE which cannot be inserted into column f, which has type FLOAT", as
// did `INSERT ... SELECT` of a FLOAT64 column, a FLOAT64 struct field in a
// RECORD and a query parameter; a load with a FLOAT field in its schema,
// and a CSV load with autodetect that detected one, made the same column.
// An untyped literal (`VALUES (0.1)`) and tabledata.insertAll were
// accepted, and read back without losing precision (0.30000000000000004,
// 1.7976931348623157e308 and 123456789.12345679 read back as sent: the
// engine keeps every float as SQLite's 64-bit REAL). The other legacy
// names are the same types in the engine (INTEGER is INT64, BOOLEAN BOOL,
// RECORD STRUCT; measured: CAST(9007199254740993 AS INT64) and a BOOL
// inserted into columns made as INTEGER and BOOLEAN), so they are sent as
// they are.
//
// So the front makes each such column FLOAT64:
//
//   - tables.insert with a FLOAT field (at any depth) is sent with it as
//     FLOAT64, and the schema is then set back to the client's through
//     tables.patch, which changes only what tables.get reads (measured:
//     the engine's column stays DOUBLE), so the table reads back FLOAT as
//     it was made, as BigQuery's does. createTable.
//   - A load job's schema is sent with each FLOAT field as FLOAT64, and
//     after the load the destination's FLOAT64 fields that the client
//     sent as FLOAT are set back. loadFloat.
//   - A CSV load with autodetect into a new table that detected a FLOAT
//     column: the table is made again with the column FLOAT64 and its
//     rows copied in (as a copy job's WRITE_TRUNCATE, copyjob.go), and
//     read back as FLOAT. remakeFloat.
//
// tables.update and tables.patch are sent as they are: the emulator
// changes only the table's metadata with them, never its engine's columns
// (measured: after a patch that added a column g, `INSERT ... (g)` failed
// "Column g is not present", #1010), so a FLOAT in them makes no 32-bit
// column. A copy job makes its destination through createTable too.

// floatAs64 changes each field of type FLOAT in a TableSchema, at any
// depth, to FLOAT64, and reports whether there was one.
func floatAs64(schema map[string]any) bool {
	fields, _ := schema["fields"].([]any)
	changed := false
	for _, fl := range fields {
		m, ok := fl.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); strings.EqualFold(t, "FLOAT") {
			m["type"] = "FLOAT64"
			changed = true
		}
		if floatAs64(m) {
			changed = true
		}
	}
	return changed
}

// hasFloat reports whether a TableSchema has a field of type FLOAT.
func hasFloat(fields []field) bool {
	for _, fl := range fields {
		if strings.EqualFold(fl.Type, "FLOAT") || hasFloat(fl.Fields) {
			return true
		}
	}
	return false
}

// decodeMap reads JSON into a map, keeping numbers as they are written.
func decodeMap(b []byte) (map[string]any, bool) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return m, dec.Decode(&m) == nil && m != nil
}

// createTable sends tables.insert of table, a Table resource, into
// dataset: each FLOAT field as FLOAT64, and then the client's schema back
// through tables.patch (above). It returns the status and body to answer
// with: the patch's, which is the whole table as it now reads, or else
// the insert's.
func (f front) createTable(r *http.Request, dataset string, table map[string]any) (int, []byte) {
	schema, _ := table["schema"].(map[string]any)
	sentSchema := map[string]any{}
	if schema != nil {
		b, _ := json.Marshal(schema)
		sentSchema, _ = decodeMap(b)
	}
	changed := floatAs64(sentSchema)
	sent := map[string]any{}
	for k, v := range table {
		sent[k] = v
	}
	if changed {
		sent["schema"] = sentSchema
	}
	body, err := json.Marshal(sent)
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	status, got := f.send(r, http.MethodPost, "/datasets/"+url.PathEscape(dataset)+"/tables", body)
	if status != http.StatusOK || !changed {
		return status, got
	}
	var made struct {
		TableReference tableRef `json:"tableReference"`
	}
	if json.Unmarshal(got, &made) != nil || made.TableReference.TableID == "" {
		b, _ := json.Marshal(table["tableReference"])
		_ = json.Unmarshal(b, &made.TableReference)
	}
	patch, err := json.Marshal(map[string]any{"schema": schema})
	if err != nil || made.TableReference.TableID == "" {
		return status, got
	}
	if ps, pb := f.send(r, http.MethodPatch, tablePath(dataset, made.TableReference.TableID), patch); ps == http.StatusOK && len(pb) > 0 {
		return ps, pb
	}
	return status, got
}

// createTableFloat64 answers tables.insert whose body, b, has a FLOAT
// field through createTable, and reports whether it did; a body without
// one is left to be sent as it is.
func (f front) createTableFloat64(w http.ResponseWriter, r *http.Request, dataset string, b []byte, fields []field) bool {
	if !hasFloat(fields) || r.Header.Get("Content-Encoding") != "" {
		return false
	}
	table, ok := decodeMap(b)
	if !ok {
		return false
	}
	status, got := f.createTable(r, dataset, table)
	writeRaw(w, status, got)
	return true
}

// loadFloat sends a load job's schema, in r's body, with each FLOAT field
// as FLOAT64 (above), and returns what to call once the load has run, to
// set the destination's schema back to FLOAT where the client had it; or
// nil when the load's schema has no FLOAT field.
func (f front) loadFloat(r *http.Request, schema *tableSchema, dest *tableRef) func() {
	if schema == nil || dest == nil || !hasFloat(schema.Fields) || (dest.ProjectID != "" && dest.ProjectID != projectOf(f.base)) {
		return nil
	}
	if !editJob(r, func(job map[string]any) bool {
		conf, _ := job["configuration"].(map[string]any)
		load, _ := conf["load"].(map[string]any)
		s, _ := load["schema"].(map[string]any)
		return s != nil && floatAs64(s)
	}) {
		return nil
	}
	return func() {
		p := tablePath(dest.DatasetID, dest.TableID)
		status, got := f.get(r, p)
		if status != http.StatusOK {
			return
		}
		var meta struct {
			Schema map[string]any `json:"schema"`
		}
		dec := json.NewDecoder(bytes.NewReader(got))
		dec.UseNumber()
		if dec.Decode(&meta) != nil || meta.Schema == nil || !float64Back(meta.Schema, schema.Fields) {
			return
		}
		if patch, err := json.Marshal(map[string]any{"schema": meta.Schema}); err == nil {
			f.send(r, http.MethodPatch, p, patch)
		}
	}
}

// float64Back sets each field of have, a TableSchema as tables.get gives
// it, that reads FLOAT64 where the client's fields, sent, have the field
// of that name as FLOAT, back to the client's type; and reports whether it
// changed one.
func float64Back(have map[string]any, sent []field) bool {
	fields, _ := have["fields"].([]any)
	changed := false
	for _, fl := range fields {
		m, ok := fl.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		for _, s := range sent {
			if !strings.EqualFold(s.Name, name) {
				continue
			}
			if t, _ := m["type"].(string); strings.EqualFold(t, "FLOAT64") && strings.EqualFold(s.Type, "FLOAT") {
				m["type"] = s.Type
				changed = true
			}
			if float64Back(m, s.Fields) {
				changed = true
			}
			break
		}
	}
	return changed
}

// remakeFloat makes ref, a table a load with autodetect has just made
// with schema, again with its FLOAT columns as FLOAT64 (above), when it
// has one: its rows are copied to a scratch table made through
// createTable, the table is deleted and made again through createTable,
// and the rows copied back. It returns why it could not, or "". When it
// fails before the table is deleted, the table is left as the load made
// it.
func (f front) remakeFloat(r *http.Request, ref tableRef, schema json.RawMessage) string {
	var s tableSchema
	if json.Unmarshal(schema, &s) != nil || !hasFloat(s.Fields) {
		return ""
	}
	scratch := tableRef{DatasetID: ref.DatasetID, TableID: scratchTable()}
	if err := f.makeTable(r, scratch, schema); err != "" {
		return err
	}
	if err := f.insertSelect(r, scratch, []copyTable{{ref: ref, fields: s.Fields}}); err != "" {
		f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil)
		return err
	}
	f.send(r, http.MethodDelete, tablePath(ref.DatasetID, ref.TableID), nil)
	if err := f.makeTable(r, ref, schema); err != "" {
		return err + "; the loaded rows are in " + ref.DatasetID + "." + scratch.TableID
	}
	if err := f.insertSelect(r, ref, []copyTable{{ref: scratch, fields: s.Fields}}); err != "" {
		return err + "; the loaded rows are in " + ref.DatasetID + "." + scratch.TableID
	}
	f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil)
	return ""
}

// loadSucceeded reports whether a load job's answer, a Job resource, is
// done without an error.
func loadSucceeded(body []byte) bool {
	var job struct {
		Status struct {
			State       string          `json:"state"`
			ErrorResult json.RawMessage `json:"errorResult"`
		} `json:"status"`
	}
	return json.Unmarshal(body, &job) == nil && job.Status.State == "DONE" && len(job.Status.ErrorResult) == 0
}

// failLoadedFloat answers a load with autodetect whose table remakeFloat
// could not make again: the job, rec's answer, fails with why, and
// jobs.get and jobs.list report it failed (jobFailures).
func (f front) failLoadedFloat(w http.ResponseWriter, rec *recorder, job jobBody, why string) {
	resp, ok := decodeMap(rec.body.Bytes())
	project, id := job.JobReference.ProjectID, job.JobReference.JobID
	if ref, _ := resp["jobReference"].(map[string]any); ref != nil {
		if s, _ := ref["jobId"].(string); s != "" {
			id = s
		}
		if s, _ := ref["projectId"].(string); s != "" {
			project = s
		}
	}
	if project == "" {
		project = projectOf(f.base)
	}
	e := rowError{Reason: "backendError", Message: "CloudBurrow could not make the loaded table's FLOAT columns 64-bit in " +
		"the emulator behind it, which makes a detected FLOAT column 32-bit (#1000): " + why}
	f.failed.add(project, id, e)
	if !ok {
		writeError(w, http.StatusInternalServerError, e.Reason, e.Message)
		return
	}
	failJob(resp, e)
	writeJSON(w, http.StatusOK, resp)
}
