package bigqueryfront

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Parquet loads the front carries out itself (#1004, #1005, #1006).
//
// The emulator's own Parquet load (parquet.go) is right only for flat
// columns of some types, into the table's own schema, by the exact column
// names. Measured through the official Go client and against the pinned
// image directly, given the BigQuery schema in the job: it mis-loads
// TIME, TIMESTAMP(MILLIS), BYTES and INT96 (#988); a LIST read as BigQuery
// reads it without list inference failed ("failed to convert struct from
// string"); with list inference a NULL list read back NULL (a REPEATED
// column is never NULL in BigQuery) and a NULL element was kept; a MAP left
// the emulator hung, answering nothing until it was restarted; it changes
// no table's schema in a load, and reads a file's values by the exact
// name. The emulator's JSON load is no way round it: BYTES given as base64
// loaded the base64 text itself (measured: "/w==" read back 2f773d3d).
//
// So such a load is the front's own job (as a copy job is, copyjob.go):
// the front reads the file's rows (parquetrows.go), writes each as a
// GoogleSQL literal of the table's row type, and inserts them with DML,
// which the emulator stores exactly for every type the rows can have
// (measured: TIME, TIMESTAMP with microseconds, BYTES, NUMERIC and
// BIGNUMERIC at their limits, the INT64 limits, ±Inf, and RECORD and
// REPEATED values nested in each other read back as written). The rows go
// first to a scratch table of the table's new schema, and the table is
// changed only once they are all there, so a load that fails leaves the
// table as it was:
//
//   - Into a new table: the table is made with the file's schema (a job
//     schema that relaxes it: that), and the rows inserted.
//   - Appending: the rows are copied into the table by one INSERT ...
//     SELECT. A file's column named as a table column in another case goes
//     to that column: "Column names from Parquet files are treated as
//     case-insensitive when loaded into BigQuery".
//   - ALLOW_FIELD_RELAXATION: the table's schema is set with the column
//     NULLABLE (tables.patch; the emulator's engine does not hold a
//     REQUIRED column to it, measured: an INSERT of NULL into one was
//     taken, so no column of the engine changes).
//   - ALLOW_FIELD_ADDITION: the new columns are added at the end ("New
//     columns and nested fields are always added at the end of the
//     table", Modifying table schemas), NULL in the table's rows ("the
//     values in the new columns are set to NULL for existing rows"). The
//     emulator adds no column to its engine through tables.patch (#1010)
//     or ALTER TABLE ADD COLUMN (measured: it answered success and an
//     INSERT naming the column failed "Column c is not present"), so the
//     table is made again with the new schema, its description, labels,
//     partitioning and clustering, and its rows and the file's copied in.
//     A REQUIRED column is 501 ("allow adding a nullable field to the
//     schema"; what BigQuery does with a REQUIRED one is not documented),
//     and so is a field added inside a RECORD.
//   - WRITE_TRUNCATE: "BigQuery overwrites the data, removes the
//     constraints and uses the schema from the load job": the table is
//     made again with the file's schema (its other properties kept, but
//     for tableConstraints), when that differs, and otherwise its rows are
//     deleted. WRITE_TRUNCATE_DATA "overwrites the data, but keeps the
//     constraints and schema of the existing table": the file is checked
//     as for an append, and the rows replaced.
//
// The job is answered done, with statistics.load's inputFiles,
// inputFileBytes (the Parquet files' bytes), outputRows and badRecords 0
// (a load that succeeded read every row), and kept for jobs.get and
// jobs.list (frontJobs). A value BigQuery refuses fails the job, reason
// invalid, and nothing is loaded; one the front does not load is 501 and
// no job is made. A dry run is 501.

// Kinds of Parquet load the front carries out.
const (
	pqLoadNew          = iota // into a table that does not exist
	pqLoadAppend              // WRITE_APPEND or WRITE_EMPTY, the schema unchanged or relaxed
	pqLoadAdd                 // appending, with columns added
	pqLoadReplace             // WRITE_TRUNCATE with the file's schema, not the table's
	pqLoadTruncate            // WRITE_TRUNCATE with the table's schema
	pqLoadTruncateData        // WRITE_TRUNCATE_DATA
)

// pqPlan is a Parquet load the front carries out.
type pqPlan struct {
	kind        int
	dest        tableRef
	writeEmpty  bool
	createNever bool
	relaxed     bool
	// table is the destination's tables.get resource, when it exists.
	table map[string]any
	// old are the destination's fields before the load.
	old []field
	// target is the table's schema after the load, and raw that schema as
	// a TableSchema's fields (the table's own, with any changes), for
	// tables.insert and tables.patch.
	target []field
	raw    []any
	files  []pqFile
	upload *spooled // the upload's data, for a load of one
}

// pqFile is one file of a load: an upload's data, or an object in Cloud
// Storage.
type pqFile struct {
	uri   string
	obj   gsObject
	elems []pqElement
	cols  []*pqCol // the file's columns, read by its own schema
}

// parquetOwnLoad carries out plan (above) and answers w. The upload's data,
// for a load of one, is removed.
func (f front) parquetOwnLoad(w http.ResponseWriter, r *http.Request, plan *pqPlan) {
	if plan.upload != nil {
		defer plan.upload.Close()
	}
	notImplemented := func(why string) {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a Parquet load "+why+
			". Nothing was loaded.")
	}
	j, ok := f.startParquetJob(w, r)
	if !ok {
		return
	}
	project := projectOf(f.base)
	name := tableName(project, plan.dest)

	// The rows, as literals of the table's row type, one per line of a
	// temporary file.
	rows, err := os.CreateTemp("", "bigquery-parquet-rows-*")
	if err != nil {
		notImplemented("whose rows CloudBurrow could not keep (" + err.Error() + ")")
		return
	}
	defer func() { rows.Close(); os.Remove(rows.Name()) }()
	bw := bufio.NewWriter(rows)
	var n, bytes int64
	fail := func(e rowError) {
		f.finishOwnJob(w, r, j, "LOAD", "load", nil, &e)
	}
	for _, pf := range plan.files {
		data, size, cleanup, err := f.parquetData(r, plan.upload, pf)
		if err != nil {
			var le *loadDataError
			if errors.As(err, &le) {
				writeError(w, le.code, le.reason, le.msg)
				return
			}
			notImplemented("whose file " + pf.uri + " CloudBurrow could not read (" + err.Error() + ")")
			return
		}
		bytes += size
		index := map[string]int{}
		for i, c := range pf.cols {
			index[strings.ToLower(c.Name)] = i
		}
		got, err := readParquetRows(data, pf.elems, pf.cols, func(vals []any) error {
			parts := make([]string, len(plan.target))
			for i, t := range plan.target {
				var v any
				if k, ok := index[strings.ToLower(t.Name)]; ok {
					v = vals[k]
				}
				s, err := literalFor(t, v)
				if err != nil {
					return err
				}
				parts[i] = s
			}
			_, err := bw.WriteString("(" + strings.Join(parts, ", ") + ")\n")
			return err
		})
		cleanup()
		n += got
		if err != nil {
			in := ""
			if pf.uri != "" {
				in = " (" + pf.uri + ")"
			}
			var ve *pqValueError
			switch {
			case errors.As(err, &ve) && ve.status == http.StatusBadRequest:
				fail(rowError{Reason: "invalid", Location: pf.uri, Message: ve.msg})
			case errors.As(err, &ve):
				notImplemented("of a file" + in + " whose " + ve.msg)
			default:
				notImplemented("of a file" + in + " whose data CloudBurrow could not read (" + err.Error() + ")")
			}
			return
		}
	}
	if err := bw.Flush(); err != nil {
		notImplemented("whose rows CloudBurrow could not keep (" + err.Error() + ")")
		return
	}

	// The table, before anything is changed.
	if plan.kind == pqLoadNew {
		if plan.createNever {
			fail(rowError{Reason: "notFound", Message: "Not found: Table " + name})
			return
		}
		if status, _ := f.get(r, "/datasets/"+url.PathEscape(plan.dest.DatasetID)); status == http.StatusNotFound {
			fail(rowError{Reason: "notFound", Message: "Not found: Dataset " + project + ":" + plan.dest.DatasetID})
			return
		}
	} else if plan.writeEmpty && !f.emptyTable(r, plan.dest.DatasetID, plan.dest.TableID, nil) {
		fail(rowError{Reason: "duplicate", Message: "Already Exists: Table " + name})
		return
	}

	backend := func(msg string) { fail(rowError{Reason: "backendError", Message: msg}) }
	if msg := f.carryOutParquet(r, plan, rows); msg != "" {
		backend("CloudBurrow could not load the Parquet file's rows into " + name + ": " + msg)
		return
	}
	f.finishOwnJob(w, r, j, "LOAD", "load", map[string]any{
		"inputFiles":     strconv.Itoa(len(plan.files)),
		"inputFileBytes": strconv.FormatInt(bytes, 10),
		"outputRows":     strconv.FormatInt(n, 10),
		"badRecords":     "0",
	}, nil)
}

