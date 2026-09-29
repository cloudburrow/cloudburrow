package bigqueryfront

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
)

// Parquet loads (#970, #988).
//
// BigQuery reads a Parquet file's schema from the file: "the table schema
// is automatically retrieved from the self-describing source data", and
// of several files "the alphabetically last file is used"
// (https://cloud.google.com/bigquery/docs/loading-data-cloud-storage-parquet).
// The emulator reads a Parquet file's columns only from the job's
// configuration.load.schema, and each row's values by those names, with no
// check of the file's columns or types (its source, server/handler.go,
// `case "PARQUET"`): measured through the official Go client, a load with
// no schema was 500 "nil pointer dereference"; given the table's schema,
// a file column the table lacks was dropped, a table column the file
// lacks (or names in another case) loaded NULL, and a file INT64 column
// loaded into a STRING one as text.
//
// So the front reads the file's schema, from its footer (parquetfooter.go),
// before anything is sent: for an upload from the request's data, for a
// load from Cloud Storage from the instance's Cloud Storage. It holds the
// file's columns to BigQuery's rules, answers 400 what BigQuery refuses
// and 501 what the emulator would not load as BigQuery does, and sends
// the emulator the schema it must read the file by.
//
// The rules, from BigQuery's documentation:
//
//   - Types, by the "Parquet conversions" table of the page above, and
//     nested and repeated columns by the same page's rules for groups,
//     LIST and MAP (parquetshape.go). Any type the table does not list is
//     501.
//   - Into a table that does not exist, the table is made with the file's
//     schema, a REQUIRED column REQUIRED and any other NULLABLE.
//   - Appending (WRITE_APPEND, WRITE_EMPTY), columns are matched by name,
//     at every depth, which BigQuery does without regard to case ("Column
//     names from Parquet files are treated as case-insensitive"): a file
//     column the table lacks is a new column, allowed only with
//     ALLOW_FIELD_ADDITION ("Add columns in a load append job",
//     https://cloud.google.com/bigquery/docs/managing-table-schemas); a
//     NULLABLE file column into a REQUIRED one is a relaxation, allowed
//     only with ALLOW_FIELD_RELAXATION (same page); a column of another
//     type is a change of type, which "isn't supported" (same page). Each
//     is 400 without its option. A table column the file lacks is left
//     NULL, as the LOAD DATA statement documents for a self-describing
//     file ("Columns in the column_list that don't exist in the source
//     file are written with NULL values",
//     https://cloud.google.com/bigquery/docs/reference/standard-sql/load-statements);
//     a REQUIRED one cannot be, and is 400.
//   - Replacing the table (WRITE_TRUNCATE), "the schema of the data you're
//     loading is used to overwrite the existing table's schema" (same
//     page). WRITE_TRUNCATE_DATA keeps the table's schema, and is checked
//     as an append is.
//   - Several files of a load from Cloud Storage: "identical columns
//     specified in multiple schemas must have the same mode", so files
//     whose columns differ in mode only are 400; files whose schemas differ
//     otherwise are 501, as the front does not reproduce which file's
//     schema BigQuery takes.
//   - A load whose job gives a schema: BigQuery's documentation does not
//     say how it reads a Parquet file by a given schema, so a schema that
//     is not the file's is 501, and so is referenceFileSchemaUri. The
//     file's own schema is loaded as if none were given, and so is one
//     that gives NULLABLE a column the file has REQUIRED (which loads the
//     same values), a new table then taking the given schema; which modes
//     BigQuery gives such a table is not measured.
//
// A load the emulator carries out as BigQuery does is sent to it with the
// schema it is to read the file by: flat columns of the types it loads
// correctly (parquetType: measured, TestBigQueryParquetLoadTypes), but for
// FLOAT and DOUBLE (a NaN, which the front cannot see in a file it does
// not read, it would store as NULL), into a new table, appended to a table
// whose columns the file names exactly, or replacing one whose schema is
// the file's. Any other the front carries out itself (parquetload.go).

// parquetJob is the part of a Parquet load's job the front reads beyond
// jobBody.
type parquetJob struct {
	Configuration struct {
		Load struct {
			ReferenceFileSchemaURI string   `json:"referenceFileSchemaUri"`
			CreateDisposition      string   `json:"createDisposition"`
			DecimalTargetTypes     []string `json:"decimalTargetTypes"`
			ParquetOptions         *struct {
				EnumAsString        bool   `json:"enumAsString"`
				EnableListInference bool   `json:"enableListInference"`
				MapTargetType       string `json:"mapTargetType"`
			} `json:"parquetOptions"`
		} `json:"load"`
	} `json:"configuration"`
}

