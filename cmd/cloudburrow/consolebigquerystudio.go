package main

// BigQuery Studio's calls (#1861): the Explorer pane's tree, a query editor
// whose runs are jobs (a dry run for the validator, query settings for the
// destination table, SQL dialect, priority and location), and the Personal
// and Project history panes. The screen is console.js's renderBigQueryStudio;
// it reaches these through POST /api/studio/bigquery (console.Studio).
//
// A statement that is not a single SELECT runs only when the request says
// Write, which the editor sends after a confirmation that names the project,
// as the Read-write mode of the dataset's Query tab does (#994): BigQuery has
// no transaction to undo it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	bqv2 "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// studioNode is one dataset of the Explorer, with what it holds.
type studioNode struct {
	ID       string        `json:"id"`
	Location string        `json:"location,omitempty"`
	Tables   []studioTable `json:"tables"`
	Routines []string      `json:"routines"`
	Models   []string      `json:"models"`
}

type studioTable struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// studioQuery is a run of the editor, or its dry run.
type studioQuery struct {
	Query  string `json:"query"`
	DryRun bool   `json:"dryRun"`
	// Write is set once the user confirmed a statement that changes data.
	Write bool `json:"write"`
	// The Edit menu's Query settings.
	LegacySQL          bool   `json:"legacySql"`
	Priority           string `json:"priority"`
	Location           string `json:"location"`
	DestinationDataset string `json:"destinationDataset"`
	DestinationTable   string `json:"destinationTable"`
	WriteDisposition   string `json:"writeDisposition"`
	DefaultDataset     string `json:"defaultDataset"`
	// QueryMode is the Edit menu's Query mode (#2094): "" (neither),
	// "optional" (Optional job creation) or "continuous" (Continuous
	// query).
	QueryMode string `json:"queryMode"`
	// ServiceAccount is Query settings' Continuous query > Use service
	// account (#2193): the continuous query runs as it, sent as the
	// connection property service_account.
	ServiceAccount string `json:"serviceAccount"`
}

type studioField struct {
	Name   string        `json:"name"`
	Type   string        `json:"type"`
	Mode   string        `json:"mode,omitempty"`
	Fields []studioField `json:"fields,omitempty"`
}

type studioResult struct {
	// Confirm asks the editor to confirm a statement that changes data
	// before it is sent with Write.
	Confirm        string             `json:"confirm,omitempty"`
	BytesProcessed int64              `json:"bytesProcessed"`
	StatementType  string             `json:"statementType,omitempty"`
	Job            []console.Property `json:"job,omitempty"`
	JobID          string             `json:"jobId,omitempty"`
	Location       string             `json:"location,omitempty"`
	Schema         []studioField      `json:"schema,omitempty"`
	Rows           []map[string]any   `json:"rows,omitempty"`
	TotalRows      uint64             `json:"totalRows"`
	Message        string             `json:"message,omitempty"`
	// QueryID is the query's ID when Optional job creation ran it without
	// a job (#2094).
	QueryID string           `json:"queryId,omitempty"`
	Stages  []map[string]any `json:"stages"`
}

func (p bigqueryProvider) StudioCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	p = p.in(project)
	if prompt := p.scope(project); prompt != "" {
		return nil, errors.New(prompt)
	}
	if call == "runtime" {
		return p.studioRuntime(ctx, project, body)
	}
	ctx, cancel := context.WithTimeout(ctx, bigqueryJobTimeout)
	defer cancel()
	svc, err := bqv2.NewService(ctx, option.WithEndpoint("http://"+p.endpoint), option.WithoutAuthentication())
	if err != nil {
		return nil, fmt.Errorf("cannot reach BigQuery: %w", err)
	}
	switch call {
	case "explorer":
		return p.studioExplorer(ctx, svc)
	case "complete":
		return p.studioComplete(ctx, svc, body)
	case "query":
		var q studioQuery
		if err := json.Unmarshal(body, &q); err != nil {
			return nil, fmt.Errorf("a query: %w", err)
		}
		res, err := p.studioRun(ctx, svc, q)
		if err != nil && q.DryRun {
			// The validator's verdict is an answer, not a failed call: an
			// invalid statement is what it is there to say (#1861).
			return map[string]any{"error": err.Error()}, nil
		}
		return res, err
	case "savedqueries", "savequery", "deletequery":
		return p.studioSaved(call, body)
	case "jobs":
		var req struct {
			All bool `json:"all"`
		}
		_ = json.Unmarshal(body, &req)
		return p.studioJobs(ctx, svc, req.All)
	}
	return nil, fmt.Errorf("unknown studio call %q", call)
}

