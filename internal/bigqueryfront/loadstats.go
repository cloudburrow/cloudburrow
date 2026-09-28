package bigqueryfront

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// What a CSV load's job reports of its data (#960).
//
// BigQuery reports a load's counts in the job's statistics.load
// (JobStatistics3): "inputFiles: Number of source files in a load job",
// "inputFileBytes: Number of bytes of source data in a load job",
// "outputRows: Number of rows imported in a load job. Note that while an
// import job is in the running state, this value may change", and
// "badRecords: The number of bad records encountered. Note that if the job
// has failed because of more bad records encountered than the maximum
// allowed in the load job configuration, then this number can be less than
// the total number of bad records present in the input data"; and the
// records it left out in status.errors: "The first errors encountered
// during the running of the job. ... Errors here do not necessarily mean
// that the job has not completed or was unsuccessful."
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobStatistics3
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#JobStatus
//
// The emulator reports none of them (measured against the pinned image
// through the official Go client, and in its source, server/handler.go:
// an upload's job is the job as it was sent, and jobs.get gives it only
// status.state DONE): a load that the front left bad records out of read
// back from jobs.get with no statistics and no errors.
//
// A load whose records the front reads (csvStream with a dialect that is
// not plain: maxBadRecords, ignoreUnknownValues, a REQUIRED column, or any
// other CSV option the front carries out) is counted as its data passes:
// the files and their bytes as read, the records written on to the
// emulator, and the bad records left out, the first maxListedBadRecords
// of them each an "invalid" error located at its file's gs:// URI (none
// for an upload). When the load succeeds, the front adds these to the
// job's statistics.load and status.errors in the jobs.insert answer, and
// keeps them for jobs.get and jobs.list (jobFailures.loads).
//
// A load whose records the front does not read (#966): a CSV load it
// passes on as it is, a NEWLINE_DELIMITED_JSON or Parquet one (the
// emulator loads no other format: measured, AVRO and ORC are 400 "not
// support sourceFormat"), or one from Cloud Storage that the emulator
// reads itself, is counted around it (countLoad): outputRows in the table,
// and inputFiles and inputFileBytes from the upload or the objects. It
// reports no badRecords.
//
// outputBytes is not reported (#965). BigQuery documents it only as "Size
// of the loaded data in bytes" (JobStatistics3), with no rule for how
// that size is taken: it is not the size of the data sent (that is
// inputFileBytes), and the emulator keeps no stored size to read (its
// tables.get reports numBytes 0, measured), so the front does not guess it.

// maxListedBadRecords bounds the bad records a job lists in
// status.errors. It is CloudBurrow's bound, to keep what the front holds
// small; BigQuery lists "the first errors" and does not say how many.
const maxListedBadRecords = 100

// loadCounts is what the front counted of a load's data. A load whose
// records the front does not read has no badRecords (noBad), and its
// outputRows from the table's rows (countLoad); one whose data the front
// does not see has no inputFiles or inputFileBytes (noInput), unless it
// read the objects' sizes.
type loadCounts struct {
	badRecords, outputRows, inputFiles, inputFileBytes int64
	errors                                             []rowError
	noRows, noBad, noInput                             bool
}

func (d *dataFailure) setCounts(c loadCounts) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.counted = &c
}

// counts returns what the stream counted, or nil when it did not read
// the records, or did not reach their end.
func (d *dataFailure) counts() *loadCounts {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.counted
}

// apply adds c to a Job resource, or an entry of jobs.list.
func (c loadCounts) apply(job map[string]any) {
	stats, _ := job["statistics"].(map[string]any)
	if stats == nil {
		stats = map[string]any{}
		job["statistics"] = stats
	}
	load, _ := stats["load"].(map[string]any)
	if load == nil {
		load = map[string]any{}
		stats["load"] = load
	}
	// int64 fields are JSON strings in the REST API.
	if !c.noInput {
		load["inputFiles"] = strconv.FormatInt(c.inputFiles, 10)
		load["inputFileBytes"] = strconv.FormatInt(c.inputFileBytes, 10)
	}
	if !c.noRows {
		load["outputRows"] = strconv.FormatInt(c.outputRows, 10)
	}
	if !c.noBad {
		load["badRecords"] = strconv.FormatInt(c.badRecords, 10)
	}
	// The load is done: its data has all been read. The emulator gives an
	// upload's job no status in the jobs.insert answer and in jobs.list
	// (its source, server/handler.go), and the Go client reads statistics
	// only with a status (measured: they were dropped).
	status, _ := job["status"].(map[string]any)
	if status == nil {
		status = map[string]any{"state": "DONE"}
		job["status"] = status
	}
	if len(c.errors) == 0 {
		return
	}
	errs, _ := status["errors"].([]any)
	for _, e := range c.errors {
		errs = append(errs, e)
	}
	status["errors"] = errs
}

