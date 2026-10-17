package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// columnNameCharacterMap V1 and V2 of a CSV load with autodetect (#1326).
//
// BigQuery's load reference (column_name_character_map,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/load-statements):
// under V1 "Column names can only contain standard column name characters"
// and under V2, "Besides standard column name characters, it also supports
// flexible column names"; under both, unsupported characters "(except
// periods in Parquet file column names) are replaced with underscores".
// The emulator ignores the map and keeps a detected name as it is in the
// data (measured: "a!" kept under V2), so the front renames the columns of
// the table the load made: each character the map's rule does not take
// (classicColumnRune for V1, the flexible rule's for V2) becomes "_", and
// the table is made again with the new names, its rows copied through a
// scratch table (scratch.go), as remakeFloat does.
//
// A name still refused once its characters are replaced (a V1 name that
// starts with a digit, an empty or reserved name, one too long), and names
// that are the same once replaced, are not renamed: BigQuery's rule for
// them is not documented, so the load stays 501 (autodetectLoad).
//
// A Parquet load (#1373) is renamed as the front reads the file's schema
// (pqOptions.name, parquetshape.go): each name, at every depth, the same
// way but for its periods, which BigQuery keeps ("except periods in
// Parquet file column names"); the front then carries out the load itself
// (parquetload.go), as the emulator reads the file by the schema's names,
// each value going to the column of its renamed field. The cases the CSV
// path leaves 501 are 501 here too: a name still refused once replaced (a
// period kept is one, as no rule takes it), and names the same once
// replaced.

// classicColumnRune is a character V1's rule takes.
var classicColumnRune = regexp.MustCompile(`^[A-Za-z0-9_]$`)

// flexibleColumnRune is a character V2's, the flexible rule, takes
// (columnNamePattern, names.go).
var flexibleColumnRune = regexp.MustCompile(`^[\p{L}\p{N}\p{Pc}\p{Pd}\p{M}&%=+:'<>#|\s]$`)

