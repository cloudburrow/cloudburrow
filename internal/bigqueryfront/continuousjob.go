package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery/reservation/apiv1/reservationpb"
)

// Continuous queries (#2094, #2193).
//
// A query job's configuration.query.continuous runs it as a continuous
// query (https://docs.cloud.google.com/bigquery/docs/continuous-queries,
// https://docs.cloud.google.com/bigquery/docs/continuous-queries-introduction,
// read 2026-11-23): it processes the rows of its tables as they arrive,
// until it is cancelled, it fails, or it reaches its maximum runtime.
//
// What BigQuery requires of one, checked when the job is inserted:
//
//   - "an Enterprise edition or Enterprise Plus edition reservation with
//     a CONTINUOUS assignment type ... Continuous queries don't support the
//     on-demand compute billing model": the project (or its folder or
//     organization) has a CONTINUOUS assignment in the job's location
//     (reservations.go), to a reservation of either edition with a baseline
//     or autoscaling slots ("Idle slots from other reservations are not
//     enough"); "Admission needs 10 slots per job", of the at most 500 a
//     CONTINUOUS assignment's reservation has: a job beyond them waits,
//     PENDING to BigQuery and RUNNING here, until one ends;
//   - GoogleSQL (useLegacySql false), and the SQL continuoussql.go reads;
//   - a service account, in connectionProperties' service_account, to
//     export to Pub/Sub; a continuous query run as a user stops after 2
//     days (its token's life), one run as a service account after 150:
//     "When the maximum query runtime is reached, the query fails";
//     jobTimeoutMs, when given, ends it sooner;
//   - its sources: no external tables, INFORMATION_SCHEMA views, wildcard
//     tables, change data capture tables, materialized views, or tables
//     with column-level or row-level security; a view is read as its
//     query.
//
// The job then runs on a request of its own (asyncjobs.go): jobs.insert
// answers it RUNNING, jobs.get and jobs.list report it so, and every
// second it runs its statement over the rows appended in the window since
// its last run (continuoussql.go), up to a second before the instance's
// clock: an INSERT inserts their rows, an EXPORT DATA writes them to the
// instance's Pub/Sub, Bigtable or Spanner (continuousexport.go). The end
// of the last window is its output watermark, "the point up to which the
// continuous query has successfully processed data", which
// INFORMATION_SCHEMA.JOBS' continuous_query_info.output_watermark reads,
// with continuous TRUE and the reservation and edition. jobs.cancel stops
// it, "Job execution was cancelled: User requested cancellation" (reason
// stopped); a window that fails fails the job with its error; "if the
// output watermark lag exceeds 48 hours, the job fails".
//
// UNVERIFIED (docs re-read 2026-11-23: the pages above): the refusals'
// messages and their 400, which Google does not give; the poll interval
// and the one-second lag, which Google does not document (its own lag is
// "seconds"); the reason of a job that reached its maximum runtime
// (timeout here).

const (
	cqPoll            = time.Second
	cqLag             = time.Second
	cqSlotsPerJob     = 10
	cqMaxRunUser      = 2 * 24 * time.Hour
	cqMaxRunAccount   = 150 * 24 * time.Hour
	cqMaxWatermarkLag = 48 * time.Hour
)

var cqServiceAccount = regexp.MustCompile(`^[a-z0-9-]+@[a-z0-9-]+\.iam\.gserviceaccount\.com$|^[0-9]+-compute@developer\.gserviceaccount\.com$`)

// continuousJobs are the continuous queries running, and the watermarks
// of those that ended, for INFORMATION_SCHEMA.JOBS.
type continuousJobs struct {
	mu   sync.Mutex
	rest http.Handler
	runs map[string]*cqRun // project/id
	kept []string
}

// cqRun is one continuous query's state.
type cqRun struct {
	reservation string // its name
	slots       int64
	edition     string
	watermark   int64 // milliseconds, 0 before the first window
	running     bool
	admitted    bool
}

// cqRegistry is the process's continuous queries: one front per process.
var cqRegistry = &continuousJobs{}

