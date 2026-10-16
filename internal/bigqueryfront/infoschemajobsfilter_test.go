package bigqueryfront

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// testJobs are n jobs j0..j(n-1), made a millisecond apart from t0:
// every tenth a child of j0, every third a LOAD.
func testJobs(n int, t0 int64) []map[string]any {
	jobs := make([]map[string]any, n)
	for i := range jobs {
		stats := map[string]any{"creationTime": strconv.FormatInt(t0+int64(i), 10)}
		if i%10 == 9 {
			stats["parentJobId"] = "j0"
		}
		conf := map[string]any{"query": map[string]any{"query": "SELECT " + strconv.Itoa(i)}}
		if i%3 == 2 {
			conf = map[string]any{"load": map[string]any{}}
		}
		jobs[i] = map[string]any{"jobReference": map[string]any{"jobId": "j" + strconv.Itoa(i), "projectId": "p"},
			"configuration": conf, "statistics": stats, "status": map[string]any{"state": "DONE"}}
	}
	return jobs
}

func keptIDs(jobs []map[string]any) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, keysOf("p", j).id)
	}
	return out
}

// TestJobsFilterPushesDownWhatIsSafe: #1369.
func TestJobsFilterPushesDownWhatIsSafe(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	jobs := testJobs(30, t0)
	now := time.UnixMilli(t0 + 29)
	from := "FROM `region-us`.INFORMATION_SCHEMA.JOBS"
	for _, c := range []struct {
		sql  string
		want string // the jobs kept; "all" when nothing is pushed down
	}{
		{"SELECT * " + from + " WHERE job_id = 'j3'", "[j3]"},
		{"SELECT * " + from + " WHERE 'j3' = job_id", "[j3]"},
		{"SELECT * " + from + " j WHERE j.job_id IN ('j3', \"j5\") AND state = 'DONE'", "[j3 j5]"},
		{"SELECT * " + from + " AS j WHERE (j.job_id = 'j4') AND j.parent_job_id IS NULL", "[j4]"},
		{"SELECT * " + from + " WHERE parent_job_id = 'j0' AND job_type = 'QUERY'", "[j9 j19]"},
		{"SELECT * " + from + " WHERE parent_job_id IS NOT NULL AND job_type = 'LOAD'", "[j29]"},
		{"SELECT * FROM `p`.`region-us`.INFORMATION_SCHEMA.JOBS_BY_PROJECT WHERE JOBS_BY_PROJECT.job_id = 'j1'", "[j1]"},
		{"SELECT * " + from + " WHERE project_id = 'other'", "[]"},
		// creation_time: literals exactly, CURRENT_TIMESTAMP a minute early.
		{"SELECT * " + from + " WHERE creation_time >= TIMESTAMP '2026-10-15 12:00:00.027' AND state = 'DONE'", "[j27 j28 j29]"},
		{"SELECT * " + from + " WHERE creation_time > '2026-10-15T12:00:00.027Z'", "[j28 j29]"},
		{"SELECT * " + from + " WHERE creation_time BETWEEN TIMESTAMP '2026-10-15 12:00:00.001 UTC' AND TIMESTAMP '2026-10-15 12:00:00.002' AND job_id != 'x'", "[j1 j2]"},
		{"SELECT * " + from + " WHERE creation_time < '2026-10-15 12:00:00.001+00:00'", "[j0]"},
		{"SELECT * " + from + " WHERE creation_time > TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 DAY)", "all"},
		// ORDER BY creation_time LIMIT, when the WHERE is all read.
		{"SELECT job_id " + from + " WHERE state = 'DONE' ORDER BY creation_time DESC LIMIT 2", "[j28 j29]"},
		{"SELECT job_id, creation_time " + from + " ORDER BY creation_time LIMIT 2 OFFSET 1", "[j0 j1 j2]"},
		{"(SELECT * " + from + " ORDER BY creation_time DESC LIMIT 1)", "[j29]"},
		// Nothing pushed down where it could change the answer.
		{"SELECT * " + from + " WHERE job_id = 'j3' OR job_id = 'j4'", "all"},
		{"SELECT * " + from + " WHERE job_id NOT IN ('j3')", "all"},
		{"SELECT * " + from + " WHERE job_id = 'j\\x33'", "all"},
		{"SELECT * " + from + " WHERE job_id = r'j3'", "all"},
		{"SELECT * " + from + " j, UNNEST(j.labels) WHERE job_id = 'j3'", "all"},
		{"SELECT * " + from + " j JOIN t ON TRUE WHERE j.job_id = 'j3'", "all"},
		{"SELECT * " + from + " j WHERE other.job_id = 'j3'", "all"},
		{"SELECT * FROM t WHERE x IN (SELECT job_id " + from + ") AND job_id = 'j3'", "all"},
		{"SELECT COUNT(*) " + from + " ORDER BY creation_time LIMIT 1", "all"},
		{"SELECT DISTINCT state " + from + " ORDER BY creation_time LIMIT 1", "all"},
		{"SELECT end_time AS creation_time " + from + " ORDER BY creation_time LIMIT 1", "all"},
		{"SELECT * " + from + " WHERE query LIKE '%x%' ORDER BY creation_time LIMIT 1", "all"},
		{"SELECT * " + from + " WHERE creation_time > TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 DAY) ORDER BY creation_time LIMIT 1", "all"},
		{"SELECT * " + from + " ORDER BY creation_time, job_id LIMIT 1", "all"},
		{"SELECT a.job_id FROM `region-us`.INFORMATION_SCHEMA.JOBS a, `region-us`.INFORMATION_SCHEMA.JOBS b WHERE a.job_id = 'j3'", "all"},
	} {
		f := pushedJobsFilter(c.sql, informationSchemaJobs(c.sql), now)
		got := keptIDs(f.apply("p", jobs))
		want := c.want
		if want == "all" {
			want = fmt.Sprint(keptIDs(jobs))
		}
		if fmt.Sprint(got) != want {
			t.Errorf("%s: kept %v, want %s", c.sql, got, want)
		}
	}
	// The creation-time bounds reach jobs.list, in milliseconds, wider.
	sql := "SELECT * " + from + " WHERE creation_time > TIMESTAMP '2026-10-15 12:00:00.0275' AND creation_time <= '2026-10-15 12:00:00.029'"
	q := map[string][]string{}
	pushedJobsFilter(sql, informationSchemaJobs(sql), now).listBounds(q)
	if q["minCreationTime"][0] != strconv.FormatInt(t0+27, 10) || q["maxCreationTime"][0] != strconv.FormatInt(t0+29, 10) {
		t.Errorf("list bounds %v", q)
	}
}