// parquetSchema checks a Parquet load (above). A load the emulator carries
// out as BigQuery does is given the schema it is to read the file by, in
// r's body and in job, and parquetSchema reports true; r's body may have
// been replaced by an equal one. Any other it answers, carrying the load
// out itself (parquetOwnLoad) or refusing it, and reports false.
func (f front) parquetSchema(w http.ResponseWriter, r *http.Request, job *jobBody) bool {
	l := job.Configuration.Load
	if l == nil || !strings.EqualFold(l.SourceFormat, "PARQUET") || l.DestinationTable == nil {
		return true
	}
	dest := l.DestinationTable
	name := dest.DatasetID + "." + dest.TableID
	// upload is the upload's data, kept while the file is read (a load
	// from Cloud Storage has none).
	var upload *spooled
	closeBody := func() {
		if upload != nil {
			upload.Close()
		}
	}
	notImplemented := func(why string) bool {
		closeBody()
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a Parquet load "+why+
			". Nothing was loaded.")
		return false
	}
	invalid := func(msg string) bool {
		closeBody()
		writeError(w, http.StatusBadRequest, "invalid", msg+" Nothing was loaded.")
		return false
	}
	var extra parquetJob
	_ = decodeJob(r, &extra)
	el := extra.Configuration.Load
	if el.ReferenceFileSchemaURI != "" {
		return notImplemented("with referenceFileSchemaUri, which CloudBurrow does not read")
	}
	if dest.ProjectID != "" && dest.ProjectID != projectOf(f.base) {
		return notImplemented("into another project's table, " + dest.ProjectID + ":" + name +
			", whose schema CloudBurrow does not check the file's columns against")
	}
	opts := pqOptions{decimalTypes: el.DecimalTargetTypes}
	if po := el.ParquetOptions; po != nil {
		opts.enumAsString, opts.listInference = po.EnumAsString, po.EnableListInference
		switch strings.ToUpper(po.MapTargetType) {
		case "", "MAP_TARGET_TYPE_UNSPECIFIED":
		case "ARRAY_OF_STRUCT":
			opts.mapArray = true
		default:
			return invalid("Invalid value for parquetOptions.mapTargetType: " + po.MapTargetType + ".")
		}
	}

	// The file's schema.
	cols, files, status, msg := f.parquetFileColumns(r, l.SourceURIs, opts)
	upload, _ = r.Body.(*spooled)
	switch status {
	case http.StatusBadRequest:
		return invalid(msg)
	case http.StatusNotImplemented:
		return notImplemented(msg)
	case 0:
	default:
		closeBody()
		writeError(w, status, "notFound", msg)
		return false
	}
	for _, c := range cols {
		if c.notHere != "" {
			return notImplemented("of a file whose column " + c.notHere)
		}
	}
	file := make([]field, len(cols))
	for i, c := range cols {
		file[i] = c.field
	}
	if l.Schema != nil && !givenFits(l.Schema.Fields, file) {
		return notImplemented("whose job gives a schema that is not the file's (" + describeFields(l.Schema.Fields) +
			"; the file's is " + describeFields(file) + "). BigQuery reads a Parquet file by the file's schema, and its " +
			"documentation does not say how it reads one by another; CloudBurrow does not guess. Leave the schema out")
	}
	plan := &pqPlan{dest: tableRef{DatasetID: dest.DatasetID, TableID: dest.TableID}, files: files, upload: upload,
		createNever: strings.EqualFold(el.CreateDisposition, "CREATE_NEVER")}
	// passes: the emulator's own load would carry this one out as BigQuery
	// does.
	passes := true
	for _, c := range cols {
		if c.how != pqHowValue || !c.passes || legacyType(c.Type) == "FLOAT" {
			passes = false
		}
	}
	write := strings.ToUpper(l.WriteDisposition)

	status, got := f.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)+"/tables/"+url.PathEscape(dest.TableID))
	var send []field
	var raw json.RawMessage
	switch {
	case status == http.StatusNotFound:
		// A new table, made with the file's schema. With CREATE_NEVER the
		// emulator's answer, not found, stands.
		plan.kind = pqLoadNew
		send = file
		if l.Schema != nil {
			// A given schema that relaxes the file's REQUIRED columns.
			send = l.Schema.Fields
		}
		plan.target = send
		plan.raw = rawFields(send, nil)
		raw, _ = json.Marshal(plan.raw)
	case status != http.StatusOK:
		return notImplemented(fmt.Sprintf("into %s, whose schema CloudBurrow could not read (the emulator answered HTTP %d)", name, status))
	default:
		var meta struct {
			Type   string `json:"type"`
			Schema *struct {
				Fields json.RawMessage `json:"fields"`
			} `json:"schema"`
		}
		table, ok := decodeMap(got)
		if json.Unmarshal(got, &meta) != nil || !ok {
			return notImplemented("into " + name + ", whose schema CloudBurrow could not read")
		}
		if meta.Type != "" && !strings.EqualFold(meta.Type, "TABLE") {
			return notImplemented("into " + name + ", which is a " + meta.Type)
		}
		var tableRaw []any
		if meta.Schema != nil && len(meta.Schema.Fields) > 0 && string(meta.Schema.Fields) != "null" {
			if json.Unmarshal(meta.Schema.Fields, &send) != nil || json.Unmarshal(meta.Schema.Fields, &tableRaw) != nil {
				return notImplemented("into " + name + ", whose schema CloudBurrow could not read")
			}
			raw = meta.Schema.Fields
		}
		plan.table, plan.old = table, send
		var why string
		code := 0
		switch write {
		case "WRITE_TRUNCATE":
			plan.kind, plan.target = pqLoadTruncate, send
			if !sameFields(send, file) {
				plan.kind, plan.target, passes = pqLoadReplace, file, false
			}
		case "WRITE_TRUNCATE_DATA":
			plan.kind, passes = pqLoadTruncateData, false
			var m pqMerge
			plan.target, code, why = mergeParquet(send, file, nil, dest, projectOf(f.base), &m)
		case "", "WRITE_APPEND", "WRITE_EMPTY":
			plan.kind, plan.writeEmpty = pqLoadAppend, write == "WRITE_EMPTY"
			var m pqMerge
			plan.target, code, why = mergeParquet(send, file, l.SchemaUpdateOptions, dest, projectOf(f.base), &m)
			plan.relaxed = m.relaxed
			if m.added {
				plan.kind = pqLoadAdd
			}
			if m.added || m.relaxed || m.renamed {
				passes = false
			}
		default:
			return invalid("Invalid value for writeDisposition: " + l.WriteDisposition + ".")
		}
		switch code {
		case http.StatusBadRequest:
			return invalid(why)
		case http.StatusNotImplemented:
			return notImplemented(why)
		}
		if plan.kind == pqLoadReplace {
			plan.raw = rawFields(plan.target, nil)
		} else {
			plan.raw = rawFields(plan.target, tableRaw)
		}
		if raw == nil {
			// A table with no columns: mergeParquet refused any file with
			// one, so this is a file with none.
			raw = json.RawMessage("[]")
		}
	}
	if !passes {
		f.parquetOwnLoad(w, r, plan)
		return false
	}

	if !editJob(r, func(j map[string]any) bool {
		conf, _ := j["configuration"].(map[string]any)
		load, _ := conf["load"].(map[string]any)
		if load == nil {
			return false
		}
		load["schema"] = map[string]any{"fields": raw}
		return true
	}) {
		closeBody()
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not give the Parquet load its schema")
		return false
	}
	l.Schema = &tableSchema{Fields: send}
	return true
}

