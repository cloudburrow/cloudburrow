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
	json      bool // NEWLINE_DELIMITED_JSON, else CSV
	gzip      bool
	delimiter rune
	header    bool
	uri       string // the URI written, its wildcard replaced
	fields    []field
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
//   - The header row of an empty table: "When set to true, header rows are
//     printed to the exported data if the data format supports headers":
//     the file holds the header row and nothing else, where the emulator
//     wrote an empty object.
//   - NEWLINE_DELIMITED_JSON, of STRING and INT64 columns: one object per
//     row, its members in the schema's order; "INT64 (integer) data types
//     are encoded as JSON strings to preserve 64-bit precision", and "the
//     symbols <, >, and & are converted by using the unicode notation"
//     (\u003c, \u003e, \u0026). The documentation does not say how a NULL
//     is written: a row with one is 501, and nothing is written. Nor does
//     it say how the other types are, or whether there are spaces between
//     members: the other types are 501, and members are written with none.
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
	data, msg := encodeExtract(x, rows)
	if msg != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: an extract job to "+
			"NEWLINE_DELIMITED_JSON of a row with a NULL value ("+msg+"). BigQuery's documentation does not say how it "+
			"writes a NULL to JSON, so CloudBurrow does not write it. Nothing was written.")
		return
	}
	contentType := http.DetectContentType(data)
	if x.json && !x.gzip {
		contentType = "application/json"
	}
	var failure *rowError
	if err := f.upload(r.Context(), x.uri, contentType, data); err != nil {
		failure = &rowError{Reason: "backendError", Message: fmt.Sprintf("could not write %s: %v", x.uri, err)}
	}
	f.finishOwnJob(w, r, j, "EXTRACT", "extract", map[string]any{"destinationUriFileCounts": []string{"1"}}, failure)
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
	return &ownJob{project: project, id: id, ref: job.JobReference, conf: job.Configuration, start: time.Now()}, true
}

// finishOwnJob records j done, of jobType, with stats as its
// statistics.<statsKey>, failed with failure if it is not nil, and
// answers w with it. jobs.get, jobs.list, jobs.cancel and jobs.delete of
// it are then answered by the front (frontJobs).
func (f front) finishOwnJob(w http.ResponseWriter, r *http.Request, j *ownJob, jobType, statsKey string, stats map[string]any, failure *rowError) {
	end := time.Now()
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
	var buf bytes.Buffer
	var out io.Writer = &buf
	var gz *gzip.Writer
	if x.gzip {
		gz = gzip.NewWriter(&buf)
		out = gz
	}
	if x.json {
		for i, row := range rows {
			var line bytes.Buffer
			line.WriteByte('{')
			for j, fl := range x.fields {
				if j >= len(row) || row[j] == nil {
					return nil, fmt.Sprintf("row %d, column %s", i+1, fl.Name)
				}
				s, _ := row[j].(string)
				if j > 0 {
					line.WriteByte(',')
				}
				name, _ := json.Marshal(fl.Name)
				val, _ := json.Marshal(s)
				line.Write(name)
				line.WriteByte(':')
				line.Write(val)
			}
			line.WriteString("}\n")
			_, _ = out.Write(line.Bytes())
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
		for _, row := range rows {
			rec := make([]string, len(x.fields))
			for j := range x.fields {
				if j < len(row) {
					rec[j], _ = row[j].(string)
				}
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
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	u := "http://" + f.storageHost + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=media&name=" + url.QueryEscape(object)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("Cloud Storage answered %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// frontJobs are the jobs the front carried out itself (writeExtract), by
// project and job ID, which it answers jobs.get, jobs.list, jobs.cancel
// and jobs.delete of. The most recent maxFrontJobs are kept.
type frontJobs struct {
	mu    sync.Mutex
	jobs  map[string]map[string]any
	order []string
}

const maxFrontJobs = 1000

func (j *frontJobs) add(project, id string, job map[string]any) {
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

// serveJobList answers jobs.list (serve) with the front's own jobs of the
// project after the emulator's: with their configuration only for
// projection=full, as BigQuery's "minimal" projection "Does not include
// the job configuration".
func (j *frontJobs) serveJobList(w http.ResponseWriter, r *http.Request, project string, serve func(http.ResponseWriter)) {
	own := j.list(project)
	if len(own) == 0 {
		serve(w)
		return
	}
	rec := newRecorder()
	serve(rec)
	var list map[string]any
	if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &list) != nil {
		rec.copyTo(w)
		return
	}
	jobs, _ := list["jobs"].([]any)
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
		jobs = append(jobs, item)
	}
	list["jobs"] = jobs
	writeJSON(w, http.StatusOK, list)
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
