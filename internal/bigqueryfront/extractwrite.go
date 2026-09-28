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
	var job struct {
		JobReference  map[string]any             `json:"jobReference"`
		Configuration map[string]json.RawMessage `json:"configuration"`
	}
	b, err := readBody(r)
	if err != nil || json.Unmarshal(b, &job) != nil {
		writeError(w, http.StatusBadRequest, "invalid", "cloudburrow: could not read the extract job")
		return
	}
	if dry := job.Configuration["dryRun"]; string(dry) == "true" {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: a dry run of an extract job "+
			"CloudBurrow writes itself. Nothing was written.")
		return
	}
	if f.storageHost == "" || f.jobs == nil {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: this extract job, which "+
			"CloudBurrow writes itself, as the emulator behind it writes it differently from BigQuery: this front has "+
			"no Cloud Storage to write it to. Nothing was written.")
		return
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
		return
	}
	if status, _ := f.get(r, "/jobs/"+url.PathEscape(id)); status == http.StatusOK {
		writeError(w, http.StatusConflict, "duplicate", fmt.Sprintf("Already Exists: Job %s:%s", project, id))
		return
	}

	start := time.Now()
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
	end := time.Now()

	conf := map[string]any{}
	for k, v := range job.Configuration {
		var val any
		if json.Unmarshal(v, &val) == nil {
			conf[k] = val
		}
	}
	conf["jobType"] = "EXTRACT"
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	resource := map[string]any{
		"kind":          "bigquery#job",
		"id":            project + ":" + id,
		"jobReference":  job.JobReference,
		"configuration": conf,
		"selfLink":      "http://" + r.Host + f.base + "/jobs/" + url.PathEscape(id),
		"status":        map[string]any{"state": "DONE"},
		"statistics": map[string]any{
			"creationTime": ms(start), "startTime": ms(start), "endTime": ms(end),
			"extract": map[string]any{"destinationUriFileCounts": []string{"1"}},
		},
	}
	if failure != nil {
		failJob(resource, *failure)
	}
	f.jobs.add(project, id, resource)
	writeJSON(w, http.StatusOK, resource)
}

// newJobID makes a job ID for a job sent without one.
func newJobID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "job_" + hex.EncodeToString(b)
}

// tableRows reads a table's rows from the emulator, each value as
// tabledata.list gives it (a string, or nil for NULL), following its page
// tokens. On failure it returns the emulator's status and body.
func (f front) tableRows(r *http.Request, dataset, table string) ([][]any, int, []byte) {
	var rows [][]any
	token := ""
	for {
		p := tablePath(dataset, table) + "/data"
		if token != "" {
			p += "?pageToken=" + url.QueryEscape(token)
		}
		status, got := f.get(r, p)
		if status != http.StatusOK {
			return nil, status, got
		}
		var page struct {
			PageToken string `json:"pageToken"`
			Rows      []struct {
				F []struct {
					V any `json:"v"`
				} `json:"f"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(got, &page); err != nil {
			return nil, http.StatusBadGateway, got
		}
		for _, row := range page.Rows {
			vals := make([]any, len(row.F))
			for i, c := range row.F {
				vals[i] = c.V
			}
			rows = append(rows, vals)
		}
		if page.PageToken == "" || page.PageToken == token {
			return rows, http.StatusOK, nil
		}
		token = page.PageToken
	}
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