// studioSaved answers the saved-query calls: the project's list, Save
// query (a new one, or a new version of the one named by id) and Delete.
func (p bigqueryProvider) studioSaved(call string, body json.RawMessage) (any, error) {
	store := bqSavedQueryStore{path: p.savedQueries}
	var in bqSavedQuery
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("a saved query: %w", err)
	}
	switch call {
	case "savequery":
		if err := p.writable(p.project); err != nil {
			return nil, err
		}
		return store.save(p.project, in, time.Now())
	case "deletequery":
		if err := p.writable(p.project); err != nil {
			return nil, err
		}
		if err := store.remove(p.project, in.ID); err != nil {
			return nil, err
		}
		return map[string]string{"deleted": in.ID}, nil
	}
	all, err := store.list(p.project)
	if err != nil {
		return nil, err
	}
	return map[string]any{"queries": all}, nil
}

func (p bigqueryProvider) studioExplorer(ctx context.Context, svc *bqv2.Service) (any, error) {
	var nodes []studioNode
	err := svc.Datasets.List(p.project).Pages(ctx, func(l *bqv2.DatasetList) error {
		for _, d := range l.Datasets {
			if d.DatasetReference == nil || len(nodes) >= detailLimit {
				continue
			}
			nodes = append(nodes, studioNode{ID: d.DatasetReference.DatasetId, Location: d.Location})
		}
		return nil
	})
	if projectNotFound(err, p.project) {
		return nil, errors.New(notServed(p.project))
	}
	if err != nil {
		return nil, bigqueryRefusal(err)
	}
	for i := range nodes {
		n := &nodes[i]
		n.Tables, n.Routines, n.Models = []studioTable{}, []string{}, []string{}
		_ = svc.Tables.List(p.project, n.ID).Pages(ctx, func(l *bqv2.TableList) error {
			for _, t := range l.Tables {
				if t.TableReference != nil && len(n.Tables) < detailLimit {
					n.Tables = append(n.Tables, studioTable{ID: t.TableReference.TableId, Type: t.Type})
				}
			}
			return nil
		})
		if r, err := svc.Routines.List(p.project, n.ID).Context(ctx).Do(); err == nil {
			for _, rt := range r.Routines {
				if rt.RoutineReference != nil {
					n.Routines = append(n.Routines, rt.RoutineReference.RoutineId)
				}
			}
		}
		if m, err := svc.Models.List(p.project, n.ID).Context(ctx).Do(); err == nil {
			for _, md := range m.Models {
				if md.ModelReference != nil {
					n.Models = append(n.Models, md.ModelReference.ModelId)
				}
			}
		}
		sort.Slice(n.Tables, func(a, b int) bool { return n.Tables[a].ID < n.Tables[b].ID })
		sort.Strings(n.Routines)
		sort.Strings(n.Models)
	}
	sort.Slice(nodes, func(a, b int) bool { return nodes[a].ID < nodes[b].ID })
	if nodes == nil {
		nodes = []studioNode{}
	}
	return map[string]any{"project": p.project, "datasets": nodes}, nil
}