// carryOutParquet writes the rows, one literal per line of rows, as plan
// says. It returns why it could not, or "".
func (f front) carryOutParquet(r *http.Request, plan *pqPlan, rows *os.File) string {
	ds := plan.dest.DatasetID
	schema := map[string]any{"fields": plan.raw}
	if plan.kind == pqLoadNew {
		if msg := f.makeParquetTable(r, plan.dest, schema, nil); msg != "" {
			return msg
		}
		if msg := f.insertRows(r, plan.dest, plan.target, rows); msg != "" {
			f.send(r, http.MethodDelete, tablePath(ds, plan.dest.TableID), nil)
			return msg
		}
		return ""
	}
	scratch := tableRef{DatasetID: ds, TableID: scratchTable()}
	if msg := f.makeParquetTable(r, scratch, schema, nil); msg != "" {
		return msg
	}
	keep := false
	defer func() {
		if !keep {
			f.send(r, http.MethodDelete, tablePath(ds, scratch.TableID), nil)
		}
	}()
	if plan.kind == pqLoadAdd {
		// The table's rows first, with the new columns NULL (a new
		// REPEATED column empty).
		cols, vals := []string{}, []string{}
		have := map[string]bool{}
		for _, o := range plan.old {
			have[strings.ToLower(o.Name)] = true
		}
		for _, t := range plan.target {
			cols = append(cols, quoteName(t.Name))
			if have[strings.ToLower(t.Name)] {
				vals = append(vals, quoteName(t.Name))
			} else {
				s, _ := literalFor(t, nil)
				vals = append(vals, s)
			}
		}
		if msg := f.runDML(r, "INSERT INTO "+quotePath([]string{ds, scratch.TableID})+" ("+strings.Join(cols, ", ")+") SELECT "+
			strings.Join(vals, ", ")+" FROM "+quotePath([]string{ds, plan.dest.TableID})); msg != "" {
			return msg
		}
	}
	if msg := f.insertRows(r, scratch, plan.target, rows); msg != "" {
		return msg
	}
	copyIn := func() string {
		cols := make([]string, len(plan.target))
		for i, t := range plan.target {
			cols[i] = quoteName(t.Name)
		}
		list := strings.Join(cols, ", ")
		return f.runDML(r, "INSERT INTO "+quotePath([]string{ds, plan.dest.TableID})+" ("+list+") SELECT "+list+" FROM "+
			quotePath([]string{ds, scratch.TableID}))
	}
	switch plan.kind {
	case pqLoadAppend:
		if plan.relaxed {
			patch, err := json.Marshal(map[string]any{"schema": schema})
			if err != nil {
				return err.Error()
			}
			if status, got := f.send(r, http.MethodPatch, tablePath(ds, plan.dest.TableID), patch); status != http.StatusOK {
				return "could not relax the table's columns: " + errorMessage(got, status)
			}
		}
		return copyIn()
	case pqLoadTruncate, pqLoadTruncateData:
		if msg := f.runDML(r, "DELETE FROM "+quotePath([]string{ds, plan.dest.TableID})+" WHERE TRUE"); msg != "" {
			return msg
		}
		if msg := copyIn(); msg != "" {
			keep = true
			return msg + "; the loaded rows are in " + ds + "." + scratch.TableID
		}
		return ""
	}
	// pqLoadAdd and pqLoadReplace: the table is made again.
	f.send(r, http.MethodDelete, tablePath(ds, plan.dest.TableID), nil)
	if msg := f.makeParquetTable(r, plan.dest, schema, carriedTableProperties(plan.table, plan.kind != pqLoadReplace)); msg != "" {
		keep = true
		return msg + "; the table's rows and the loaded ones are in " + ds + "." + scratch.TableID
	}
	if msg := copyIn(); msg != "" {
		keep = true
		return msg + "; the table's rows and the loaded ones are in " + ds + "." + scratch.TableID
	}
	return ""
}

