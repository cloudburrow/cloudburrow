package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// A jobs.insert of a job ID the project already has (#1622).
//
// A client that chooses its job's ID may send the same jobs.insert again:
// the official Go client retries a jobs.insert with a jobReference after a
// transient failure (a 5xx, a reset connection), and the first attempt may
// have made the job even though its answer never reached the client.
// BigQuery answers such an insert 409 with reason "duplicate"
// (https://cloud.google.com/bigquery/docs/error-messages: "This error
// returns when trying to create a job, dataset, or table that already
// exists"), "Already Exists: Job project:LOCATION.id", and runs nothing.
// The emulator ran the query again, and only then, adding the job, failed
// "failed to add job: job … is already created" with 400 jobInternalError
// (measured): a DML statement ran a second time before the error rolled it
// back, and the answer was not one a client can recognise. The front's
// own records of the job (its times, its client's text, a failure it
// gave) were written again for the second insert, too.
//
// So, before anything else, a client's insert that names its job ID is
// answered 409 duplicate when the project has that job: one the front
// carried out itself (frontJobs) or one the emulator has (jobs.get). The
// emulator's patch 0076 answers the same for an insert that reaches it,
// as two inserts of one ID at once do. The front's own inserts name IDs
// it has just made, and are not looked up.

// duplicateJob answers r, a jobs.insert of the job id (as the client gave
// it), with 409 duplicate when the project already has the job, and
// reports whether it did.
func (f front) duplicateJob(w http.ResponseWriter, r *http.Request, project, id string) bool {
	if id == "" || isInternalRequest(r.Context()) {
		return false
	}
	location, ok := f.existingJob(r, project, id)
	if !ok {
		return false
	}
	writeError(w, http.StatusConflict, "duplicate", duplicateJobMessage(project, location, id))
	return true
}

// existingJob reports whether project has a job id, one of the front's own
// or one the emulator has, and its location (the project's default when
// the job names none).
func (f front) existingJob(r *http.Request, project, id string) (string, bool) {
	var job map[string]any
	if f.jobs != nil {
		job, _ = f.jobs.get(project, id)
	}
	if job == nil {
		status, body := f.get(r, "/jobs/"+url.PathEscape(id))
		if status != http.StatusOK || json.Unmarshal(body, &job) != nil {
			return "", false
		}
	}
	ref, _ := job["jobReference"].(map[string]any)
	location, _ := ref["location"].(string)
	if location == "" {
		location, _ = job["location"].(string)
	}
	if location == "" {
		location = defaultLocation
		if f.settings != nil {
			location = f.settings.defaultLocation(project)
		}
	}
	return location, true
}

// duplicateJobMessage is BigQuery's message for an insert of a job that
// exists. The form is BigQuery's for an existing resource ("Already
// Exists: <Kind> <name>"), the job named project:LOCATION.id as BigQuery
// names a job; UNVERIFIED wording (docs re-read 2026-10-18:
// https://docs.cloud.google.com/bigquery/docs/error-messages).
func duplicateJobMessage(project, location, id string) string {
	return fmt.Sprintf("Already Exists: Job %s:%s.%s", project, location, id)
}