// pqMerge is what mergeParquet found: columns added, relaxed, or named in
// another case than the table's.
type pqMerge struct{ added, relaxed, renamed bool }

// mergeParquet checks a file's columns, file, appended to a table whose
// columns are table (the rules above), and returns the table's columns
// after the load; or 400 or 501 and why.
func mergeParquet(table, file []field, updates []string, dest *tableRef, project string, m *pqMerge) ([]field, int, string) {
	has := func(opt string) bool {
		for _, u := range updates {
			if strings.EqualFold(u, opt) {
				return true
			}
		}
		return false
	}
	ref := project + ":" + dest.DatasetID + "." + dest.TableID
	return mergeFields(table, file, has("ALLOW_FIELD_ADDITION"), has("ALLOW_FIELD_RELAXATION"), "", ref, m)
}

func mergeFields(table, file []field, add, relax bool, prefix, ref string, m *pqMerge) ([]field, int, string) {
	mismatch := "Provided Schema does not match Table " + ref + "."
	target := append([]field{}, table...)
	at := map[string]int{}
	for i, c := range table {
		at[strings.ToLower(c.Name)] = i
	}
	inFile := map[string]bool{}
	for _, c := range file {
		inFile[strings.ToLower(c.Name)] = true
		i, ok := at[strings.ToLower(c.Name)]
		name := prefix + c.Name
		if !ok {
			switch {
			case add && modeOf(c) == "REQUIRED":
				return nil, http.StatusNotImplemented, "with ALLOW_FIELD_ADDITION whose file adds a REQUIRED column, " + name +
					", to " + ref + "; BigQuery allows \"adding a nullable field to the schema\", and its documentation does not " +
					"say what it does with a REQUIRED one"
			case add:
				// At the end of the table or of its RECORD: "New columns
				// and nested fields are always added at the end of the
				// table or field" (Modifying table schemas, #1068).
				target = append(target, c)
				m.added = true
				continue
			}
			return nil, http.StatusBadRequest, mismatch + " Cannot add fields (field: " + name + "): the Parquet file has a " +
				"column the table does not. Set schemaUpdateOptions ALLOW_FIELD_ADDITION to add it."
		}
		t := table[i]
		tname := prefix + t.Name
		if t.Name != c.Name {
			m.renamed = true
		}
		repeatedT, repeatedC := modeOf(t) == "REPEATED", modeOf(c) == "REPEATED"
		switch {
		case repeatedT != repeatedC || isRecord(t.Type) != isRecord(c.Type):
			return nil, http.StatusBadRequest, mismatch + " Field " + tname + " has changed type from " + describeField(t) +
				" to " + describeField(c) + "."
		case bqType(t.Type) == "GEOGRAPHY" && c.Type == "STRING":
			return nil, http.StatusNotImplemented, "of a STRING column, " + name + ", into " + ref + "'s GEOGRAPHY column; " +
				"CloudBurrow does not load geography from Parquet"
		case bqType(t.Type) != bqType(c.Type):
			return nil, http.StatusBadRequest, mismatch + " Field " + tname + " has changed type from " +
				legacyType(t.Type) + " to " + c.Type + "."
		case modeOf(t) == "REQUIRED" && modeOf(c) != "REQUIRED" && relax:
			target[i].Mode = "NULLABLE"
			m.relaxed = true
		case modeOf(t) == "REQUIRED" && modeOf(c) != "REQUIRED":
			return nil, http.StatusBadRequest, mismatch + " Field " + tname + " has changed mode from REQUIRED to NULLABLE: " +
				"the Parquet file's column is optional. Set schemaUpdateOptions ALLOW_FIELD_RELAXATION to relax it."
		}
		if isRecord(t.Type) {
			kids, code, why := mergeFields(t.Fields, c.Fields, add, relax, tname+".", ref, m)
			if code != 0 {
				return nil, code, why
			}
			target[i].Fields = kids
		}
	}
	for _, t := range table {
		if !inFile[strings.ToLower(t.Name)] && modeOf(t) == "REQUIRED" {
			return nil, http.StatusBadRequest, mismatch + " Field " + prefix + t.Name + " is missing in new schema: the " +
				"table's column is REQUIRED and the Parquet file has no such column."
		}
	}
	return target, 0, ""
}