// carriedTableProperties are the properties of a table made again with a
// new schema that it keeps; its tableConstraints only when constraints
// is set.
func carriedTableProperties(table map[string]any, constraints bool) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"description", "friendlyName", "labels", "expirationTime", "timePartitioning",
		"rangePartitioning", "clustering", "requirePartitionFilter", "defaultCollation"} {
		if v, ok := table[k]; ok {
			out[k] = v
		}
	}
	if v, ok := table["tableConstraints"]; ok && constraints {
		out["tableConstraints"] = v
	}
	return out
}

// makeParquetTable makes ref with schema and props, through createTable
// (which makes a FLOAT column FLOAT64 in the engine, #1000).
func (f front) makeParquetTable(r *http.Request, ref tableRef, schema map[string]any, props map[string]any) string {
	t := map[string]any{
		"tableReference": map[string]string{"projectId": projectOf(f.base), "datasetId": ref.DatasetID, "tableId": ref.TableID},
		"schema":         schema,
	}
	for k, v := range props {
		t[k] = v
	}
	status, got := f.createTable(r, ref.DatasetID, t)
	if status != http.StatusOK {
		return "could not make " + ref.DatasetID + "." + ref.TableID + ": " + errorMessage(got, status)
	}
	return ""
}

// Bounds on one INSERT of rows.
const (
	maxInsertBytes = 256 << 10
	maxInsertRows  = 500
)