// TestJobsLimitKeepsTies: #1369.
func TestJobsLimitKeepsTies(t *testing.T) {
	jobs := testJobs(5, 1000)
	jobs[3]["statistics"].(map[string]any)["creationTime"] = "1004"
	delete(jobs[0]["statistics"].(map[string]any), "creationTime")
	f := &jobsFilter{limit: 1, desc: true}
	if got := fmt.Sprint(keptIDs(f.apply("p", jobs))); got != "[j3 j4]" {
		t.Errorf("DESC LIMIT 1 kept %s", got)
	}
	f = &jobsFilter{limit: 1}
	if got := fmt.Sprint(keptIDs(f.apply("p", jobs))); got != "[j0]" {
		t.Errorf("ASC LIMIT 1 kept %s (NULL first)", got)
	}
}

// BenchmarkJobsRows5000 is the front's work for a query of JOBS over
// 5,000 jobs: every row (no filter) against WHERE job_id = (#1369).
func BenchmarkJobsRows5000(b *testing.B) {
	jobs := testJobs(5000, 1792085926120)
	for _, c := range []struct{ name, sql string }{
		{"all", "SELECT COUNT(*) FROM `region-us`.INFORMATION_SCHEMA.JOBS"},
		{"job_id", "SELECT * FROM `region-us`.INFORMATION_SCHEMA.JOBS WHERE job_id = 'j4000'"},
	} {
		b.Run(c.name, func(b *testing.B) {
			var size int
			for i := 0; i < b.N; i++ {
				f := pushedJobsFilter(c.sql, informationSchemaJobs(c.sql), time.Now())
				kept := f.apply("p", jobs)
				rows := make([][]string, len(kept))
				for j, job := range kept {
					rows[j] = jobRow("p", job)
				}
				size = len(rowsSubquery(jobsColumns, rows))
			}
			b.ReportMetric(float64(size), "sql-bytes")
		})
	}
}
