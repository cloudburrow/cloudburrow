package bigqueryfront

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// writtenExtract is an extract the front writes to Cloud Storage itself
// (#957), because the emulator writes it differently from BigQuery.
type writtenExtract struct {
	json      bool   // NEWLINE_DELIMITED_JSON, else CSV
	avro      bool   // AVRO (an Avro object container file), else CSV
	avroCodec string // "", "deflate" or "snappy"
	// avroLogical is the job's useAvroLogicalTypes: DATE as int date, TIME
	// as long time-micros.
	avroLogical bool
	// parquet is PARQUET (extractparquet.go), with parquetCodec "",
	// "SNAPPY", "GZIP" or "ZSTD".
	parquet      bool
	parquetCodec string
	// parquetNativeGeography is the job's nativeGeographyExportEnabled:
	// GEOGRAPHY in Parquet's GEOGRAPHY logical type (extractparquet.go).
	parquetNativeGeography bool
	gzip                   bool
	delimiter              rune
	header                 bool
	// uris are the job's URIs, a wildcard's "*" kept: the rows are shared
	// out between them (shareRows), and a wildcard URI's share between its
	// files (extractFiles).
	uris   []string
	fields []field
	// size is the table's logical size in bytes (numBytes), 0 when not
	// known.
	size int64
}

// writeExtract carries out an extract job the front writes itself (#957):
// it reads the table's rows from the emulator (tabledata.list), writes the
// file as BigQuery documents it, uploads it to the instance's Cloud
// Storage (f.storage), and answers the job done. The job is the front's:
// the emulator never sees it, so jobs.get, jobs.list, jobs.cancel and
// jobs.delete of it are answered by the front (frontJobs).
//
// What is written, and why it is BigQuery's
// (https://cloud.google.com/bigquery/docs/exporting-data):
//
//   - CSV, with any single-character fieldDelimiter ("You can control the
//     CSV delimiter") and GZIP compression (the formats table lists GZIP
//     for CSV and JSON): the values as the emulator writes a comma-separated
//     CSV, which were measured to be BigQuery's for the types in
//     csvExportTypes, with encoding/csv's quoting, as the emulator's; each
//     row ends with "\n".
//
//   - The header row of an empty table: "When set to true, header rows are
//     printed to the exported data if the data format supports headers":
//     the file holds the header row and nothing else, where the emulator
//     wrote an empty object.
//
//   - NEWLINE_DELIMITED_JSON: one object per row (jsonExtractRow,
//     extracttext.go), and CSV and JSON values in each type's text
//     (extractText).
//
//   - AVRO, of the columns avroExportSchema gives a type (extractavro.go:
//     TIMESTAMP, DATE, TIME, NUMERIC and BIGNUMERIC as the documentation's
//     logical types, RECORDs as nested records, REPEATED as arrays) and of
//     STRING, INT64, FLOAT64, BOOL and BYTES columns: an Avro
//     object container file whose schema is a record of one field a column,
//     in the schema's order, each of the Avro type the documentation maps
//     the column's type to ("INTEGER ... long", "FLOAT ... double",
//     "BOOLEAN ... boolean", "STRING ... string", "BYTES ... bytes"), a
//     NULLABLE column as a union with "null" ("NULLABLE ... A union of the
//     field type and null"), written with the DEFLATE or SNAPPY codec the
//     job names ("DEFLATE, SNAPPY" are listed for Avro). The record's name,
//     "Root", is not given by the documentation (UNVERIFIED against
//
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
//
//	BigQuery), nor are the file's sync marker and block sizes: an Avro
//	reader reads the same rows and types, not the same bytes.
//
// The object is uploaded with the content type the emulator's Cloud
// Storage client gives the same bytes (it detects it), and
// "application/json" for JSON, as the emulator sets it.
func (f front) writeExtract(w http.ResponseWriter, r *http.Request, e *extractConfig, x writtenExtract) {
	j, ok := f.startOwnJob(w, r, "an extract job CloudBurrow writes itself")
	if !ok {
		return
	}
	if f.storageHost == "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: this extract job, which "+
			"CloudBurrow writes itself, as the emulator behind it writes it differently from BigQuery: this front has "+
			"no Cloud Storage to write it to. Nothing was written.")
		return
	}
	src := e.SourceTable
	rows, status, got := f.tableRows(r, src.DatasetID, src.TableID)
	if status != http.StatusOK {
		writeRaw(w, status, got)
		return
	}
	// Each file is encoded before anything is uploaded, so a share that
	// cannot be written writes nothing.
	files := extractFiles(x, rows)
	var msg string
	for k := range files {
		if files[k].data, msg = encodeExtract(x, files[k].rows); msg != "" {
			break
		}
	}
	if msg != "" && x.parquet {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: an extract job to PARQUET "+
			"that CloudBurrow could not write ("+msg+"). Nothing was written.")
		return
	}
	if msg != "" && x.avro {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: an extract job to AVRO "+
			"that CloudBurrow could not write ("+msg+"). Nothing was written.")
		return
	}
	if msg != "" {
		format := "CSV"
		if x.json {
			format = "NEWLINE_DELIMITED_JSON"
		}
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: an extract job to "+format+
			" that CloudBurrow could not write as BigQuery documents it ("+msg+"). Nothing was written.")
		return
	}
	var failure *rowError
	counts := make([]string, len(x.uris))
	perURI := make([]int, len(x.uris))
	for _, fl := range files {
		perURI[fl.uri]++
		contentType := http.DetectContentType(fl.data)
		if x.avro || x.parquet {
			contentType = "application/octet-stream"
		}
		if x.json && !x.gzip {
			contentType = "application/json"
		}
		if err := f.upload(r.Context(), fl.name, contentType, fl.data); err != nil {
			failure = &rowError{Reason: "backendError", Message: fmt.Sprintf("could not write %s: %v", fl.name, err)}
			break
		}
	}
	for k, n := range perURI {
		counts[k] = strconv.Itoa(n)
	}
	f.finishOwnJob(w, r, j, "EXTRACT", "extract", map[string]any{"destinationUriFileCounts": counts}, failure)
}