// studioRun runs the editor's statement as a query job, or dry-runs it.
func (p bigqueryProvider) studioRun(ctx context.Context, svc *bqv2.Service, q studioQuery) (*studioResult, error) {
	q.Query = strings.TrimSpace(q.Query)
	if q.Query == "" {
		return nil, errors.New("the query is empty")
	}
	if !q.DryRun && !q.Write && !q.LegacySQL {
		if readOnlyBigQuery(q.Query) != nil {
			if _, err := writableBigQuery(q.Query); err != nil {
				return nil, err
			}
			return &studioResult{Confirm: fmt.Sprintf("This statement can change data in project %s. "+
				"BigQuery has no transaction to undo it: what it creates, replaces, drops, adds, changes or deletes "+
				"is changed when it runs.", p.project), Stages: []map[string]any{}}, nil
		}
	}
	legacy := q.LegacySQL
	cfg := &bqv2.JobConfigurationQuery{Query: q.Query, UseLegacySql: &legacy, ForceSendFields: []string{"UseLegacySql"}}
	switch q.Priority {
	case "", "INTERACTIVE":
	case "BATCH":
		cfg.Priority = "BATCH"
	default:
		return nil, fmt.Errorf("unknown priority %q: INTERACTIVE or BATCH", q.Priority)
	}
	if q.DefaultDataset != "" {
		cfg.DefaultDataset = &bqv2.DatasetReference{ProjectId: p.project, DatasetId: q.DefaultDataset}
	}
	if q.DestinationTable != "" {
		if q.DestinationDataset == "" {
			return nil, errors.New("a destination table needs its dataset")
		}
		cfg.DestinationTable = &bqv2.TableReference{ProjectId: p.project, DatasetId: q.DestinationDataset, TableId: q.DestinationTable}
		switch q.WriteDisposition {
		case "", "WRITE_EMPTY", "WRITE_APPEND", "WRITE_TRUNCATE":
			cfg.WriteDisposition = q.WriteDisposition
		default:
			return nil, fmt.Errorf("unknown write preference %q", q.WriteDisposition)
		}
	}
	switch q.QueryMode {
	case "":
	case "continuous":
		cfg.Continuous = true
		if sa := strings.TrimSpace(q.ServiceAccount); sa != "" {
			cfg.ConnectionProperties = append(cfg.ConnectionProperties, &bqv2.ConnectionProperty{Key: "service_account", Value: sa})
		}
	case "optional":
		// jobs.query, the only call that takes jobCreationMode; a
		// destination table or batch priority needs a job's configuration,
		// so such a query runs as a job.
		if !q.DryRun && q.DestinationTable == "" && cfg.Priority == "" {
			return p.studioRunOptional(ctx, svc, q, cfg)
		}
	default:
		return nil, fmt.Errorf("unknown query mode %q: optional or continuous", q.QueryMode)
	}
	job := &bqv2.Job{
		Configuration: &bqv2.JobConfiguration{Query: cfg, DryRun: q.DryRun},
	}
	if q.Location != "" {
		job.JobReference = &bqv2.JobReference{ProjectId: p.project, Location: q.Location}
	}
	inserted, err := svc.Jobs.Insert(p.project, job).Context(ctx).Do()
	if err != nil {
		return nil, bigqueryRefusal(err)
	}
	res := &studioResult{Stages: []map[string]any{}}
	if s := inserted.Statistics; s != nil {
		res.BytesProcessed = s.TotalBytesProcessed
		if s.Query != nil {
			res.StatementType = s.Query.StatementType
			if s.Query.TotalBytesProcessed > res.BytesProcessed {
				res.BytesProcessed = s.Query.TotalBytesProcessed
			}
		}
	}
	if q.DryRun {
		return res, nil
	}
	if inserted.Status != nil && inserted.Status.ErrorResult != nil {
		return nil, errors.New(inserted.Status.ErrorResult.Message)
	}
	ref := inserted.JobReference
	if ref == nil || ref.JobId == "" {
		return nil, errors.New("BigQuery answered without a job reference")
	}
	if cfg.Continuous {
		return p.studioContinuous(ctx, svc, res, ref)
	}
	return p.studioResults(ctx, svc, res, ref)
}

