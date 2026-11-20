//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"cloud.google.com/go/bigquery"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// TestBigQueryVectorIndexFirstInAFreshProject (#2045): in a project that
// has never run a query, CREATE VECTOR INDEX on a table over 10 MB, the
// project's first statement after the table's, builds the index (it
// failed "dataset _cloudburrow_query_results is not found", for 300 s, as
// the index's tables are kept in that dataset, which was made only by a
// query result), through the official cloud.google.com/go/bigquery client.
// Then:
//
//   - VECTOR_INDEXES gives it ACTIVE with coverage 100;
//   - VECTOR_SEARCH, the project's first search, finds the nearest rows
//     through the index (vectorSearchStatistics FULLY_USED, read with
//     google.golang.org/api/bigquery/v2 jobs.get);
//   - datasets.list leaves the hidden dataset out, as BigQuery does a
//     dataset whose name begins with an underscore unless all=true
//     (https://cloud.google.com/bigquery/docs/datasets#dataset-naming).
//
// covers: bigquery.jobs.insert, bigquery.jobs.query, bigquery.jobs.get, bigquery.datasets.list
func TestBigQueryVectorIndexFirstInAFreshProject(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	endpoint := h.Endpoint(EnvBigQuery)
	ctx := h.Context()

	pc, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	project := v1ID(h, "bqvi-")
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: project}})
	if err != nil {
		t.Fatalf("CreateProject %s: %v", project, err)
	}
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + project})
		_, _ = adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, project), "", nil)
	})
	c, err := bigquery.NewClient(ctx, project, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("bigquery.NewClient(%s): %v", project, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	svc, err := bq.NewService(ctx, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("bigquery/v2 NewService: %v", err)
	}
	// datasets lists the project's datasets, hidden ones too with hidden.
	datasets := func(hidden bool) map[string]bool {
		t.Helper()
		it := c.Datasets(ctx)
		it.ListHidden = hidden
		out := map[string]bool{}
		for {
			ds, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatalf("datasets.list (all=%v): %v", hidden, err)
			}
			out[ds.DatasetID] = true
		}
	}

	ds := c.Dataset("vi")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	// A 12 MB table of 20 clusters, mostly a padding column.
	if err := mlRun(h, c, "CREATE TABLE `vi.big` AS SELECT id, MOD(id, 20) AS cat, REPEAT('x', 1000) AS pad, "+
		"ARRAY(SELECT MOD(id, 20) * 1.0 + 0.05 * SIN(id * 0.7 + j) FROM UNNEST(GENERATE_ARRAY(1, 8)) AS j ORDER BY j) AS e "+
		"FROM UNNEST(GENERATE_ARRAY(1, 11000)) AS id"); err != nil {
		t.Fatalf("CREATE TABLE big: %v", err)
	}
	if datasets(true)[queryResultsDataset] {
		t.Fatalf("the project's hidden results dataset is there before the index: the project is not fresh, the test proves nothing")
	}

	// The index first.
	if err := mlRun(h, c, "CREATE VECTOR INDEX big_index ON `vi.big`(e) STORING(cat) "+
		"OPTIONS(index_type = 'IVF', ivf_options = '{\"num_lists\": 20}')"); err != nil {
		t.Fatalf("CREATE VECTOR INDEX, first in a fresh project: %v", err)
	}
	rows := mlQuery(t, h, c, "SELECT index_status, coverage_percentage FROM `vi`.INFORMATION_SCHEMA.VECTOR_INDEXES WHERE table_name = 'big'")
	if len(rows) != 1 || rows[0]["index_status"] != "ACTIVE" || rows[0]["coverage_percentage"] != int64(100) {
		t.Fatalf("VECTOR_INDEXES: %v, want ACTIVE with coverage 100", rows)
	}

	// The search: the ten rows nearest id 1's embedding are of its cluster.
	job, err := c.Query("SELECT base.id AS id, base.cat AS cat, distance FROM VECTOR_SEARCH(TABLE `vi.big`, 'e', " +
		"(SELECT e FROM `vi.big` WHERE id = 1), 'e', top_k => 10)").Run(ctx)
	if err != nil {
		t.Fatalf("VECTOR_SEARCH: %v", err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatalf("VECTOR_SEARCH: %v", err)
	}
	n := 0
	for {
		row := map[string]bigquery.Value{}
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("VECTOR_SEARCH rows: %v", err)
		}
		n++
		if row["cat"] != int64(1) {
			t.Errorf("VECTOR_SEARCH row %v, want cluster 1", row)
		}
	}
	if n != 10 {
		t.Errorf("VECTOR_SEARCH: %d rows, want 10", n)
	}
	got, err := svc.Jobs.Get(project, job.ID()).Location(job.Location()).Context(ctx).Do()
	if err != nil {
		t.Fatalf("jobs.get %s: %v", job.ID(), err)
	}
	if got.Statistics == nil || got.Statistics.Query == nil || got.Statistics.Query.VectorSearchStatistics == nil ||
		got.Statistics.Query.VectorSearchStatistics.IndexUsageMode != "FULLY_USED" {
		t.Errorf("VECTOR_SEARCH's vectorSearchStatistics: %+v, want FULLY_USED", got.Statistics)
	}

	// The hidden dataset stays hidden.
	if listed := datasets(false); listed[queryResultsDataset] || !listed["vi"] {
		t.Errorf("datasets.list = %v, want vi and not %s", listed, queryResultsDataset)
	}
}