// reportLoad adds c to rec, the emulator's answer to a load that
// succeeded, and keeps it for jobs.get and jobs.list; with c nil, it
// forgets what it kept of the job.
func (j *jobFailures) reportLoad(job jobBody, rec *recorder, c *loadCounts) {
	if rec.status != 0 && rec.status != 200 {
		return
	}
	var got map[string]any
	if json.Unmarshal(rec.body.Bytes(), &got) != nil {
		return
	}
	project, id := job.JobReference.ProjectID, job.JobReference.JobID
	if ref, ok := got["jobReference"].(map[string]any); ok {
		if p, _ := ref["projectId"].(string); p != "" {
			project = p
		}
		if i, _ := ref["jobId"].(string); i != "" {
			id = i
		}
	}
	if c == nil {
		j.setLoad(project, id, nil)
		return
	}
	c.apply(got)
	b, err := json.Marshal(got)
	if err != nil {
		return
	}
	rec.body.Reset()
	rec.body.Write(b)
	j.setLoad(project, id, c)
}

// setLoad keeps c for the job, or forgets the job's with c nil. The most
// recent maxJobFailures are kept.
func (j *jobFailures) setLoad(project, id string, c *loadCounts) {
	if id == "" {
		return
	}
	key := project + "/" + id
	j.mu.Lock()
	defer j.mu.Unlock()
	if c == nil {
		delete(j.loads, key)
		return
	}
	if j.loads == nil {
		j.loads = map[string]loadCounts{}
	}
	if _, ok := j.loads[key]; !ok {
		j.loadOrder = append(j.loadOrder, key)
	}
	j.loads[key] = *c
	for len(j.loadOrder) > maxJobFailures {
		delete(j.loads, j.loadOrder[0])
		j.loadOrder = j.loadOrder[1:]
	}
}

func (j *jobFailures) load(project, id string) (loadCounts, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	c, ok := j.loads[project+"/"+id]
	return c, ok
}