// extractFile is one file an extract writes: its object's URI, the index
// of the job's URI it is a file of, and its rows and bytes.
type extractFile struct {
	name string
	uri  int
	rows [][]any
	data []byte
}

// extractFiles shares rows out between x's URIs (shareRows), and a
// wildcard URI's share between as many files as keep each at most
// extractFileLimit bytes of the table's logical size ("BigQuery can export
// up to 1 GB of logical data size to a single file"), the share's size
// taken in proportion to its rows. Each file of a wildcard URI is named by
// the "*" replaced with its number: "BigQuery replaces the asterisk with a
// 12-digit number, starting with 000000000000"; a URI's files number from
// 0 each (exporting-data). A wildcard URI with no rows gets one empty file
// (destinationUriFileCounts 1, as BigQuery writes a file with the header).
// How many files BigQuery writes, and which rows each holds, is UNVERIFIED:
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
// "The number of files generated by an extract job can vary".
func extractFiles(x writtenExtract, rows [][]any) []extractFile {
	var out []extractFile
	for k, share := range shareRows(rows, len(x.uris)) {
		uri := x.uris[k]
		if !strings.Contains(uri, "*") {
			out = append(out, extractFile{name: uri, uri: k, rows: share})
			continue
		}
		n := 1
		if x.size > 0 && len(rows) > 0 {
			bytes := x.size * int64(len(share)) / int64(len(rows))
			n = int((bytes + extractFileLimit - 1) / extractFileLimit)
			n = max(1, min(n, len(share)))
		}
		for i, part := range shareRows(share, n) {
			out = append(out, extractFile{name: strings.Replace(uri, "*", fmt.Sprintf("%012d", i), 1), uri: k, rows: part})
		}
	}
	return out
}

// shareRows shares rows out between n URIs, in order: the first URI gets
// the first rows, each URI a share of the same size but for the last
// ones, which get one row fewer when the rows do not divide evenly. A URI
// with no share gets no rows. BigQuery documents only that "the size of
// the files will vary" and that "the globally sorted result set is
// distributed sequentially across the generated files", not how many rows
// each gets (exporting-data): which file holds which row is UNVERIFIED.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/exporting-data
func shareRows(rows [][]any, n int) [][][]any {
	if n < 1 {
		n = 1
	}
	shares := make([][][]any, n)
	size, extra := len(rows)/n, len(rows)%n
	at := 0
	for k := range shares {
		m := size
		if k < extra {
			m++
		}
		shares[k] = rows[at : at+m]
		at += m
	}
	return shares
}

// ownJob is a job the front carries out itself (writeExtract, copyJob),
// from jobs.insert's body.
type ownJob struct {
	project, id string
	ref         map[string]any
	conf        map[string]json.RawMessage
	start       time.Time
}

// startOwnJob reads the job in r, which the front carries out itself
// (what names it), giving it a job ID when it has none. It answers w, and
// reports false, for a body it cannot read, a dry run (501), a front with
// no job store (501), and a job ID in use (409).
func (f front) startOwnJob(w http.ResponseWriter, r *http.Request, what string) (*ownJob, bool) {
	var job struct {
		JobReference  map[string]any             `json:"jobReference"`
		Configuration map[string]json.RawMessage `json:"configuration"`
	}
	b, err := readBody(r)
	if err != nil || json.Unmarshal(b, &job) != nil {
		writeError(w, http.StatusBadRequest, "invalid", "cloudburrow: could not read the job")
		return nil, false
	}
	if dry := job.Configuration["dryRun"]; string(dry) == "true" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a dry run of "+what+
			". Nothing was run.")
		return nil, false
	}
	if f.jobs == nil {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: "+what+
			": this front keeps no jobs of its own. Nothing was run.")
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
	return &ownJob{project: project, id: id, ref: job.JobReference, conf: job.Configuration, start: f.now()}, true
}

