//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// TestBigQueryPurgedProjectStartsEmpty (#1781): a project of Resource
// Manager gets, through the official Go clients, a dataset with a table, a
// view, a SQL function, a table function, a stored procedure, a routine
// routines.insert made, a model and a row access policy, a second
// dataset, and query jobs. Deleted and undeleted it keeps them; deleted,
// purged (POST /admin/purge) and created again under the same ID, it has
// none: its datasets are gone, the jobs are 404, and a dataset made again
// under the old name has no routine, model or row access policy, so
// CREATE FUNCTION of the old name succeeds (it was 409 Already Exists,
// the purged project's routine).
//
// A project's purge removes "all of its resources" for good
// (https://cloud.google.com/resource-manager/docs/creating-managing-projects#shutting_down_projects).
//
// covers: bigquery.routines.list, bigquery.routines.get, bigquery.routines.insert, bigquery.models.get, bigquery.rowAccessPolicies.list, bigquery.jobs.get, bigquery.jobs.list
func TestBigQueryPurgedProjectStartsEmpty(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	endpoint := h.Endpoint(EnvBigQuery)
	if endpoint == "" {
		t.Skip("no BigQuery endpoint")
	}
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	ctx := h.Context()
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(rmEndpoint))
	id := v1ID(h, "bqpurge-")
	parent := "projects/" + id
	create := func() {
		t.Helper()
		op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id}})
		if err == nil {
			_, err = waitLRO(ctx, op)
		}
		if err != nil {
			t.Fatalf("CreateProject %s: %v", id, err)
		}
	}
	purge := func() {
		t.Helper()
		if code, body := adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, id), "", nil); code != http.StatusOK {
			t.Fatalf("POST /admin/purge = %d %s", code, body)
		}
	}
	create()
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: parent})
		_, _ = adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, id), "", nil)
	})
	c, err := bigquery.NewClient(ctx, id, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	svc, err := bq.NewService(ctx, option.WithEndpoint(strings.TrimRight(endpoint, "/")+"/bigquery/v2/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	run := func(sql string) (string, error) {
		q := c.Query(sql)
		q.Location = "US"
		j, err := q.Run(ctx)
		if err != nil {
			return "", err
		}
		st, err := j.Wait(ctx)
		if err != nil {
			return j.ID(), err
		}
		return j.ID(), st.Err()
	}
	must := func(sql string) string {
		t.Helper()
		id, err := run(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	is404 := func(err error) bool {
		var e *googleapi.Error
		return errors.As(err, &e) && e.Code == http.StatusNotFound
	}

	for _, d := range []string{"data", "more"} {
		if err := c.Dataset(d).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
			t.Fatalf("datasets.insert %s in %s: %v", d, id, err)
		}
	}
	var jobs []string
	for _, sql := range []string{
		"CREATE TABLE data.t AS SELECT 1 AS x, 2.0 AS y UNION ALL SELECT 2, 4.0",
		"CREATE VIEW data.v AS SELECT x FROM data.t",
		"CREATE FUNCTION data.f(x INT64) AS (x + 1)",
		"CREATE TABLE FUNCTION data.tf(n INT64) AS SELECT x FROM data.t WHERE x > n",
		"CREATE PROCEDURE data.p() BEGIN SELECT 1; END",
		"CREATE MODEL data.m OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, y FROM data.t",
		"CREATE ROW ACCESS POLICY rp ON data.t GRANT TO ('allUsers') FILTER USING (x > 0)",
		"CREATE TABLE more.t AS SELECT 1 AS x",
		"SELECT data.f(1)",
	} {
		jobs = append(jobs, must(sql))
	}
	if err := c.Dataset("data").Routine("ins").Create(ctx, &bigquery.RoutineMetadata{Type: "SCALAR_FUNCTION", Language: "SQL",
		Body: "x * 2", Arguments: []*bigquery.RoutineArgument{{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}}}}); err != nil {
		t.Fatalf("routines.insert data.ins: %v", err)
	}

	routines := func() []string {
		t.Helper()
		var out []string
		it := c.Dataset("data").Routines(ctx)
		for {
			r, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("routines.list data: %v", err)
			}
			out = append(out, r.RoutineID)
		}
		return out
	}
	policies := func() int {
		t.Helper()
		got, err := svc.RowAccessPolicies.List(id, "data", "t").Context(ctx).Do()
		if err != nil {
			t.Fatalf("rowAccessPolicies.list data.t: %v", err)
		}
		return len(got.RowAccessPolicies)
	}
	// What the project holds, by kind, read back.
	held := func() []string {
		t.Helper()
		var out []string
		for _, d := range []string{"data", "more"} {
			if _, err := c.Dataset(d).Metadata(ctx); err == nil {
				out = append(out, "dataset "+d)
			} else if !is404(err) {
				t.Fatalf("datasets.get %s: %v", d, err)
			}
		}
		for _, j := range jobs {
			if _, err := c.JobFromIDLocation(ctx, j, "US"); err == nil {
				out = append(out, "job "+j)
			} else if !is404(err) {
				t.Fatalf("jobs.get %s: %v", j, err)
			}
		}
		if len(out) == 0 || out[0] != "dataset data" {
			return out
		}
		for _, r := range routines() {
			out = append(out, "routine "+r)
		}
		if _, err := c.Dataset("data").Model("m").Metadata(ctx); err == nil {
			out = append(out, "model m")
		} else if !is404(err) {
			t.Fatalf("models.get m: %v", err)
		}
		if _, err := c.Dataset("data").Table("t").Metadata(ctx); err == nil {
			out = append(out, "table t")
			if n := policies(); n > 0 {
				out = append(out, fmt.Sprintf("%d row access policies", n))
			}
		}
		return out
	}
	all := held()
	for _, want := range []string{"dataset data", "dataset more", "routine f", "routine tf", "routine p", "routine ins", "model m", "table t", "1 row access policies"} {
		if !strings.Contains(strings.Join(all, ","), want) {
			t.Fatalf("set up %v, want %s among them", all, want)
		}
	}

	// Deleted and undeleted: kept.
	if _, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := pc.UndeleteProject(ctx, &rmpb.UndeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("UndeleteProject: %v", err)
	}
	if got := held(); strings.Join(got, ",") != strings.Join(all, ",") {
		t.Errorf("after undelete %v, want %v", got, all)
	}

	// Deleted, purged and made again: nothing.
	if _, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	purge()
	create()
	if got := held(); len(got) != 0 {
		t.Errorf("the project made again under a purged ID holds %v", got)
	}
	it := c.Jobs(ctx)
	it.AllUsers = true
	for {
		j, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("jobs.list: %v", err)
		}
		t.Errorf("jobs.list of the project made again names %s", j.ID())
	}
	if err := c.Dataset("data").Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("datasets.insert data again: %v", err)
	}
	if got := held(); len(got) != 1 {
		t.Errorf("the dataset made again under its old name holds %v, want nothing", got[1:])
	}
	must("CREATE TABLE data.t AS SELECT 1 AS x")
	if n := policies(); n != 0 {
		t.Errorf("the table made again under its old name has %d row access policies, want none", n)
	}
	// CREATE, not CREATE OR REPLACE: the purged project's function is gone.
	must("CREATE FUNCTION data.f(x INT64) AS (x + 10)")
	must("CREATE TABLE FUNCTION data.tf(n INT64) AS SELECT n AS x")
	must("CREATE PROCEDURE data.p() BEGIN SELECT 2; END")
	must("CREATE MODEL data.m OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT 1 AS x, 2.0 AS y UNION ALL SELECT 2, 3.0")
	q := c.Query("SELECT data.f(1)")
	q.Location = "US"
	rows, err := q.Read(ctx)
	if err != nil {
		t.Fatalf("SELECT data.f(1): %v", err)
	}
	var row []bigquery.Value
	if err := rows.Next(&row); err != nil {
		t.Fatalf("SELECT data.f(1): %v", err)
	}
	if row[0] != int64(11) {
		t.Errorf("data.f(1) = %v, want 11 (the new function's)", row[0])
	}
}
