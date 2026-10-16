//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// jobsAtScale is how many jobs TestBigQueryInformationSchemaJobsAtScale
// makes the project hold: #1419's measure.
const jobsAtScale = 5000

// TestBigQueryInformationSchemaJobsAtScale (#1419): with 5,000 jobs in the
// project, a query of `region-us`.INFORMATION_SCHEMA.JOBS with no filter
// the front pushes down (an unfiltered COUNT(*), an aggregate by state and
// job_type) answers in well under a second, and counts every job; the
// first such query after the jobs were made, in under 2 s. Measured on one
// instance (--services bigquery,storage, arm64), the first COUNT(*) after
// the 5,000 jobs took 25.1 s before #1419 (16.6 s of jobs.get to list them,
// 8.5 s to write their rows), 0.41 s after; the ones after it 0.24 to
// 0.27 s before, 0.28 to 0.36 s after.
//
// covers: bigquery.jobs.query
func TestBigQueryInformationSchemaJobsAtScale(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	// Making the jobs outlasts the harness's 60-second context.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	count := func() (int64, time.Duration) {
		t.Helper()
		start := time.Now()
		rows := readScaleRows(t, ctx, c, "SELECT COUNT(*) FROM `region-us`.INFORMATION_SCHEMA.JOBS")
		return rows[0][0].(int64), time.Since(start)
	}
	have, first := count()
	t.Logf("%d jobs before: COUNT(*) %v", have, first)
	if need := jobsAtScale - int(have); need > 0 {
		var wg sync.WaitGroup
		errs := make(chan error, need)
		next := make(chan int)
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range next {
					it, err := c.Query(fmt.Sprintf("SELECT %d AS n", i)).Read(ctx)
					if err == nil {
						var row []bigquery.Value
						err = it.Next(&row)
					}
					if err != nil {
						errs <- err
					}
				}
			}()
		}
		made := time.Now()
		for i := range need {
			next <- i
		}
		close(next)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		t.Logf("made %d jobs in %v", need, time.Since(made))
	}

	n, took := count()
	t.Logf("COUNT(*) of %d jobs, the first since: %v", n, took)
	if n < jobsAtScale {
		t.Fatalf("COUNT(*) gave %d, want at least %d", n, jobsAtScale)
	}
	if took > 2*time.Second {
		t.Errorf("the first COUNT(*) since the jobs were made took %v, want under 2s", took)
	}
	for i := range 3 {
		m, took := count()
		t.Logf("COUNT(*) again (%d): %d in %v", i, m, took)
		if m <= n {
			t.Errorf("COUNT(*) gave %d after %d: the previous query is a job too", m, n)
		}
		n = m
		if took > time.Second {
			t.Errorf("COUNT(*) of %d jobs took %v, want under 1s", m, took)
		}
	}
	start := time.Now()
	rows := readScaleRows(t, ctx, c, "SELECT state, job_type, COUNT(*) AS n, MAX(creation_time) AS last "+
		"FROM `region-us`.INFORMATION_SCHEMA.JOBS GROUP BY state, job_type ORDER BY n DESC")
	took = time.Since(start)
	var total int64
	for _, r := range rows {
		total += r[2].(int64)
	}
	t.Logf("GROUP BY state, job_type over %d jobs: %d groups in %v", total, len(rows), took)
	if total <= n {
		t.Errorf("the aggregate counted %d jobs, want more than %d", total, n)
	}
	if took > time.Second {
		t.Errorf("the aggregate took %v, want under 1s", took)
	}
}

// readScaleRows reads every row of sql through jobs.query.
func readScaleRows(t *testing.T, ctx context.Context, c *bigquery.Client, sql string) [][]bigquery.Value {
	t.Helper()
	qctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	it, err := c.Query(sql).Read(qctx)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var rows [][]bigquery.Value
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			return rows
		}
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		rows = append(rows, row)
	}
}