// bqType is a field type's name for comparing: its legacy name, and the
// decimal types' own for their aliases.
func bqType(t string) string {
	switch t = legacyType(t); t {
	case "DECIMAL":
		return "NUMERIC"
	case "BIGDECIMAL":
		return "BIGNUMERIC"
	}
	return t
}

// rawFields writes fields as a TableSchema's fields, each one table (as
// tables.get gave it) has taken from it, with its other properties
// (descriptions and the like), and its mode from fields.
func rawFields(fields []field, table []any) []any {
	byName := map[string]map[string]any{}
	for _, t := range table {
		if m, ok := t.(map[string]any); ok {
			n, _ := m["name"].(string)
			byName[strings.ToLower(n)] = m
		}
	}
	out := make([]any, len(fields))
	for i, fl := range fields {
		m := map[string]any{}
		if old, ok := byName[strings.ToLower(fl.Name)]; ok {
			for k, v := range old {
				m[k] = v
			}
		} else {
			m["name"], m["type"] = fl.Name, legacyType(fl.Type)
		}
		m["mode"] = modeOf(fl)
		if isRecord(fl.Type) {
			var kids []any
			if old, ok := byName[strings.ToLower(fl.Name)]; ok {
				kids, _ = old["fields"].([]any)
			}
			m["fields"] = rawFields(fl.Fields, kids)
		}
		out[i] = m
	}
	return out
}

