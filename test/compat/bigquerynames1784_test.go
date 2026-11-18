//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// TestBigQueryNamesOfTheJobsProject (#1784): two projects of Resource
// Manager, through the official Go client, each make a dataset `same` with
// a table, a view, a wildcard table, a SQL function, a table function, a
// stored procedure and a model of the same names, A first. A name without
// a project (`same.t`, `same.f(0)`) is read in the job's project, and a
// name of one part in the default dataset, as BigQuery does ("If you omit
// the project, BigQuery uses the project that runs the query",
// https://cloud.google.com/bigquery/docs/reference/standard-sql/lexical#table_names):
// each project reads its own rows, runs its own function, table function,
// procedure and model, and lists only its own tables and routines in
// INFORMATION_SCHEMA, either way. B's CREATE FUNCTION of A's name succeeds
// (it was 409 Already Exists, A's routine). A view's and a routine's body
// keep their own project's names when another project reads or calls them
// by their whole name, and a third project, without the dataset, finds
// neither A's nor B's objects.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.routines.list
func TestBigQueryNamesOfTheJobsProject(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	endpoint := h.Endpoint(EnvBigQuery)
	if endpoint == "" {
		t.Skip("no BigQuery endpoint")
	}
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	ctx := h.Context()
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(rmEndpoint))
	clients := map[string]*bigquery.Client{}
	ids := map[string]string{}
	for _, p := range []string{"A", "B", "C"} {
		id := v1ID(h, "bqnames-"+strings.ToLower(p)+"-")
		ids[p] = id
		op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id}})
		if err == nil {
			_, err = waitLRO(ctx, op)
		}
		if err != nil {
			t.Fatalf("CreateProject %s: %v", id, err)
		}
		t.Cleanup(func() {
			_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id})
			_, _ = adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, id), "", nil)
		})
		c, err := bigquery.NewClient(ctx, id, option.WithEndpoint(endpoint), option.WithoutAuthentication())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		clients[p] = c
	}
	// defaultIn, when set, is the project of the default dataset `same` of
	// a query with a default dataset: the job's own when empty.
	defaultIn := ""
	query := func(p, sql string, defaultDataset bool) ([][]bigquery.Value, error) {
		q := clients[p].Query(sql)
		q.Location = "US"
		if defaultDataset {
			q.DefaultProjectID, q.DefaultDatasetID = ids[p], "same"
			if defaultIn != "" {
				q.DefaultProjectID = ids[defaultIn]
			}
		}
		it, err := q.Read(ctx)
		if err != nil {
			return nil, err
		}
		var out [][]bigquery.Value
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if err == iterator.Done {
				return out, nil
			}
			if err != nil {
				return nil, err
			}
			out = append(out, row)
		}
	}
	one := func(p, sql string, defaultDataset bool) string {
		t.Helper()
		rows, err := query(p, sql, defaultDataset)
		if err != nil {
			t.Errorf("in %s: %s: %v", p, sql, err)
			return ""
		}
		if len(rows) != 1 || len(rows[0]) != 1 {
			t.Errorf("in %s: %s = %v, want one value", p, sql, rows)
			return ""
		}
		return fmt.Sprint(rows[0][0])
	}

	// A first, then B, the same names; A's rows say A, and its model is
	// y = 2x, B's y = 3x.
	for _, p := range []string{"A", "B"} {
		if err := clients[p].Dataset("same").Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
			t.Fatalf("datasets.insert same in %s: %v", p, err)
		}
		k := map[string]string{"A": "2", "B": "3"}[p]
		for _, sql := range []string{
			fmt.Sprintf("CREATE TABLE same.t AS SELECT '%s' AS who, x, x * %s AS y FROM UNNEST([1, 2, 3]) AS x", p, k),
			fmt.Sprintf("CREATE TABLE same.s_1 AS SELECT '%s' AS who", p),
			"CREATE VIEW same.v AS SELECT who FROM same.t WHERE x = 1",
			fmt.Sprintf("CREATE FUNCTION same.f(x INT64) AS (CONCAT('%s', CAST(x AS STRING)))", p),
			"CREATE TABLE FUNCTION same.tf() AS SELECT who FROM same.t WHERE x = 2",
			"CREATE PROCEDURE same.p(OUT r STRING) BEGIN SET r = (SELECT who FROM same.t WHERE x = 3); END",
			"CREATE MODEL same.m OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, y FROM same.t",
		} {
			if _, err := query(p, sql, false); err != nil {
				t.Fatalf("in %s: %s: %v", p, sql, err)
			}
		}
	}

	for _, p := range []string{"A", "B"} {
		for _, dd := range []bool{false, true} {
			for sql, want := range map[string]string{
				"SELECT who FROM same.t WHERE x = 1":         p,
				"SELECT who FROM `same.t` WHERE x = 1":       p,
				"SELECT who FROM same.v":                     p,
				"SELECT who FROM `same.s_*`":                 p,
				"SELECT same.f(0)":                           p + "0",
				"SELECT who FROM same.tf()":                  p,
				"DECLARE r STRING; CALL same.p(r); SELECT r": p,
				"SELECT CAST(ROUND(predicted_y) AS INT64) FROM ML.PREDICT(MODEL same.m, (SELECT 10 AS x))":                 map[string]string{"A": "20", "B": "30"}[p],
				"SELECT COUNT(*) FROM same.INFORMATION_SCHEMA.TABLES WHERE table_name = 't'":                               "1",
				"SELECT STRING_AGG(table_catalog) FROM same.INFORMATION_SCHEMA.TABLES WHERE table_name = 't'":              ids[p],
				"SELECT COUNT(*) FROM same.INFORMATION_SCHEMA.ROUTINES WHERE routine_name = 'f'":                           "1",
				"SELECT STRING_AGG(routine_catalog) FROM `region-us`.INFORMATION_SCHEMA.ROUTINES WHERE routine_name = 'f'": ids[p],
				"SELECT STRING_AGG(table_catalog) FROM `region-us`.INFORMATION_SCHEMA.TABLES WHERE table_name = 't'":       ids[p],
				"CREATE TEMP FUNCTION f(x INT64) AS (x * 100); SELECT CONCAT(same.f(f(1)), '')":                            p + "100",
			} {
				if got := one(p, sql, dd); got != want {
					t.Errorf("in %s (default dataset %v): %s = %q, want %q", p, dd, sql, got, want)
				}
			}
		}
		// One part, in the default dataset.
		if got := one(p, "SELECT who FROM t WHERE x = 1", true); got != p {
			t.Errorf("in %s: SELECT who FROM t (default dataset same) = %q, want %q", p, got, p)
		}
		// routines.list lists the project's routines, each its own body.
		it := clients[p].Dataset("same").Routines(ctx)
		var names []string
		for {
			r, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("routines.list in %s: %v", p, err)
			}
			names = append(names, r.RoutineID)
		}
		sort.Strings(names)
		if strings.Join(names, ",") != "f,p,tf" {
			t.Errorf("routines.list same in %s = %v, want [f p tf]", p, names)
		}
		md, err := clients[p].Dataset("same").Routine("f").Metadata(ctx)
		if err != nil {
			t.Fatalf("routines.get same.f in %s: %v", p, err)
		}
		if !strings.Contains(md.Body, "'"+p+"'") {
			t.Errorf("routines.get same.f in %s: body %q, want the project's own", p, md.Body)
		}
	}

	// A view's and a routine's body read their own project's names.
	other := map[string]string{"A": "B", "B": "A"}
	for _, p := range []string{"A", "B"} {
		o := other[p]
		for sql, want := range map[string]string{
			fmt.Sprintf("SELECT who FROM `%s`.same.v", ids[o]):                              o,
			fmt.Sprintf("SELECT who FROM `%s.same.t` WHERE x = 1", ids[o]):                  o,
			fmt.Sprintf("SELECT `%s`.same.f(1)", ids[o]):                                    o + "1",
			fmt.Sprintf("SELECT who FROM `%s`.same.tf()", ids[o]):                           o,
			fmt.Sprintf("DECLARE r STRING; CALL `%s`.same.p(r); SELECT r", ids[o]):          o,
			fmt.Sprintf("SELECT who FROM `%s.same.s_*`", ids[o]):                            o,
			fmt.Sprintf("SELECT COUNT(*) FROM `%s`.same.INFORMATION_SCHEMA.TABLES", ids[o]): "3",
		} {
			if got := one(p, sql, false); got != want {
				t.Errorf("in %s: %s = %q, want %q", p, sql, got, want)
			}
		}
	}

	// A default dataset of the other project: a name without a project is
	// that project's ("the default datasetId and projectId to assume for
	// any unqualified table names",
	// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job#jobconfigurationquery).
	for _, p := range []string{"A", "B"} {
		o := other[p]
		defaultIn = o
		for sql, want := range map[string]string{
			"SELECT who FROM same.t WHERE x = 1":         o,
			"SELECT who FROM t WHERE x = 1":              o,
			"SELECT same.f(0)":                           o + "0",
			"SELECT who FROM same.tf()":                  o,
			"DECLARE r STRING; CALL same.p(r); SELECT r": o,
			"SELECT CAST(ROUND(predicted_y) AS INT64) FROM ML.PREDICT(MODEL same.m, (SELECT 10 AS x))": map[string]string{"A": "20", "B": "30"}[o],
		} {
			if got := one(p, sql, true); got != want {
				t.Errorf("in %s, default dataset %s.same: %s = %q, want %q", p, o, sql, got, want)
			}
		}
		defaultIn = ""
	}

	// A project without the dataset finds neither's.
	for _, sql := range []string{
		"SELECT who FROM same.t",
		"SELECT same.f(0)",
		"SELECT who FROM same.tf()",
		"SELECT who FROM same.v",
		"DECLARE r STRING; CALL same.p(r); SELECT r",
		"SELECT * FROM ML.PREDICT(MODEL same.m, (SELECT 10 AS x))",
	} {
		if rows, err := query("C", sql, false); err == nil {
			t.Errorf("in C: %s = %v, want not found", sql, rows)
		} else if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			t.Errorf("in C: %s: %v, want not found", sql, err)
		}
	}
	// And makes its own, under the same names.
	if err := clients["C"].Dataset("same").Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("datasets.insert same in C: %v", err)
	}
	for _, sql := range []string{
		"CREATE FUNCTION same.f(x INT64) AS (CONCAT('C', CAST(x AS STRING)))",
		"CREATE TABLE same.t AS SELECT 'C' AS who",
	} {
		if _, err := query("C", sql, false); err != nil {
			t.Fatalf("in C: %s: %v", sql, err)
		}
	}
	if got := one("C", "SELECT CONCAT(same.f(0), who) FROM same.t", false); got != "C0C" {
		t.Errorf("in C: same.f(0) and same.t = %q, want C0C", got)
	}
	if got := one("A", "SELECT CONCAT(same.f(0), who) FROM same.t WHERE x = 1", false); got != "A0A" {
		t.Errorf("in A after C: same.f(0) and same.t = %q, want A0A", got)
	}
}
