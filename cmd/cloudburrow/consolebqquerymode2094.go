package main

// The query editor's Edit > Query mode (#2094): "Click Edit > Query mode >
// Optional job creation. To confirm this choice, click Confirm"
// (https://docs.cloud.google.com/bigquery/docs/running-queries, Optional
// job creation mode) and "click Edit > Query mode > Continuous query. To
// confirm this choice, click Confirm"
// (https://docs.cloud.google.com/bigquery/docs/continuous-queries), both
// read 2026-11-23. The docs name no other mode.
//
// Optional job creation runs the query with jobs.query's jobCreationMode
// JOB_CREATION_OPTIONAL: the instance's BigQuery answers a query whose
// rows all came back with a queryId and no job (internal/bigqueryfront,
// optionaljob.go), and one it made a job for with the job and its
// jobCreationReason, whose results the editor reads as any job's.
// Continuous query sets the job's configuration.query.continuous, which
// BigQuery runs only on a CONTINUOUS reservation assignment
// (continuousjob.go).

import (
	"context"
	"fmt"

	bqv2 "google.golang.org/api/bigquery/v2"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func (p bigqueryProvider) studioRunOptional(ctx context.Context, svc *bqv2.Service, q studioQuery, cfg *bqv2.JobConfigurationQuery) (*studioResult, error) {
	req := &bqv2.QueryRequest{
		Query: q.Query, UseLegacySql: cfg.UseLegacySql, ForceSendFields: []string{"UseLegacySql"},
		DefaultDataset: cfg.DefaultDataset, Location: q.Location, JobCreationMode: "JOB_CREATION_OPTIONAL",
		MaxResults: int64(detailLimit), TimeoutMs: (dbTimeout - dbTimeout/4).Milliseconds(),
	}
	resp, err := svc.Jobs.Query(p.project, req).Context(ctx).Do()
	if err != nil {
		return nil, bigqueryRefusal(err)
	}
	res := &studioResult{Stages: []map[string]any{}}
	if ref := resp.JobReference; ref != nil && ref.JobId != "" {
		// BigQuery made a job: its results are read as any job's.
		out, err := p.studioResults(ctx, svc, res, ref)
		if err == nil && resp.JobCreationReason != nil {
			out.Job = append(out.Job, console.Property{Label: "Job creation reason", Value: resp.JobCreationReason.Code})
		}
		return out, err
	}
	res.QueryID = resp.QueryId
	res.TotalRows = resp.TotalRows
	res.BytesProcessed = resp.TotalBytesProcessed
	if resp.Schema != nil {
		res.Schema = studioFields(resp.Schema.Fields)
		for _, row := range resp.Rows {
			res.Rows = append(res.Rows, studioRow(row.F, resp.Schema.Fields))
		}
	}
	res.Message = "Optional job creation: BigQuery ran this query without creating a job."
	// The Job information tab of a query that made no job: there is no
	// job to read, so it has the query's ID and what the answer reports
	// (UNVERIFIED (re-read 2026-11-23: https://docs.cloud.google.com/bigquery/docs/running-queries): Google's docs do not
	// show this tab for a query run without a job).
	res.Job = []console.Property{
		{Label: "Query ID", Value: orDash(resp.QueryId)},
		{Label: "Job ID", Value: "No job created"},
		{Label: "Location", Value: orDash(resp.Location)},
		{Label: "Bytes processed", Value: studioBytes(resp.TotalBytesProcessed)},
		{Label: "Use legacy SQL", Value: fmt.Sprint(cfg.UseLegacySql != nil && *cfg.UseLegacySql)},
	}
	return res, nil
}