func (c *continuousJobs) info(project, id string) (cqRun, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[project+"/"+id]
	if !ok {
		return cqRun{}, false
	}
	return *r, true
}

// admit registers run, and reports whether it may run: the reservation's
// slots (at most 500) hold 10 for each continuous query admitted.
func (c *continuousJobs) admit(key string, run *cqRun) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runs == nil {
		c.runs = map[string]*cqRun{}
	}
	if _, ok := c.runs[key]; !ok {
		c.kept = append(c.kept, key)
		for len(c.kept) > maxAsyncDone {
			if old := c.runs[c.kept[0]]; old != nil && !old.running {
				delete(c.runs, c.kept[0])
			}
			c.kept = c.kept[1:]
		}
	}
	c.runs[key] = run
	if run.admitted {
		return true
	}
	slots := run.slots
	if slots > maxContinuousSlots {
		slots = maxContinuousSlots
	}
	n := int64(0)
	for k, o := range c.runs {
		if k != key && o.running && o.admitted && o.reservation == run.reservation {
			n++
		}
	}
	if (n+1)*cqSlotsPerJob > slots {
		return false
	}
	run.admitted = true
	return true
}

func (c *continuousJobs) mark(key string, f func(*cqRun)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.runs[key]; r != nil {
		f(r)
	}
}