// studioResults reads the results and the job of a query job.
func (p bigqueryProvider) studioResults(ctx context.Context, svc *bqv2.Service, res *studioResult, ref *bqv2.JobReference) (*studioResult, error) {
	res.JobID, res.Location = ref.JobId, ref.Location
	results, err := svc.Jobs.GetQueryResults(p.project, ref.JobId).Location(ref.Location).
		MaxResults(int64(detailLimit)).TimeoutMs((dbTimeout - dbTimeout/4).Milliseconds()).Context(ctx).Do()
	if err != nil {
		return nil, bigqueryRefusal(err)
	}
	if results.Schema != nil {
		res.Schema = studioFields(results.Schema.Fields)
		for _, row := range results.Rows {
			res.Rows = append(res.Rows, studioRow(row.F, results.Schema.Fields))
		}
	}
	res.TotalRows = results.TotalRows
	if results.NumDmlAffectedRows != 0 || strings.HasPrefix(res.StatementType, "INSERT") ||
		res.StatementType == "UPDATE" || res.StatementType == "DELETE" || res.StatementType == "MERGE" {
		res.Message = fmt.Sprintf("This statement affected %d rows.", results.NumDmlAffectedRows)
	}
	if !results.JobComplete {
		res.Message = "The job is still running: Personal history shows how it ends."
	}
	full, err := svc.Jobs.Get(p.project, ref.JobId).Location(ref.Location).Context(ctx).Do()
	if err == nil {
		res.Job = studioJobInfo(full)
		if full.Statistics != nil && full.Statistics.Query != nil {
			if full.Statistics.Query.StatementType != "" {
				res.StatementType = full.Statistics.Query.StatementType
			}
			if b := full.Statistics.TotalBytesProcessed; b > res.BytesProcessed {
				res.BytesProcessed = b
			}
			for _, st := range full.Statistics.Query.QueryPlan {
				res.Stages = append(res.Stages, map[string]any{
					"name": st.Name, "status": st.Status, "recordsRead": st.RecordsRead,
					"recordsWritten": st.RecordsWritten, "inputStages": st.InputStages,
				})
			}
		}
	}
	return res, nil
}

// studioJobInfo is the Job information tab: the fields Google's tab names, as
// the job reports them; one it does not report is a dash.
func studioJobInfo(j *bqv2.Job) []console.Property {
	s := j.Statistics
	if s == nil {
		s = &bqv2.JobStatistics{}
	}
	duration := "—"
	if s.StartTime > 0 && s.EndTime >= s.StartTime {
		duration = (time.Duration(s.EndTime-s.StartTime) * time.Millisecond).String()
	}
	q := j.Configuration
	priority, legacy, dest := "INTERACTIVE", "false", "—"
	if q != nil && q.Query != nil {
		if q.Query.Priority != "" {
			priority = q.Query.Priority
		}
		if q.Query.UseLegacySql != nil {
			legacy = strconv.FormatBool(*q.Query.UseLegacySql)
		}
		if t := q.Query.DestinationTable; t != nil && strings.HasPrefix(t.DatasetId, "_") {
			// A hidden dataset's: the anonymous table BigQuery writes a
			// query's results to when no destination is set.
			dest = "Temporary table"
		} else if t != nil {
			dest = t.ProjectId + "." + t.DatasetId + "." + t.TableId
		}
	}
	billed := "—"
	if s.Query != nil {
		billed = studioBytes(s.Query.TotalBytesBilled)
	}
	id, loc := "—", "—"
	if j.JobReference != nil {
		id = j.JobReference.ProjectId + ":" + j.JobReference.Location + "." + j.JobReference.JobId
		loc = orDash(j.JobReference.Location)
	}
	return []console.Property{
		{Label: "Job ID", Value: id},
		{Label: "User", Value: orDash(j.UserEmail)},
		{Label: "Location", Value: loc},
		{Label: "Creation time", Value: orDash(millis(s.CreationTime))},
		{Label: "Start time", Value: orDash(millis(s.StartTime))},
		{Label: "End time", Value: orDash(millis(s.EndTime))},
		{Label: "Duration", Value: duration},
		{Label: "Bytes processed", Value: studioBytes(s.TotalBytesProcessed)},
		{Label: "Bytes billed", Value: billed},
		{Label: "Job priority", Value: priority},
		{Label: "Use legacy SQL", Value: legacy},
		{Label: "Destination table", Value: dest},
	}
}