// finishOwnJob records j done, of jobType, with stats as its
// statistics.<statsKey>, failed with failure if it is not nil, and
// answers w with it. jobs.get, jobs.list, jobs.cancel and jobs.delete of
// it are then answered by the front (frontJobs).
func (f front) finishOwnJob(w http.ResponseWriter, r *http.Request, j *ownJob, jobType, statsKey string, stats map[string]any, failure *rowError) {
	end := f.now()
	conf := map[string]any{}
	for k, v := range j.conf {
		var val any
		if json.Unmarshal(v, &val) == nil {
			conf[k] = val
		}
	}
	conf["jobType"] = jobType
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	statistics := map[string]any{"creationTime": ms(j.start), "startTime": ms(j.start), "endTime": ms(end)}
	if stats != nil {
		statistics[statsKey] = stats
	}
	resource := map[string]any{
		"kind":          "bigquery#job",
		"id":            j.project + ":" + j.id,
		"jobReference":  j.ref,
		"configuration": conf,
		"selfLink":      "http://" + r.Host + f.base + "/jobs/" + url.PathEscape(j.id),
		"status":        map[string]any{"state": "DONE"},
		"statistics":    statistics,
	}
	if failure != nil {
		failJob(resource, *failure)
	}
	f.jobs.add(j.project, j.id, resource)
	writeJSON(w, http.StatusOK, resource)
}

// newJobID makes a job ID for a job sent without one.
func newJobID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "job_" + hex.EncodeToString(b)
}

// tableRows reads a table's rows, each value as tabledata.list gives it
// (a string, or nil for NULL): through the emulator's tabledata.list, or,
// when another dataset has a table of the ID, from a query of its whole
// name (tableData, #1015: the emulator's tabledata.list reads the first
// table of the ID made in any dataset). On failure it returns the emulator's status and
// body.
func (f front) tableRows(r *http.Request, dataset, table string) ([][]any, int, []byte) {
	read := f.emulatorTableData
	if f.sharedID(r, dataset, table) {
		read = func(r *http.Request, dataset, table string) ([]json.RawMessage, int, []byte) {
			return f.tableData(r, dataset, table, nil, "")
		}
	}
	raw, status, got := read(r, dataset, table)
	if status != http.StatusOK {
		return nil, status, got
	}
	rows := make([][]any, 0, len(raw))
	for _, b := range raw {
		var row struct {
			F []struct {
				V any `json:"v"`
			} `json:"f"`
		}
		if err := json.Unmarshal(b, &row); err != nil {
			return nil, http.StatusBadGateway, b
		}
		vals := make([]any, len(row.F))
		for i, c := range row.F {
			vals[i] = c.V
		}
		rows = append(rows, vals)
	}
	return rows, http.StatusOK, nil
}

// encodeExtract writes rows as x's file. It returns why a row cannot be
// written, or "".
func encodeExtract(x writtenExtract, rows [][]any) ([]byte, string) {
	if x.avro {
		return encodeAvroExtract(x, rows)
	}
	if x.parquet {
		return encodeParquetExtract(x, rows)
	}
	var buf bytes.Buffer
	var out io.Writer = &buf
	var gz *gzip.Writer
	if x.gzip {
		gz = gzip.NewWriter(&buf)
		out = gz
	}
	if x.json {
		for i, row := range rows {
			line, err := jsonExtractRow(x.fields, row)
			if err != nil {
				return nil, fmt.Sprintf("row %d, %v", i+1, err)
			}
			_, _ = out.Write(line)
		}
	} else {
		cw := csv.NewWriter(out)
		cw.Comma = x.delimiter
		if x.header {
			names := make([]string, len(x.fields))
			for i, fl := range x.fields {
				names[i] = fl.Name
			}
			_ = cw.Write(names)
		}
		for i, row := range rows {
			rec := make([]string, len(x.fields))
			for j, fl := range x.fields {
				if j >= len(row) || row[j] == nil {
					continue // a NULL is an empty field
				}
				s, _ := row[j].(string)
				t, err := extractText(fl, s) // extracttext.go
				if err != nil {
					return nil, fmt.Sprintf("row %d, column %s: %v", i+1, fl.Name, err)
				}
				rec[j] = t
			}
			_ = cw.Write(rec)
		}
		cw.Flush()
	}
	if gz != nil {
		_ = gz.Close()
	}
	return buf.Bytes(), ""
}