// call sends a request of the front's own through the whole front.
func (c *continuousJobs) call(r *http.Request, method, path string, body []byte) (int, []byte) {
	if c.rest == nil {
		return http.StatusServiceUnavailable, []byte(`{"error":{"message":"the front is starting"}}`)
	}
	var rd = bytes.NewReader(body)
	req, err := http.NewRequestWithContext(asInternal(r.Context()), method, "http://"+r.Host+path, rd)
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	for k, v := range r.Header {
		if k == "Content-Length" || k == "Content-Encoding" {
			continue
		}
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Host = r.Host
	rec := newRecorder()
	c.rest.ServeHTTP(rec, req)
	st := rec.status
	if st == 0 {
		st = http.StatusOK
	}
	return st, rec.body.Bytes()
}

// continuousConfig is what a continuous query's job sets beyond jobBody.
type continuousConfig struct {
	Configuration struct {
		DryRun       bool            `json:"dryRun"`
		JobTimeoutMs json.RawMessage `json:"jobTimeoutMs"`
		Query        struct {
			ConnectionProperties []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"connectionProperties"`
			DestinationTable *tableRef `json:"destinationTable"`
		} `json:"query"`
	} `json:"configuration"`
	JobReference struct {
		Location string `json:"location"`
	} `json:"jobReference"`
}

// continuous carries out a continuous query's jobs.insert, reporting
// false for any other job.
func (f front) continuous(w http.ResponseWriter, r *http.Request, project string, job jobBody, created int64) bool {
	q := job.Configuration.Query
	if q == nil || !q.Continuous {
		return false
	}
	fail := func(code int, msg string) bool {
		reason := "invalidQuery"
		switch code {
		case http.StatusNotImplemented:
			reason = "notImplemented"
		case http.StatusNotFound:
			reason = "notFound"
		}
		writeError(w, code, reason, msg)
		return true
	}
	var cc continuousConfig
	if !decodeJob(r, &cc) {
		return fail(400, "The job's configuration cannot be read")
	}
	if q.UseLegacySQL == nil || *q.UseLegacySQL {
		return fail(400, "Continuous queries must use GoogleSQL: set useLegacySql to false")
	}
	if cc.Configuration.Query.DestinationTable != nil {
		return fail(400, "Continuous queries do not support a destination table: the query's INSERT or EXPORT DATA statement names where its rows go")
	}
	location := cc.JobReference.Location
	if location == "" && f.settings != nil {
		location = f.settings.defaultLocation(project)
	}
	if location == "" {
		location = "US"
	}
	var ancestors []string
	if f.resourceManager != "" {
		chain, _, _ := f.ancestry(r.Context(), project)
		for _, a := range chain {
			switch a.ResourceID.Type {
			case "folder":
				ancestors = append(ancestors, "folders/"+a.ResourceID.ID)
			case "organization":
				ancestors = append(ancestors, "organizations/"+a.ResourceID.ID)
			}
		}
	}
	rv, ok := f.reservations.continuousReservation(project, location, ancestors)
	if !ok {
		return fail(400, fmt.Sprintf("Continuous queries must run on a reservation with a CONTINUOUS assignment; "+
			"project %s has none in %s and is billed on demand, which continuous queries do not support", project, location))
	}
	if e := rv.GetEdition(); e != reservationpb.Edition_ENTERPRISE && e != reservationpb.Edition_ENTERPRISE_PLUS {
		return fail(400, fmt.Sprintf("Continuous queries run on an Enterprise edition or Enterprise Plus edition reservation; %s is %s", rv.GetName(), e))
	}
	if reservationSlots(rv) == 0 {
		return fail(400, fmt.Sprintf("Reservation %s has no baseline or autoscaling slots: a continuous query needs a reservation with "+
			"a non-zero baseline or autoscaling, as idle slots from other reservations are not enough", rv.GetName()))
	}
	sa := ""
	for _, p := range cc.Configuration.Query.ConnectionProperties {
		if p.Key == "service_account" {
			sa = p.Value
		}
	}
	if sa != "" && !cqServiceAccount.MatchString(sa) {
		return fail(400, fmt.Sprintf("Invalid service_account %q: it is a service account's email", sa))
	}
	plan, err := parseContinuous(q.Query)
	if err != nil {
		e := err.(*cqRefusal)
		return fail(e.code, e.msg)
	}
	if plan.format == "CLOUD_PUBSUB" && sa == "" {
		return fail(400, "A continuous query that exports to Pub/Sub must run as a service account: set the connection property service_account")
	}
	dataset := defaultDatasetOf(q.queryOptions)
	if msg := f.resolveSources(r, plan.query, dataset, 0); msg != nil {
		return fail(msg.code, msg.msg)
	}
	if plan.insert != "" {
		parts := plan.target
		if len(parts) == 1 {
			if dataset == "" {
				return fail(400, fmt.Sprintf("Table %q must be qualified with a dataset (e.g. dataset.table).", parts[0]))
			}
			parts = []string{dataset, parts[0]}
		}
		ds, tb := parts[len(parts)-2], parts[len(parts)-1]
		if st, _ := cqRegistry.call(r, http.MethodGet, f.base+tablePath(ds, tb), nil); st == http.StatusNotFound {
			return fail(404, "Not found: Table "+project+":"+ds+"."+tb)
		}
	}
	if cc.Configuration.DryRun {
		stmt := "INSERT"
		if plan.insert == "" {
			stmt = "EXPORT_DATA"
		}
		ms := strconv.FormatInt(created, 10)
		writeJSON(w, http.StatusOK, map[string]any{"kind": "bigquery#job", "jobReference": map[string]any{"projectId": project, "jobId": job.JobReference.JobID, "location": location},
			"configuration": map[string]any{"query": map[string]any{"query": q.Query, "useLegacySql": false, "continuous": true}, "dryRun": true, "jobType": "QUERY"},
			"status":        map[string]any{"state": "DONE"},
			"statistics":    map[string]any{"creationTime": ms, "totalBytesProcessed": "0", "query": map[string]any{"statementType": stmt, "totalBytesProcessed": "0"}}})
		return true
	}
	var sink cqSink
	if plan.format != "" {
		sink, err = f.sinks.openSink(r.Context(), plan)
		if err != nil {
			if e, ok := err.(*cqRefusal); ok {
				return fail(e.code, e.msg)
			}
			return fail(400, "The continuous query's export cannot be opened: "+err.Error())
		}
		defer sink.close()
	}
	maxRun := cqMaxRunUser
	if sa != "" {
		maxRun = cqMaxRunAccount
	}
	if t := jobMs(cc.Configuration.JobTimeoutMs); t > 0 && time.Duration(t)*time.Millisecond < maxRun {
		maxRun = time.Duration(t) * time.Millisecond
	}
	id := job.JobReference.JobID
	key := project + "/" + id
	run := &cqRun{reservation: rv.GetName(), slots: reservationSlots(rv), edition: rv.GetEdition().String(), running: true}
	defer cqRegistry.mark(key, func(r *cqRun) { r.running = false })
	conf := map[string]any{}
	if raw, err := readBody(r); err == nil {
		var all struct {
			Configuration map[string]any `json:"configuration"`
		}
		if json.Unmarshal(raw, &all) == nil && all.Configuration != nil {
			conf = all.Configuration
		}
	}
	failJobWith := func(e rowError) bool {
		cqRegistry.mark(key, func(r *cqRun) { r.running = false })
		writeJSON(w, http.StatusOK, f.recordFailedJob(r, project, id, location, conf, e, created))
		return true
	}
	ctx := r.Context()
	start := f.now()
	for !cqRegistry.admit(key, run) {
		select {
		case <-ctx.Done():
			return fail(400, stoppedError().Message)
		case <-time.After(cqPoll):
		}
	}
	low := start.UnixMilli()
	first := true
	for {
		select {
		case <-ctx.Done():
			// Cancelled: asyncjobs.go records it stopped.
			writeError(w, http.StatusBadRequest, "stopped", stoppedError().Message)
			return true
		default:
		}
		now := f.now()
		if now.Sub(start) >= maxRun {
			if sa == "" && t0(cc.Configuration.JobTimeoutMs) {
				return failJobWith(rowError{Reason: "timeout", Message: "The continuous query ran as a user account, whose credentials last 2 days, " +
					"and reached its maximum runtime: run it as a service account to run it up to 150 days"})
			}
			if t0(cc.Configuration.JobTimeoutMs) {
				return failJobWith(rowError{Reason: "timeout", Message: "The continuous query reached its maximum runtime of 150 days"})
			}
			return failJobWith(timeoutError(maxRun))
		}
		high := now.Add(-cqLag).UnixMilli()
		if plan.changes && high-low > int64(24*time.Hour/time.Millisecond) {
			high = low + int64(24*time.Hour/time.Millisecond) // CHANGES reads at most a day
		}
		if high > low {
			if e := f.cqWindow(r, plan, q.queryOptions, location, sink, cqWindow{low: low, high: high, first: first}); e != nil {
				if ctx.Err() != nil {
					writeError(w, http.StatusBadRequest, "stopped", stoppedError().Message)
					return true
				}
				return failJobWith(*e)
			}
			low, first = high, false
			cqRegistry.mark(key, func(r *cqRun) { r.watermark = high })
		}
		if lag := now.UnixMilli() - low; time.Duration(lag)*time.Millisecond > cqMaxWatermarkLag {
			return failJobWith(rowError{Reason: "invalidQuery", Message: "The continuous query's output watermark lag exceeded 48 hours"})
		}
		if now.UnixMilli()-low > int64(cqLag/time.Millisecond)+1 && plan.changes {
			continue // catching up a day at a time
		}
		select {
		case <-ctx.Done():
		case <-time.After(cqPoll):
		}
	}
}

// t0 reports whether no jobTimeoutMs was given.
func t0(raw json.RawMessage) bool { return jobMs(raw) <= 0 }

// resolveSources checks a query's tables and reads its views (depth:
// views within views).
func (f front) resolveSources(r *http.Request, q *cqQuery, dataset string, depth int) *cqRefusal {
	project := projectOf(f.base)
	for i := range q.sources {
		s := &q.sources[i]
		parts := s.parts
		if len(parts) == 1 {
			if dataset == "" {
				return &cqRefusal{400, fmt.Sprintf("Table %q must be qualified with a dataset (e.g. dataset.table).", parts[0])}
			}
			parts = []string{dataset, parts[0]}
		}
		ds, tb := parts[len(parts)-2], parts[len(parts)-1]
		st, body := cqRegistry.call(r, http.MethodGet, f.base+tablePath(ds, tb), nil)
		if st == http.StatusNotFound {
			return &cqRefusal{404, "Not found: Table " + project + ":" + ds + "." + tb}
		}
		if st != http.StatusOK {
			return &cqRefusal{st, "Table " + ds + "." + tb + " cannot be read: " + errorMessage(body, st)}
		}
		var meta struct {
			Type         string `json:"type"`
			CreationTime string `json:"creationTime"`
			View         *struct {
				Query        string `json:"query"`
				UseLegacySQL *bool  `json:"useLegacySql"`
			} `json:"view"`
			Schema *struct {
				Fields []policyField `json:"fields"`
			} `json:"schema"`
		}
		_ = json.Unmarshal(body, &meta)
		name := ds + "." + tb
		switch meta.Type {
		case "EXTERNAL":
			return cqInvalid("external tables as sources: %s", name)
		case "MATERIALIZED_VIEW":
			return cqInvalid("materialized views as sources: %s", name)
		case "SNAPSHOT":
			return cqInvalid("table snapshots as sources: %s", name)
		case "VIEW":
			if s.call != "" {
				break // APPENDS refuses a view itself
			}
			if depth >= 8 || meta.View == nil {
				return cqInvalid("the view %s", name)
			}
			if meta.View.UseLegacySQL == nil || *meta.View.UseLegacySQL {
				return cqInvalid("the legacy SQL view %s", name)
			}
			vq, err := parseCQQuery(meta.View.Query)
			if err != nil {
				e := err.(*cqRefusal)
				return &cqRefusal{e.code, "View " + name + ": " + e.msg}
			}
			if msg := f.resolveSources(r, vq, ds, depth+1); msg != nil {
				return msg
			}
			s.view = vq
		}
		if meta.Schema != nil && hasPolicyTags(meta.Schema.Fields) {
			return cqInvalid("tables with column-level security as sources: %s", name)
		}
		if pols, _ := f.policies.list(policyKey(project, ds, tb)); len(pols) > 0 {
			return cqInvalid("tables with row-level security as sources: %s", name)
		}
		if f.cdcStreams.active(project, ds, tb) {
			return cqInvalid("change data capture tables as sources: %s", name)
		}
		if _, ok := f.cdcStreams.watermark(project, ds, tb, meta.CreationTime); ok {
			return cqInvalid("change data capture tables as sources: %s", name)
		}
		s.parts = parts
	}
	return nil
}

// policyField is a schema field's policy tags, and its fields'.
type policyField struct {
	PolicyTags *struct {
		Names []string `json:"names"`
	} `json:"policyTags"`
	Fields []policyField `json:"fields"`
}

func hasPolicyTags(fields []policyField) bool {
	for _, fl := range fields {
		if fl.PolicyTags != nil && len(fl.PolicyTags.Names) > 0 {
			return true
		}
		if hasPolicyTags(fl.Fields) {
			return true
		}
	}
	return false
}

// cqWindow runs plan over window w: an INSERT inserts, an export reads
// the rows and writes them to sink.
func (f front) cqWindow(r *http.Request, plan *cqPlan, q queryOptions, location string, sink cqSink, w cqWindow) *rowError {
	stmt := plan.statement(w, defaultDatasetOf(q))
	body, _ := json.Marshal(map[string]any{"query": stmt, "useLegacySql": false, "defaultDataset": q.DefaultDataset,
		"parameterMode": q.ParameterMode, "queryParameters": q.QueryParameters, "maxResults": 10000, "timeoutMs": 600000, "location": location})
	st, got := cqRegistry.call(r, http.MethodPost, f.base+"/queries", body)
	if st != http.StatusOK {
		return &rowError{Reason: "invalidQuery", Message: errorMessage(got, st)}
	}
	if sink == nil {
		return nil
	}
	type page struct {
		Schema struct {
			Fields []cqField `json:"fields"`
		} `json:"schema"`
		Rows         []json.RawMessage `json:"rows"`
		PageToken    string            `json:"pageToken"`
		JobComplete  *bool             `json:"jobComplete"`
		JobReference struct {
			JobID    string `json:"jobId"`
			Location string `json:"location"`
		} `json:"jobReference"`
	}
	var p page
	if err := json.Unmarshal(got, &p); err != nil {
		return &rowError{Reason: "internalError", Message: "cloudburrow: the window's results cannot be read: " + err.Error()}
	}
	for p.JobComplete != nil && !*p.JobComplete {
		path := f.base + "/queries/" + url.PathEscape(p.JobReference.JobID) + "?timeoutMs=60000&maxResults=10000&location=" + url.QueryEscape(p.JobReference.Location)
		st, got = cqRegistry.call(r, http.MethodGet, path, nil)
		if st != http.StatusOK {
			return &rowError{Reason: "invalidQuery", Message: errorMessage(got, st)}
		}
		p = page{}
		if err := json.Unmarshal(got, &p); err != nil {
			return &rowError{Reason: "internalError", Message: err.Error()}
		}
		if r.Context().Err() != nil {
			return &rowError{Reason: "stopped", Message: stoppedError().Message}
		}
	}
	fields := p.Schema.Fields
	for {
		rows := make([][]any, 0, len(p.Rows))
		for _, raw := range p.Rows {
			rows = append(rows, decodeCQRow(fields, raw))
		}
		if len(rows) > 0 {
			if err := sink.write(r.Context(), fields, rows, f.now()); err != nil {
				if e, ok := err.(*cqRefusal); ok {
					return &rowError{Reason: "invalidQuery", Message: e.msg}
				}
				return &rowError{Reason: "backendError", Message: "The continuous query's export failed: " + err.Error()}
			}
		}
		if p.PageToken == "" || p.JobReference.JobID == "" {
			return nil
		}
		path := f.base + "/queries/" + url.PathEscape(p.JobReference.JobID) + "?maxResults=10000&pageToken=" + url.QueryEscape(p.PageToken) +
			"&location=" + url.QueryEscape(p.JobReference.Location)
		st, got = cqRegistry.call(r, http.MethodGet, path, nil)
		if st != http.StatusOK {
			return &rowError{Reason: "invalidQuery", Message: errorMessage(got, st)}
		}
		p = page{}
		if err := json.Unmarshal(got, &p); err != nil {
			return &rowError{Reason: "internalError", Message: err.Error()}
		}
	}
}

// decodeCQRow is a row of the REST API's f/v form as values: a scalar's
// string, a RECORD's []any of its fields, a REPEATED field's []any.
func decodeCQRow(fields []cqField, raw json.RawMessage) []any {
	var row struct {
		F []struct {
			V json.RawMessage `json:"v"`
		} `json:"f"`
	}
	_ = json.Unmarshal(raw, &row)
	out := make([]any, len(fields))
	for i, fl := range fields {
		if i < len(row.F) {
			out[i] = decodeCQValue(fl, row.F[i].V)
		}
	}
	return out
}

func decodeCQValue(fl cqField, v json.RawMessage) any {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	if fl.Mode == "REPEATED" {
		var items []struct {
			V json.RawMessage `json:"v"`
		}
		_ = json.Unmarshal(v, &items)
		one := fl
		one.Mode = ""
		out := make([]any, 0, len(items))
		for _, it := range items {
			out = append(out, decodeCQValue(one, it.V))
		}
		return out
	}
	if fl.Type == "RECORD" || fl.Type == "STRUCT" {
		return decodeCQRow(fl.Fields, v)
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return string(v)
}

// continuousInfo is a job's JOBS values: continuous, its
// continuous_query_info, reservation_id and edition (SQL).
func continuousInfo(project, id string, qc map[string]any) (continuous, info, reservation, edition string) {
	continuous, info, reservation, edition = "FALSE", "NULL", "NULL", "NULL"
	if c, _ := qc["continuous"].(bool); !c {
		return
	}
	continuous = "TRUE"
	run, ok := cqRegistry.info(project, id)
	if !ok {
		return
	}
	if run.watermark > 0 {
		info = "STRUCT(TIMESTAMP_MILLIS(" + strconv.FormatInt(run.watermark, 10) + ") AS output_watermark)"
	}
	// "RESERVATION_ADMIN_PROJECT:RESERVATION_LOCATION.RESERVATION_NAME".
	parts := strings.Split(run.reservation, "/")
	if len(parts) == 6 {
		reservation = sqlString(parts[1] + ":" + parts[3] + "." + parts[5])
	}
	edition = sqlString(run.edition)
	return
}
