package bigqueryfront

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// The front's own jobs (#1555).
//
// The front runs queries of its own: the reads a check needs, a query that
// reads a result's columns, the change history's bounds, the table of
// INFORMATION_SCHEMA.JOBS rows (jobstable.go), the Storage Read front's
// reads (storagerows.go). Each is a job on the emulator, and no client's.
// The front knew them by job ID in memory alone (jobRecords.internal), the
// last maxJobRecords of them, so after a front restart, or 5,000 of them
// later, jobs.list and INFORMATION_SCHEMA.JOBS listed every earlier one as
// the client's: measured in the BigQuery compat suite (shuffled, seed
// 1792237754362941000, --services bigquery,storage), jobs.list listed 1,306
// jobs before TestBigQueryStorageWriteStreamsOutliveAFrontRestart restarted
// the front and 3,684 after it, 290 of the extra ones the change history's
// SELECT MIN(googlesqlite_change_micros) and 257 the CREATE TABLE of a
// kept deletion's copy.
//
// The mark is now the job's ID, which the engine keeps with the job for as
// long as it keeps the job. A request of the front's own (send, dpRows)
// carries internalJobKey in its context; JobIDPrefix sends it on to the
// emulator with the header bigquery-emulator patch 0057 reads, and the
// engine begins the ID of each job it makes for the request with
// internalJobPrefix. A job so named is left out of jobs.list (and so of
// INFORMATION_SCHEMA.JOBS and its variants, and the console's Job
// history, which read it), and jobs.get, jobs.getQueryResults, jobs.cancel
// and jobs.delete of one answer 404, as BigQuery answers a job it does not
// have: no client made it. A client's jobs.insert naming a job ID with the
// prefix is refused (400), since its job would be hidden so; BigQuery
// reserves no job ID, but this one prefix is CloudBurrow's.
//
// The jobs of a script the front carries out statement by statement are
// the script's children (scriptjobs.go), the client's, and are not so
// named. jobRecords.internal still hides one until the script is answered,
// and hides jobs made before the front ran with patch 0057; the JOBS
// table's own statements of that time are known by their text
// (internalText, #1553).

// internalJobPrefix begins the ID of each job of the front's own.
const internalJobPrefix = "_cloudburrow_"

// jobIDPrefixHeader is the header bigquery-emulator patch 0057 reads.
const jobIDPrefixHeader = "X-Cloudburrow-Job-Id-Prefix"

// internalJobKey marks a request's context as the front's own (above).
type internalJobKey struct{}

// asInternal returns ctx marked as the front's own.
func asInternal(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalJobKey{}, true)
}

// isInternalRequest reports whether ctx is the front's own.
func isInternalRequest(ctx context.Context) bool {
	v, _ := ctx.Value(internalJobKey{}).(bool)
	return v
}

// isInternalJobID reports whether id names a job of the front's own.
func isInternalJobID(id string) bool {
	return strings.HasPrefix(id, internalJobPrefix)
}

// jobIDFor is a new job ID for a job the front names on ctx's request:
// one of its own when ctx is (newJobID).
func jobIDFor(ctx context.Context) string {
	if isInternalRequest(ctx) {
		return internalJobPrefix + newJobID()
	}
	return newJobID()
}

// JobIDPrefix returns next, the path to the emulator, with each request
// of the front's own (internalJobKey) sent with internalJobPrefix in the
// header the engine reads; a client's own header is dropped.
func JobIDPrefix(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isInternalRequest(r.Context()) {
			r = r.Clone(r.Context())
			r.Header.Set(jobIDPrefixHeader, internalJobPrefix)
		} else if r.Header.Get(jobIDPrefixHeader) != "" {
			r = r.Clone(r.Context())
			r.Header.Del(jobIDPrefixHeader)
		}
		next.ServeHTTP(w, r)
	})
}

// serveInternalJob answers a client's request of one job (jobs.get,
// jobs.getQueryResults, jobs.cancel, jobs.delete) of the front's own with
// BigQuery's 404, and reports whether it did. project and id are the
// path's, id still escaped.
func serveInternalJob(w http.ResponseWriter, r *http.Request, project, id string) bool {
	if u, err := url.PathUnescape(id); err == nil {
		id = u
	}
	if !isInternalJobID(id) || isInternalRequest(r.Context()) {
		return false
	}
	loc := r.URL.Query().Get("location")
	if loc == "" {
		loc = "US"
	}
	writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Job %s:%s.%s", project, strings.ToUpper(loc), id))
	return true
}

// refuseInternalJobID answers a client's jobs.insert naming a job ID of
// the front's own (above) with 400, and reports whether it did.
func refuseInternalJobID(w http.ResponseWriter, r *http.Request, id string) bool {
	if !isInternalJobID(id) || isInternalRequest(r.Context()) {
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("Invalid job ID %q: job IDs beginning %q are reserved by CloudBurrow for its own queries", id, internalJobPrefix))
	return true
}