// studioBytes is a byte count as Google's validator writes it: B, KB, MB,
// GB, TB, in powers of 1024.
func studioBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.2f %s", f, units[i])
}

func studioFields(fs []*bqv2.TableFieldSchema) []studioField {
	out := make([]studioField, 0, len(fs))
	for _, f := range fs {
		out = append(out, studioField{Name: f.Name, Type: f.Type, Mode: f.Mode, Fields: studioFields(f.Fields)})
	}
	return out
}

// studioRow is one result row as the JSON tab shows it: a RECORD an object,
// a REPEATED field an array, every leaf as BigQuery's REST answer carries it
// (a string, or null).
func studioRow(cells []*bqv2.TableCell, fs []*bqv2.TableFieldSchema) map[string]any {
	row := make(map[string]any, len(fs))
	for i, f := range fs {
		if i >= len(cells) || cells[i] == nil {
			row[f.Name] = nil
			continue
		}
		row[f.Name] = studioValue(cells[i].V, f, f.Mode == "REPEATED")
	}
	return row
}

func studioValue(v any, f *bqv2.TableFieldSchema, repeated bool) any {
	if v == nil {
		return nil
	}
	if repeated {
		items, _ := v.([]any)
		out := make([]any, 0, len(items))
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				out = append(out, studioValue(m["v"], f, false))
			}
		}
		return out
	}
	if f.Type == "RECORD" || f.Type == "STRUCT" {
		m, _ := v.(map[string]any)
		raw, _ := m["f"].([]any)
		rec := make(map[string]any, len(f.Fields))
		for i, sub := range f.Fields {
			if i < len(raw) {
				cell, _ := raw[i].(map[string]any)
				rec[sub.Name] = studioValue(cell["v"], sub, sub.Mode == "REPEATED")
			}
		}
		return rec
	}
	return v
}

// studioJobs is Personal history (the caller's jobs) or Project history
// (allUsers): jobs.list with the full projection, newest first.
func (p bigqueryProvider) studioJobs(ctx context.Context, svc *bqv2.Service, all bool) (any, error) {
	type row struct {
		ID       string `json:"id"`
		Location string `json:"location"`
		Type     string `json:"type"`
		State    string `json:"state"`
		Created  string `json:"created"`
		User     string `json:"user"`
		Query    string `json:"query,omitempty"`
		Bytes    string `json:"bytes"`
		Error    string `json:"error,omitempty"`
	}
	rows := []row{}
	page, err := svc.Jobs.List(p.project).AllUsers(all).Projection("full").MaxResults(int64(bigqueryJobsShown)).Context(ctx).Do()
	if err != nil {
		return nil, bigqueryRefusal(err)
	}
	for _, j := range page.Jobs {
		if j.JobReference == nil {
			continue
		}
		r := row{ID: j.JobReference.JobId, Location: j.JobReference.Location, Type: jobType(j.Configuration),
			State: listedJobState(j), Created: jobTime(j.Statistics), User: orDash(j.UserEmail),
			Bytes: "—", Error: errorText(j.ErrorResult)}
		if j.Configuration != nil && j.Configuration.Query != nil {
			r.Query = j.Configuration.Query.Query
		}
		if j.Statistics != nil {
			r.Bytes = studioBytes(j.Statistics.TotalBytesProcessed)
		}
		rows = append(rows, r)
	}
	return map[string]any{"jobs": rows, "all": all}, nil
}

var _ console.Studio = bigqueryProvider{}

// listedJobState is a listed job's state: jobs.list's own state and
// errorResult, or, where the answer leaves them out, the job's status's.
func listedJobState(j *bqv2.JobListJobs) string {
	state, errorResult := j.State, j.ErrorResult
	if j.Status != nil {
		if state == "" {
			state = j.Status.State
		}
		if errorResult == nil {
			errorResult = j.Status.ErrorResult
		}
	}
	return jobStateWord(state, errorResult)
}
