//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"
)

// TestBigQueryCSVLoadReportsBadRecords (#960): a CSV load that leaves out
// bad records (maxBadRecords) succeeds and reports them as BigQuery does:
// statistics.load.badRecords counts them, status.errors lists them, and
// statistics.load has the rows loaded, the files and their bytes, in the
// jobs.insert answer, jobs.get and jobs.list. A load with
// ignoreUnknownValues trims the wide records and reports none bad.
//
// Read through the official Go client, cloud.google.com/go/bigquery
// (JobStatus.Errors, LoadStatistics), and, as that client's
// LoadStatistics has no BadRecords, through the official generated
// client it is built on, google.golang.org/api/bigquery/v2
// (JobStatistics3.BadRecords). Measured first, on the front of #952: the
// load left the bad records out, and jobs.get gave it no statistics and
// no errors.
func TestBigQueryCSVLoadReportsBadRecords(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	svc, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("bigquery/v2 NewService: %v", err)
	}

	const data = "1,x\n2\n3,y,z\n4,w\n"
	for _, c2 := range []struct {
		table   string
		set     func(*bigquery.ReaderSource)
		bad     int64
		rows    int64
		errLine []string
	}{
		{"bad_left_out", func(s *bigquery.ReaderSource) { s.MaxBadRecords = 2 }, 2, 2, []string{"Line 2", "Line 3"}},
		{"unknown_trimmed", func(s *bigquery.ReaderSource) { s.IgnoreUnknownValues = true; s.AllowJaggedRows = true }, 0, 4, nil},
	} {
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.Schema = loadSchema
		c2.set(src)
		lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		job, err := ds.Table(c2.table).LoaderFrom(src).Run(lctx)
		if err != nil {
			cancel()
			t.Errorf("%s: jobs.insert: %v", c2.table, err)
			continue
		}
		inserted := job.LastStatus()
		st, err := job.Wait(lctx)
		cancel()
		if err != nil || st.Err() != nil {
			t.Errorf("%s: the load failed: %v %v", c2.table, err, st)
			continue
		}
		listed, ok := listedStatus(t, ctx, c, job.ID())
		if !ok {
			t.Errorf("%s: jobs.list does not list the job", c2.table)
		}
		for what, s := range map[string]*bigquery.JobStatus{"jobs.insert": inserted, "jobs.get": st, "jobs.list": listed} {
			if s == nil || s.Statistics == nil {
				t.Errorf("%s: %s: no statistics", c2.table, what)
				continue
			}
			ls, ok := s.Statistics.Details.(*bigquery.LoadStatistics)
			if !ok {
				t.Errorf("%s: %s: statistics %T, want LoadStatistics", c2.table, what, s.Statistics.Details)
				continue
			}
			if ls.OutputRows != c2.rows || ls.InputFiles != 1 || ls.InputFileBytes != int64(len(data)) {
				t.Errorf("%s: %s: LoadStatistics %+v, want %d rows from 1 file of %d bytes", c2.table, what, ls, c2.rows, len(data))
			}
			if len(s.Errors) != len(c2.errLine) {
				t.Errorf("%s: %s: Errors %v, want %d", c2.table, what, s.Errors, len(c2.errLine))
				continue
			}
			for i, e := range s.Errors {
				if e.Reason != "invalid" || !strings.Contains(e.Message, c2.errLine[i]) {
					t.Errorf("%s: %s: Errors[%d] = %+v, want invalid at %s", c2.table, what, i, e, c2.errLine[i])
				}
			}
		}

		got, err := svc.Jobs.Get(project, job.ID()).Context(ctx).Do()
		if err != nil {
			t.Errorf("%s: bigquery/v2 jobs.get: %v", c2.table, err)
			continue
		}
		if got.Statistics == nil || got.Statistics.Load == nil || got.Statistics.Load.BadRecords != c2.bad {
			t.Errorf("%s: bigquery/v2 jobs.get: statistics %+v, want %d bad records", c2.table, got.Statistics, c2.bad)
		}
		list, err := svc.Jobs.List(project).AllUsers(true).Context(ctx).Do()
		if err != nil {
			t.Errorf("%s: bigquery/v2 jobs.list: %v", c2.table, err)
			continue
		}
		found := false
		for _, j := range list.Jobs {
			if j.JobReference == nil || j.JobReference.JobId != job.ID() {
				continue
			}
			found = true
			if j.Statistics == nil || j.Statistics.Load == nil || j.Statistics.Load.BadRecords != c2.bad {
				t.Errorf("%s: bigquery/v2 jobs.list: statistics %+v, want %d bad records", c2.table, j.Statistics, c2.bad)
			}
		}
		if !found {
			t.Errorf("%s: bigquery/v2 jobs.list: the job is not on the first page", c2.table)
		}
		if n := countRows(t, h, ds.Table(c2.table)); n != int(c2.rows) {
			t.Errorf("%s: the table has %d rows, want %d", c2.table, n, c2.rows)
		}
	}
}