// parquetFileColumns reads the columns of a Parquet load's file, as
// BigQuery reads them with opts: the data of r, a multipart upload, when
// uris is empty, else the objects uris name in the instance's Cloud
// Storage. A status of 0 means cols are the files'; any other is the
// answer, with its message.
func (f front) parquetFileColumns(r *http.Request, uris []string, opts pqOptions) (cols []*pqCol, files []pqFile, status int, msg string) {
	if len(uris) == 0 {
		elems, err := spoolParquetUpload(r)
		if err != nil {
			var bad *badParquet
			if errors.As(err, &bad) {
				return nil, nil, http.StatusBadRequest, bad.Error()
			}
			return nil, nil, http.StatusNotImplemented, "whose data CloudBurrow could not read the schema of (" + err.Error() + ")"
		}
		cols, err := parquetColumns(elems, opts)
		if err != nil {
			return nil, nil, http.StatusBadRequest, "Error while reading data: the Parquet file's schema: " + err.Error() + "."
		}
		return cols, []pqFile{{elems: elems, cols: cols}}, 0, ""
	}
	if f.storage == nil {
		return nil, nil, http.StatusNotImplemented, "from Cloud Storage with no Cloud Storage for CloudBurrow to read the " +
			"files' schema from"
	}
	objs, err := f.storage.resolve(r.Context(), uris)
	if err != nil {
		e := asLoadDataError(err)
		return nil, nil, e.code, e.msg
	}
	var first []*pqCol
	var firstURI string
	for _, o := range objs {
		elems, err := f.storage.parquetSchemaOf(r.Context(), o)
		if err != nil {
			var bad *badParquet
			if errors.As(err, &bad) {
				return nil, nil, http.StatusBadRequest, "Error while reading data: " + o.uri() + ": " + bad.Error()
			}
			var le *loadDataError
			if errors.As(err, &le) {
				return nil, nil, le.code, le.msg
			}
			return nil, nil, http.StatusNotImplemented, "whose file " + o.uri() + " CloudBurrow could not read the schema of (" +
				err.Error() + ")"
		}
		cols, err := parquetColumns(elems, opts)
		if err != nil {
			return nil, nil, http.StatusBadRequest, "Error while reading data: " + o.uri() + ": the Parquet file's schema: " + err.Error() + "."
		}
		files = append(files, pqFile{uri: o.uri(), obj: o, elems: elems})
		if first == nil {
			first, firstURI = cols, o.uri()
			continue
		}
		if msg, code := sameParquetColumns(first, cols); code != 0 {
			if code == http.StatusBadRequest {
				return nil, nil, code, "Error while reading data: " + firstURI + " and " + o.uri() + ": " + msg
			}
			return nil, nil, code, "of files whose schemas differ (" + firstURI + " and " + o.uri() + ": " + msg + "); " +
				"BigQuery then takes the schema of the alphabetically last file, which CloudBurrow does not reproduce"
		}
		files[len(files)-1].cols = cols
	}
	files[0].cols = first
	return first, files, 0, ""
}

// sameParquetColumns compares two files' columns: 0 when they are the
// same, 400 when they differ only in a column's mode (which BigQuery
// refuses), else 501.
func sameParquetColumns(a, b []*pqCol) (string, int) {
	fa, fb := make([]field, len(a)), make([]field, len(b))
	for i := range a {
		fa[i] = a[i].field
	}
	for i := range b {
		fb[i] = b[i].field
	}
	if len(a) == len(b) {
		modeOnly := ""
		same := true
		for i := range a {
			x, y := a[i], b[i]
			if x.Name != y.Name || x.Type != y.Type || x.notHere != y.notHere || !sameFields(x.Fields, y.Fields) ||
				(x.Mode == "REPEATED") != (y.Mode == "REPEATED") {
				same = false
				break
			}
			if x.Mode != y.Mode && modeOnly == "" {
				modeOnly = fmt.Sprintf("column %s is %s in one and %s in the other; identical columns must have the same "+
					"mode in each file", x.Name, x.Mode, y.Mode)
			}
		}
		if same && modeOnly == "" {
			return "", 0
		}
		if same {
			return modeOnly, http.StatusBadRequest
		}
	}
	return describeFields(fa) + " and " + describeFields(fb), http.StatusNotImplemented
}