// insertRows inserts the rows, one literal per line of rows, into ref,
// whose columns are fields, by INSERTs of a bounded size.
func (f front) insertRows(r *http.Request, ref tableRef, fields []field, rows *os.File) string {
	if len(fields) == 0 {
		return ""
	}
	if _, err := rows.Seek(0, io.SeekStart); err != nil {
		return err.Error()
	}
	cols := make([]string, len(fields))
	for i, fl := range fields {
		cols[i] = quoteName(fl.Name)
	}
	head := "INSERT INTO " + quotePath([]string{ref.DatasetID, ref.TableID}) + " (" + strings.Join(cols, ", ") + ") VALUES "
	var b strings.Builder
	count := 0
	flush := func() string {
		if count == 0 {
			return ""
		}
		msg := f.runDML(r, head+b.String())
		b.Reset()
		count = 0
		return msg
	}
	sc := bufio.NewScanner(rows)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for sc.Scan() {
		if count > 0 {
			b.WriteString(", ")
		}
		b.WriteString(sc.Text())
		count++
		if count >= maxInsertRows || b.Len() >= maxInsertBytes {
			if msg := flush(); msg != "" {
				return msg
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err.Error()
	}
	return flush()
}

// runDML runs one statement, and returns the emulator's error, or "".
func (f front) runDML(r *http.Request, sql string) string {
	legacy := false
	body, err := json.Marshal(queryOptions{Query: sql, UseLegacySQL: &legacy})
	if err != nil {
		return err.Error()
	}
	status, got := f.send(r, http.MethodPost, "/queries", body)
	if status != http.StatusOK {
		return errorMessage(got, status)
	}
	var res struct {
		Errors []rowError `json:"errors"`
	}
	if json.Unmarshal(got, &res) == nil && len(res.Errors) > 0 {
		return res.Errors[0].Message
	}
	return ""
}

// parquetData opens one file of a load: the upload's data, s, or the
// object, copied to a temporary file. cleanup removes what it made.
func (f front) parquetData(r *http.Request, s *spooled, pf pqFile) (*readerAtSeeker, int64, func(), error) {
	if pf.uri == "" {
		if s == nil {
			return nil, 0, nil, errors.New("the upload's data was not kept")
		}
		return &readerAtSeeker{ReaderAt: s.file, size: s.size}, s.size, func() {}, nil
	}
	body, err := f.storage.open(r.Context(), pf.obj)
	if err != nil {
		return nil, 0, nil, err
	}
	defer body.Close()
	tmp, err := os.CreateTemp("", "bigquery-parquet-gcs-*")
	if err != nil {
		return nil, 0, nil, err
	}
	cleanup := func() { tmp.Close(); os.Remove(tmp.Name()) }
	size, err := io.Copy(tmp, body)
	if err != nil {
		cleanup()
		return nil, 0, nil, fmt.Errorf("reading %s: %w", pf.uri, err)
	}
	return &readerAtSeeker{ReaderAt: tmp, size: size}, size, cleanup, nil
}

// startParquetJob reads the job of r, a load the front carries out, as
// startOwnJob does for a job whose body is JSON; r may be a multipart
// upload.
func (f front) startParquetJob(w http.ResponseWriter, r *http.Request) (*ownJob, bool) {
	var job struct {
		JobReference  map[string]any             `json:"jobReference"`
		Configuration map[string]json.RawMessage `json:"configuration"`
	}
	if !decodeJob(r, &job) {
		writeError(w, http.StatusBadRequest, "invalid", "cloudburrow: could not read the job")
		return nil, false
	}
	if dry := job.Configuration["dryRun"]; string(dry) == "true" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a dry run of a Parquet load "+
			"CloudBurrow carries out itself. Nothing was run.")
		return nil, false
	}
	if f.jobs == nil {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a Parquet load CloudBurrow "+
			"carries out itself: this front keeps no jobs of its own. Nothing was loaded.")
		return nil, false
	}
	project := projectOf(f.base)
	if job.JobReference == nil {
		job.JobReference = map[string]any{}
	}
	if p, _ := job.JobReference["projectId"].(string); p != "" {
		project = p
	}
	id, _ := job.JobReference["jobId"].(string)
	if id == "" {
		id = newJobID()
	}
	job.JobReference["projectId"], job.JobReference["jobId"] = project, id
	if _, ok := f.jobs.get(project, id); ok {
		writeError(w, http.StatusConflict, "duplicate", fmt.Sprintf("Already Exists: Job %s:%s", project, id))
		return nil, false
	}
	if status, _ := f.get(r, "/jobs/"+url.PathEscape(id)); status == http.StatusOK {
		writeError(w, http.StatusConflict, "duplicate", fmt.Sprintf("Already Exists: Job %s:%s", project, id))
		return nil, false
	}
	return &ownJob{project: project, id: id, ref: job.JobReference, conf: job.Configuration, start: time.Now()}, true
}