// upload writes an object to the instance's Cloud Storage, over its JSON
// API's media upload.
func (f front) upload(ctx context.Context, uri, contentType string, data []byte) error {
	bucket, object, ok := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/")
	if !strings.HasPrefix(uri, "gs://") || !ok || object == "" {
		return fmt.Errorf("not a gs:// URI of an object")
	}
	u := "http://" + f.storageHost + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=media&name=" + url.QueryEscape(object)
	// A media upload names its object, so sending it again writes the same
	// object (serviceclient.go, #1628).
	status, b, err := serviceCall(ctx, "Cloud Storage upload of "+uri, http.MethodPost, u, contentType, data)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		if len(b) > 512 {
			b = b[:512]
		}
		return fmt.Errorf("Cloud Storage answered %d: %s", status, strings.TrimSpace(string(b)))
	}
	return nil
}

// frontJobs are the jobs the front carried out itself (writeExtract), and
// the failed queries the emulator made no job of (failedjobs.go, #1397), by
// project and job ID, which it answers jobs.get, jobs.list, jobs.cancel
// and jobs.delete of. The most recent maxFrontJobs are kept.
type frontJobs struct {
	mu    sync.Mutex
	jobs  map[string]map[string]any
	order []string
	// made is told of each job added (jobstable.go's noteMade).
	made func(project, id string)
}

const maxFrontJobs = maxJobRecords

func (j *frontJobs) add(project, id string, job map[string]any) {
	if j.made != nil {
		defer j.made(project, id)
	}
	key := project + "/" + id
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.jobs == nil {
		j.jobs = map[string]map[string]any{}
	}
	if _, ok := j.jobs[key]; !ok {
		j.order = append(j.order, key)
	}
	j.jobs[key] = job
	for len(j.order) > maxFrontJobs {
		delete(j.jobs, j.order[0])
		j.order = j.order[1:]
	}
}

func (j *frontJobs) get(project, id string) (map[string]any, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[project+"/"+id]
	return job, ok
}

func (j *frontJobs) remove(project, id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	key := project + "/" + id
	if _, ok := j.jobs[key]; !ok {
		return false
	}
	delete(j.jobs, key)
	for i, k := range j.order {
		if k == key {
			j.order = append(j.order[:i], j.order[i+1:]...)
			break
		}
	}
	return true
}

// list returns the project's jobs, oldest first, as the emulator lists
// its own.
func (j *frontJobs) list(project string) []map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []map[string]any
	for _, k := range j.order {
		if strings.HasPrefix(k, project+"/") {
			out = append(out, j.jobs[k])
		}
	}
	return out
}

// jobActionRoute matches jobs.cancel and jobs.delete.
var jobActionRoute = regexp.MustCompile(`^(/bigquery/v2)?/projects/([^/]+)/jobs/([^/]+)/(cancel|delete)$`)

// listed is l, an answer to jobs.list, with the front's own jobs of the
// project after the emulator's: with their configuration only for
// projection=full, as BigQuery's "minimal" projection "Does not include
// the job configuration".
func (j *frontJobs) listed(r *http.Request, project string, l jobListing) jobListing {
	own := j.list(project)
	if len(own) == 0 || l.list == nil {
		return l
	}
	jobs := l.jobs()
	full := r.URL.Query().Get("projection") == "full"
	for _, job := range own {
		item := map[string]any{"kind": job["kind"], "id": job["id"], "jobReference": job["jobReference"],
			"statistics": job["statistics"], "status": job["status"]}
		if st, ok := job["status"].(map[string]any); ok {
			item["state"] = st["state"]
			if e, ok := st["errorResult"]; ok {
				item["errorResult"] = e
			}
		}
		if full {
			item["configuration"] = job["configuration"]
		}
		// A copy: the layers above change the listed jobs in place
		// (joblisting.go), and the job kept here stays as it is.
		jobs = append(jobs, jsonCopy(item))
	}
	l.list["jobs"] = jobs
	return l
}

// serveJobAction answers jobs.get, jobs.cancel and jobs.delete of one of
// the front's own jobs, and reports whether the job was one.
func (j *frontJobs) serveJobAction(w http.ResponseWriter, r *http.Request, project, rawID, action string) bool {
	id, err := url.PathUnescape(rawID)
	if err != nil {
		return false
	}
	job, ok := j.get(project, id)
	if !ok {
		return false
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, job)
	case action == "cancel" && r.Method == http.MethodPost:
		// The job is done, so cancelling it leaves it as it is.
		writeJSON(w, http.StatusOK, map[string]any{"kind": "bigquery#jobCancelResponse", "job": job})
	case action == "delete" && r.Method == http.MethodDelete:
		j.remove(project, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		return false
	}
	return true
}