// badParquet is data that is not a readable Parquet file, which BigQuery
// refuses (400).
type badParquet struct{ err error }

func (b *badParquet) Error() string {
	return "the data is not a Parquet file BigQuery can read: " + b.err.Error() + "."
}

// spoolParquetUpload reads the schema of the data of r, a multipart
// upload of a job and its data. The data is kept in a temporary file,
// which the schema is read from, and r's body is replaced by an equal
// multipart upload of the same job and data, which removes the file when
// it is closed.
func spoolParquetUpload(r *http.Request) ([]pqElement, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" {
		return nil, errors.New("the request is not a multipart upload")
	}
	if r.Header.Get("Content-Encoding") != "" {
		return nil, errors.New("the upload is compressed (Content-Encoding " + r.Header.Get("Content-Encoding") + ")")
	}
	body := r.Body
	mr := multipart.NewReader(body, params["boundary"])
	meta, err := mr.NextPart()
	if err != nil {
		return nil, err
	}
	job, err := io.ReadAll(io.LimitReader(meta, maxJobPart+1))
	if err != nil {
		return nil, err
	}
	if len(job) > maxJobPart {
		return nil, errors.New("the job's part is too large")
	}
	media, err := mr.NextPart()
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "bigquery-parquet-*")
	if err != nil {
		return nil, err
	}
	fail := func(err error) ([]pqElement, error) {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	size, err := io.Copy(tmp, media)
	_ = body.Close()
	if err != nil {
		return fail(err)
	}
	elems, err := readParquetSchema(tmp, size)
	if err != nil {
		return fail(&badParquet{err})
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	ct := meta.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=UTF-8"
	}
	head := []byte("--" + params["boundary"] + "\r\nContent-Type: " + ct + "\r\n\r\n")
	head = append(head, job...)
	head = append(head, []byte("\r\n--"+params["boundary"]+"\r\nContent-Type: application/octet-stream\r\n\r\n")...)
	tail := []byte("\r\n--" + params["boundary"] + "--\r\n")
	r.Body = &spooled{Reader: io.MultiReader(strings.NewReader(string(head)), io.LimitReader(tmp, size),
		strings.NewReader(string(tail))), file: tmp, size: size}
	r.ContentLength = int64(len(head)) + size + int64(len(tail))
	r.Header.Del("Content-Length")
	return elems, nil
}

// spooled is a request body read in part from a temporary file, which
// Close removes.
type spooled struct {
	io.Reader
	file *os.File
	size int64 // the data's bytes in file
	once sync.Once
}

func (s *spooled) Close() error {
	var err error
	s.once.Do(func() {
		err = s.file.Close()
		_ = os.Remove(s.file.Name())
	})
	return err
}

// parquetSchemaOf reads the schema of the Parquet object o, reading only
// its footer when Cloud Storage serves ranges.
func (s *storageReader) parquetSchemaOf(ctx context.Context, o gsObject) ([]pqElement, error) {
	var meta struct {
		Size json.Number `json:"size"`
	}
	status, err := s.do(ctx, "/storage/v1/b/"+url.PathEscape(o.bucket)+"/o/"+url.PathEscape(o.name), &meta)
	if err != nil {
		return nil, s.unreachable(o.uri(), err)
	}
	if status != http.StatusOK {
		return nil, notFoundURI(o.uri(), status)
	}
	size, err := meta.Size.Int64()
	if err != nil {
		return nil, fmt.Errorf("its size %q: %w", meta.Size, err)
	}
	elems, err := readParquetSchema(&rangeReader{ctx: ctx, s: s, o: o}, size)
	if err != nil && (errors.Is(err, errNotParquet) || strings.HasPrefix(err.Error(), "parquet metadata")) {
		return nil, &badParquet{err}
	}
	return elems, err
}

// rangeReader reads an object's bytes by ranged reads.
type rangeReader struct {
	ctx context.Context
	s   *storageReader
	o   gsObject
}

func (rr *rangeReader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(rr.ctx, http.MethodGet,
		rr.s.base+"/storage/v1/b/"+url.PathEscape(rr.o.bucket)+"/o/"+url.PathEscape(rr.o.name)+"?alt=media", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1))
	resp, err := rr.s.client.Do(req)
	if err != nil {
		return 0, rr.s.unreachable(rr.o.uri(), err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		// The whole object: skip to the range.
		if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
			return 0, err
		}
	default:
		return 0, notFoundURI(rr.o.uri(), resp.StatusCode)
	}
	return io.ReadFull(resp.Body, p)
}

