package bigqueryfront

import (
	"encoding/json"
	"strconv"
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
// keeps them for jobs.get and jobs.list (jobFailures.loads). outputBytes,
// the size BigQuery stores the rows in, is not reported (#965): it is not the
// size of the data sent, and the front does not guess it. A load whose
// data the front passes on as it is, or does not see, reports what the
// emulator reports (#966).

// maxListedBadRecords bounds the bad records a job lists in
// status.errors. It is CloudBurrow's bound, to keep what the front holds
// small; BigQuery lists "the first errors" and does not say how many.
const maxListedBadRecords = 100

// loadCounts is what the front counted of a load's data.
type loadCounts struct {
	badRecords, outputRows, inputFiles, inputFileBytes int64
	errors                                             []rowError
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
	load["inputFiles"] = strconv.FormatInt(c.inputFiles, 10)
	load["inputFileBytes"] = strconv.FormatInt(c.inputFileBytes, 10)
	load["outputRows"] = strconv.FormatInt(c.outputRows, 10)
	load["badRecords"] = strconv.FormatInt(c.badRecords, 10)
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