// countLoad returns the load req to send on and next, reporting the load's
// counts when it succeeds (#966). fail is what csvLoad counted of the
// data it read, or nil when it did not read the data.
//
// outputRows is what the front counted, for a load whose records it read;
// otherwise it is the destination table's rows after the load (tables.get's
// numRows, which the emulator counts in the table) less those before it,
// or all of them after a WRITE_TRUNCATE, which the emulator carries out
// in the same transaction as the load (its source, server/handler.go).
// A write to the same table by another request while the load runs is
// counted with it; a load into another project's table, or one whose table
// the front cannot read, has no outputRows. inputFiles and inputFileBytes
// are the upload's (one file, the bytes of its data as they pass through
// the front) or, for a load from Cloud Storage, the objects' the emulator
// read, by their sizes in the instance's Cloud Storage (objectSizes); with
// no Cloud Storage given to the front, they are left out. badRecords is
// left out for a load whose records the front does not read: the emulator
// fails a load on its first bad record (measured, #952), so a load that
// succeeded had none it left out, but BigQuery's own count is not known.
func (f front) countLoad(req *http.Request, next http.Handler, job jobBody, fail *dataFailure) (*http.Request, http.Handler) {
	l := job.Configuration.Load
	dest := l.DestinationTable
	measurable := dest != nil && dest.DatasetID != "" && dest.TableID != "" &&
		(dest.ProjectID == "" || dest.ProjectID == projectOf(f.base))
	var upload *mediaCount
	if fail == nil && len(l.SourceURIs) == 0 {
		upload = countMedia(req)
	}
	return req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r != req {
			next.ServeHTTP(w, r)
			return
		}
		before, beforeOK := int64(0), false
		if measurable {
			before, beforeOK = f.loadTableRows(r, dest)
		}
		rec := newRecorder()
		next.ServeHTTP(rec, r)
		size, sized := upload.size()
		if rec.status != 0 && rec.status != http.StatusOK {
			rec.copyTo(w)
			return
		}
		var c loadCounts
		if fail != nil {
			if got := fail.counts(); got != nil {
				c = *got
			} else {
				c.noRows, c.noBad, c.noInput = true, true, true
			}
		} else {
			c.noRows, c.noBad = true, true
			switch {
			case sized:
				c.inputFiles, c.inputFileBytes = 1, size
			case len(l.SourceURIs) > 0 && f.storage != nil:
				files, n, err := f.storage.objectSizes(r.Context(), l.SourceURIs)
				c.inputFiles, c.inputFileBytes, c.noInput = files, n, err != nil
			default:
				c.noInput = true
			}
		}
		if c.noRows && beforeOK && !jobFailed(rec) {
			if after, ok := f.loadTableRows(r, dest); ok {
				rows := after - before
				if strings.EqualFold(l.WriteDisposition, "WRITE_TRUNCATE") {
					rows = after
				}
				if rows >= 0 {
					c.outputRows, c.noRows = rows, false
				}
			}
		}
		if c.noRows && c.noBad && c.noInput {
			f.failed.reportLoad(job, rec, nil)
		} else {
			f.failed.reportLoad(job, rec, &c)
		}
		rec.copyTo(w)
	})
}

// jobFailed reports whether rec, a jobs.insert answer, is of a job that
// failed.
func jobFailed(rec *recorder) bool {
	var job struct {
		Status struct {
			ErrorResult *rowError `json:"errorResult"`
		} `json:"status"`
	}
	return json.Unmarshal(rec.body.Bytes(), &job) == nil && job.Status.ErrorResult != nil
}

// loadTableRows returns the rows of the table dest names, 0 when it does not
// exist, and whether it could be read. The emulator leaves numRows out
// for a table with none (it is omitempty).
func (f front) loadTableRows(r *http.Request, dest *tableRef) (int64, bool) {
	status, got := f.get(r, "/datasets/"+url.PathEscape(dest.DatasetID)+"/tables/"+url.PathEscape(dest.TableID))
	if status == http.StatusNotFound {
		return 0, true
	}
	var meta struct {
		NumRows json.Number `json:"numRows"`
	}
	if status != http.StatusOK || json.Unmarshal(got, &meta) != nil {
		return 0, false
	}
	if meta.NumRows == "" {
		return 0, true
	}
	n, err := meta.NumRows.Int64()
	return n, err == nil && n >= 0
}

// mediaCount counts the bytes of a multipart upload's data, its second
// part, as the body passes through to the emulator unchanged.
type mediaCount struct {
	pw   *io.PipeWriter
	done chan struct{}
	n    int64
	ok   bool
}

// countMedia returns a count of r's data, read as r's body is read, or nil
// for a body that is not a multipart one.
func countMedia(r *http.Request) *mediaCount {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" || r.Header.Get("Content-Encoding") != "" || r.Body == nil {
		return nil
	}
	pr, pw := io.Pipe()
	m := &mediaCount{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		defer func() { _, _ = io.Copy(io.Discard, pr) }()
		mr := multipart.NewReader(pr, params["boundary"])
		if _, err := mr.NextPart(); err != nil {
			return
		}
		p, err := mr.NextPart()
		if err != nil {
			return
		}
		n, err := io.Copy(io.Discard, p)
		m.n, m.ok = n, err == nil
	}()
	body := r.Body
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.TeeReader(body, pw), body}
	return m
}

// size returns the data's bytes, and whether the whole of it was read. It
// is called once the request has been served.
func (m *mediaCount) size() (int64, bool) {
	if m == nil {
		return 0, false
	}
	_ = m.pw.Close()
	<-m.done
	return m.n, m.ok
}