// bqColumnName is BigQuery's column name rule, without flexible column
// names: letters, digits and underscores, not starting with a digit, at
// most 300 characters.
var bqColumnName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,299}$`)

// parquetAnnotation is a column's logical type, from its LogicalType or,
// in a file written without one, its ConvertedType: "" for none,
// "STRING", "DATE", "TIMESTAMP(MICROS)", "INT(8,signed)" and so on.
func parquetAnnotation(e pqElement) string {
	unit := func(u int) string {
		return map[int]string{1: "MILLIS", 2: "MICROS", 3: "NANOS"}[u]
	}
	sign := func(s bool) string {
		if s {
			return "signed"
		}
		return "unsigned"
	}
	if l := e.Logical; l != nil {
		switch l.Kind {
		case pqLogicalString:
			return "STRING"
		case pqLogicalMap:
			return "MAP"
		case pqLogicalList:
			return "LIST"
		case pqLogicalEnum:
			return "ENUM"
		case pqLogicalDecimal:
			return "DECIMAL"
		case pqLogicalDate:
			return "DATE"
		case pqLogicalTime:
			return "TIME(" + unit(l.Unit) + ")"
		case pqLogicalTimestamp:
			return "TIMESTAMP(" + unit(l.Unit) + ")"
		case pqLogicalInteger:
			return fmt.Sprintf("INT(%d,%s)", l.BitWidth, sign(l.Signed))
		case pqLogicalUnknown:
			return "UNKNOWN"
		case pqLogicalJSON:
			return "JSON"
		case pqLogicalBSON:
			return "BSON"
		case pqLogicalUUID:
			return "UUID"
		}
		return fmt.Sprintf("LogicalType %d", l.Kind)
	}
	switch e.Converted {
	case pqNone:
		return ""
	case pqConvUTF8:
		return "STRING"
	case pqConvMap, pqConvMapKeyValue:
		return "MAP"
	case pqConvList:
		return "LIST"
	case pqConvEnum:
		return "ENUM"
	case pqConvDecimal:
		return "DECIMAL"
	case pqConvDate:
		return "DATE"
	case pqConvTimeMillis:
		return "TIME(MILLIS)"
	case pqConvTimeMicros:
		return "TIME(MICROS)"
	case pqConvTimestampMillis:
		return "TIMESTAMP(MILLIS)"
	case pqConvTimestampMicros:
		return "TIMESTAMP(MICROS)"
	case pqConvUint8, pqConvUint16, pqConvUint32, pqConvUint64:
		return fmt.Sprintf("INT(%d,unsigned)", 8<<(e.Converted-pqConvUint8))
	case pqConvInt8, pqConvInt16, pqConvInt32, pqConvInt64:
		return fmt.Sprintf("INT(%d,signed)", 8<<(e.Converted-pqConvInt8))
	case pqConvJSON:
		return "JSON"
	case pqConvBSON:
		return "BSON"
	case pqConvInterval:
		return "INTERVAL"
	}
	return fmt.Sprintf("ConvertedType %d", e.Converted)
}

var parquetPhysical = []string{"BOOLEAN", "INT32", "INT64", "INT96", "FLOAT", "DOUBLE", "BYTE_ARRAY", "FIXED_LEN_BYTE_ARRAY"}

// describeParquet names a column's Parquet type, as "INT64 (TIMESTAMP(MILLIS))".
func describeParquet(e pqElement) string {
	s := "a group"
	if e.Type >= 0 && e.Type < len(parquetPhysical) {
		s = parquetPhysical[e.Type]
	} else if e.Children < 0 {
		s = fmt.Sprintf("physical type %d", e.Type)
	}
	if a := parquetAnnotation(e); a != "" {
		s += " (" + a + ")"
	}
	return s
}

// parquetType returns the BigQuery type BigQuery's Parquet conversion
// table gives a column's Parquet type ("" for a combination it does not
// list: "Other combinations of Parquet types and converted types are not
// supported"), and whether the emulator loads it as BigQuery does.
//
// Measured through the official Go client, each given its BigQuery type
// in the job's schema (TestBigQueryParquetLoadTypes): the emulator loads
// BOOLEAN; INT32 with no annotation, INT(8/16/32) and DATE; INT64 with no
// annotation and TIMESTAMP(MICROS); FLOAT; DOUBLE; and BYTE_ARRAY STRING
// correctly. It loaded a TIME(MILLIS) 01:02:03.004 as 00:12:06.004, a
// TIME(MICROS) 01:02:03.004567 as 01:02:07.567, a TIMESTAMP(MILLIS) in
// 2024 as one in January 1970, a BYTE_ARRAY BYTES value 0xFF as EF BF BD,
// and refused INT96 ("failed to convert ... to time.Time"). DECIMAL
// depends on decimalTargetTypes, ENUM on parquetOptions, and an unsigned
// INT64 is refused above the INT64 maximum, none of which the emulator
// reads.
func parquetType(e pqElement) (bq string, loads bool) {
	if e.Children >= 0 {
		return "RECORD", false
	}
	a := parquetAnnotation(e)
	switch e.Type {
	case pqBoolean:
		if a == "" {
			return "BOOLEAN", true
		}
	case pqInt32:
		switch a {
		case "", "INT(8,signed)", "INT(16,signed)", "INT(32,signed)", "INT(8,unsigned)", "INT(16,unsigned)", "INT(32,unsigned)":
			return "INTEGER", true
		case "DECIMAL":
			return "NUMERIC", false
		case "DATE":
			return "DATE", true
		case "TIME(MILLIS)":
			return "TIME", false
		}
	case pqInt64:
		switch a {
		case "", "INT(64,signed)":
			return "INTEGER", true
		case "INT(64,unsigned)":
			return "INTEGER", false
		case "DECIMAL":
			return "NUMERIC", false
		case "TIME(MICROS)":
			return "TIME", false
		case "TIMESTAMP(MILLIS)":
			return "TIMESTAMP", false
		case "TIMESTAMP(MICROS)":
			return "TIMESTAMP", true
		}
	case pqInt96:
		if a == "" {
			return "TIMESTAMP", false
		}
	case pqFloat, pqDouble:
		if a == "" {
			return "FLOAT", true
		}
	case pqByteArray:
		switch a {
		case "":
			return "BYTES", false
		case "STRING":
			return "STRING", true
		case "ENUM":
			return "STRING", false
		}
	case pqFixedLenByteArray:
		switch a {
		case "":
			return "BYTES", false
		case "DECIMAL":
			return "NUMERIC", false
		}
	}
	return "", false
}

// legacyType is a field type's legacy name, which the emulator's
// tables.get and parquetType use.
func legacyType(t string) string {
	switch t = strings.ToUpper(t); t {
	case "INT64":
		return "INTEGER"
	case "FLOAT64":
		return "FLOAT"
	case "BOOL":
		return "BOOLEAN"
	case "STRUCT":
		return "RECORD"
	}
	return t
}

func isRecord(t string) bool { return legacyType(t) == "RECORD" }

func modeOf(f field) string {
	if f.Mode == "" {
		return "NULLABLE"
	}
	return strings.ToUpper(f.Mode)
}

// sameFields reports whether two schemas are the same: names, types and
// modes, in order, at every depth.
func sameFields(a, b []field) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || bqType(a[i].Type) != bqType(b[i].Type) || modeOf(a[i]) != modeOf(b[i]) ||
			!sameFields(a[i].Fields, b[i].Fields) {
			return false
		}
	}
	return true
}

// givenFits reports whether a load's given schema is its file's, but for
// top-level columns the file has REQUIRED that it gives NULLABLE.
func givenFits(given, file []field) bool {
	if len(given) != len(file) {
		return false
	}
	for i := range given {
		g, f := given[i], file[i]
		if g.Name != f.Name || bqType(g.Type) != bqType(f.Type) || !sameFields(g.Fields, f.Fields) ||
			modeOf(g) != modeOf(f) && !(modeOf(g) == "NULLABLE" && modeOf(f) == "REQUIRED") {
			return false
		}
	}
	return true
}

func describeField(f field) string {
	s := legacyType(f.Type)
	if m := modeOf(f); m != "NULLABLE" {
		s = m + " " + s
	}
	return s
}

// describeFields writes a schema as "a INTEGER, b REQUIRED STRING".
func describeFields(fs []field) string {
	if len(fs) == 0 {
		return "no columns"
	}
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = f.Name + " " + describeField(f)
	}
	return strings.Join(parts, ", ")
}