// mapColumnName is name under charMap, V1 or V2: each character the rule
// does not take replaced with "_".
func mapColumnName(name, charMap string) string {
	keep := flexibleColumnRune
	if charMap == "V1" {
		keep = classicColumnRune
	}
	var b strings.Builder
	for _, c := range name {
		if keep.MatchString(string(c)) {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// mappedNames are the fields' names under charMap, and whether any
// changed, or ok false when a mapped name is still refused by check or two
// are the same (above).
func mappedNames(fields []field, charMap string, check func(string) string) (names []string, changed, ok bool) {
	seen := map[string]bool{}
	for _, fl := range fields {
		n := mapColumnName(fl.Name, charMap)
		key := strings.ToLower(n)
		if check(n) != "" || seen[key] {
			return nil, false, false
		}
		seen[key] = true
		changed = changed || n != fl.Name
		names = append(names, n)
	}
	return names, changed, true
}

// renameLoadedColumns makes ref, the table a load has just made with
// schema (raw, as tables.get gave it), again with its top-level columns
// named names, in order. It returns the new schema, or why it could not.
// When it fails before the table is deleted, the table is left as the
// load made it.
func (f front) renameLoadedColumns(r *http.Request, ref tableRef, raw json.RawMessage, fields []field, names []string) (json.RawMessage, string) {
	var s struct {
		Fields []map[string]any `json:"fields"`
	}
	if json.Unmarshal(raw, &s) != nil || len(s.Fields) != len(names) {
		return nil, "could not read the loaded table's schema"
	}
	for i := range s.Fields {
		s.Fields[i]["name"] = names[i]
	}
	schema, err := json.Marshal(s)
	if err != nil {
		return nil, err.Error()
	}
	scratch := tableRef{DatasetID: resultsDataset, TableID: scratchTable()} // scratch.go
	if why := f.makeTable(r, scratch, schema); why != "" {
		return nil, why
	}
	dropScratch := func() { f.send(r, http.MethodDelete, tablePath(scratch.DatasetID, scratch.TableID), nil) }
	to, from := make([]string, len(names)), make([]string, len(names))
	for i := range names {
		to[i], from[i] = quoteName(names[i]), quoteName(fields[i].Name)
	}
	legacy := false
	body, err := json.Marshal(queryOptions{Query: "INSERT INTO " + quotePath([]string{scratch.DatasetID, scratch.TableID}) +
		" (" + strings.Join(to, ", ") + ") SELECT " + strings.Join(from, ", ") + " FROM " +
		quotePath([]string{ref.DatasetID, ref.TableID}), UseLegacySQL: &legacy})
	if err != nil {
		dropScratch()
		return nil, err.Error()
	}
	if status, got := f.send(r, http.MethodPost, "/queries", body); status != http.StatusOK {
		dropScratch()
		return nil, errorMessage(got, status)
	}
	renamed := make([]field, len(fields))
	for i, fl := range fields {
		fl.Name = names[i]
		renamed[i] = fl
	}
	f.send(r, http.MethodDelete, tablePath(ref.DatasetID, ref.TableID), nil)
	if why := f.makeTable(r, ref, schema); why != "" {
		return nil, why + "; the loaded rows are in " + scratch.DatasetID + "." + scratch.TableID
	}
	if why := f.insertSelect(r, ref, []copyTable{{ref: scratch, fields: renamed}}); why != "" {
		return nil, why + "; the loaded rows are in " + scratch.DatasetID + "." + scratch.TableID
	}
	dropScratch()
	return schema, ""
}

// applyCharacterMap renames the columns of the table a CSV load has just
// made, path its tables.get path and got its answer, to charMap's rules,
// and returns the table's tables.get answer after, nil when no name
// changed or one cannot be renamed (left to autodetectLoad's checks), or
// why the table could not be made again.
func (f front) applyCharacterMap(r *http.Request, ref tableRef, path, charMap string, check func(string) string,
	got []byte, fields []field) ([]byte, string) {
	names, changed, ok := mappedNames(fields, charMap, check)
	if !ok || !changed {
		return nil, ""
	}
	var raw struct {
		Schema json.RawMessage `json:"schema"`
	}
	if json.Unmarshal(got, &raw) != nil {
		return nil, "could not read the loaded table"
	}
	if _, why := f.renameLoadedColumns(r, ref, raw.Schema, fields, names); why != "" {
		return nil, why
	}
	status, after := f.get(r, path)
	if status != http.StatusOK {
		return nil, errorMessage(after, status)
	}
	return after, ""
}

// failLoadedRename answers a load whose table applyCharacterMap could not
// make again: the job, rec's answer, fails with why, and jobs.get and
// jobs.list report it failed (jobFailures).
func (f front) failLoadedRename(w http.ResponseWriter, rec *recorder, job jobBody, why string) {
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
	e := rowError{Reason: "backendError", Message: "CloudBurrow could not rename the loaded table's columns to the load's " +
		"columnNameCharacterMap in the emulator behind it, which keeps a detected name as it is (#1326): " + why}
	f.failed.add(project, id, e)
	if !ok {
		writeError(w, http.StatusInternalServerError, e.Reason, e.Message)
		return
	}
	failJob(resp, e)
	writeJSON(w, http.StatusOK, resp)
}

// charMapFormat is why a load's columnNameCharacterMap, charMap, is refused
// for its sourceFormat, or "" (#1336). BigQuery documents the map as the
// "Character map supported for column names in CSV/Parquet loads", and
// "Using this option with unsupporting load formats will result in an
// error" (JobConfigurationLoad); the LOAD DATA statement's
// column_name_character_map "Applies to CSV and Parquet data". The
// option is any value given but COLUMN_NAME_CHARACTER_MAP_UNSPECIFIED.
// The error's wording is CloudBurrow's (UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/schemas
func charMapFormat(charMap, format string) string {
	m := strings.ToUpper(charMap)
	if m == "" || m == "COLUMN_NAME_CHARACTER_MAP_UNSPECIFIED" {
		return ""
	}
	f := strings.ToUpper(format)
	if f == "" || f == "CSV" || f == "PARQUET" {
		return ""
	}
	return fmt.Sprintf("columnNameCharacterMap %s is not supported for sourceFormat %s: the column name character map "+
		"applies to CSV and Parquet loads only.", charMap, format)
}

// parquetCharMapNames is why the column names of a Parquet file, file,
// are refused under the load's columnNameCharacterMap, with the status:
// under STRICT (or none) a name the column rules refuse is 400, as
// BigQuery documents ("Load jobs with unsupported characters in column
// names fail with an error message") (#1336); under V1 or V2, file holds
// the names once their unsupported characters are replaced with
// underscores, "except periods in Parquet file column names" (#1373,
// pqOptions.name), and one still refused is 501, as BigQuery does not
// document how it names it; one refused only for its periods, which the
// map keeps, is 400 (#1547, below).
func parquetCharMapNames(file []field, charMap string) (int, string) {
	m := strings.ToUpper(charMap)
	check := checkColumnName
	if m == "V1" {
		check = checkClassicColumnName
	}
	bad := checkNames(file, "", nil, check)
	if bad == "" {
		return 0, ""
	}
	if m == "V1" || m == "V2" {
		if name := periodOnly(file, check); name != "" {
			// #1547, UNVERIFIED: "Periods are not replaced by underscores
			// in Parquet file column names when a column name character
			// map is used" (Specifying a schema, flexible column names,
			// read 2026-10-17), a period is not a supported character
			// ("A period (.)"), and "Load jobs with unsupported characters
			// in column names fail with an error message": the load fails.
			// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/schemas
			return http.StatusBadRequest, fmt.Sprintf("Invalid field name %q of the Parquet file: under "+
				"columnNameCharacterMap %s, \"Periods are not replaced by underscores in Parquet file column names\", "+
				"and a column name cannot hold a period.", name, m)
		}
		return http.StatusNotImplemented, "under columnNameCharacterMap " + m + " of a file whose column is still refused " +
			"once its unsupported characters (but periods) are replaced with underscores: " + bad + " BigQuery's " +
			"documentation does not say how it names such a column (\"Invalid column names will be normalized\")"
	}
	return http.StatusBadRequest, bad + " The name is the Parquet file's; columnNameCharacterMap STRICT, the default, " +
		"refuses it rather than renaming it."
}

// periodOnly is the first name in fields, at any depth, that check
// refuses, when it is refused only for its periods (#1547), or "".
func periodOnly(fields []field, check func(string) string) string {
	for _, fl := range fields {
		if check(fl.Name) != "" {
			if strings.Contains(fl.Name, ".") && check(strings.ReplaceAll(fl.Name, ".", "_")) == "" {
				return fl.Name
			}
			return ""
		}
		if n := periodOnly(fl.Fields, check); n != "" {
			return n
		}
	}
	return ""
}
