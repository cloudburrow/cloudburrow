//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// The BigQuery REST methods docs/coverage listed as Unknown (#1200): each is
// driven here through the official Go client, or the official generated
// client where the Go client has no call for it, and either verified or
// shown refused.

func bigqueryService(t *testing.T, h *Harness) *bq.Service {
	t.Helper()
	svc, err := bq.NewService(h.Context(), option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("bigquery.NewService: %v", err)
	}
	return svc
}

// wantNotImplemented fails unless err is the front's 501 naming method.
func wantNotImplemented(t *testing.T, method string, err error) {
	t.Helper()
	var e *googleapi.Error
	if !errors.As(err, &e) || e.Code != http.StatusNotImplemented || !strings.Contains(e.Message, "Not implemented here: "+method+".") {
		t.Errorf("%s returned %v, want 501 \"Not implemented here: %s.\"", method, err, method)
	}
}

// TestBigQueryGetQueryResultsPages reads a query's rows back through
// jobs.getQueryResults, a page at a time, and through the Go client's
// Job.Read of a query job.
//
// covers: bigquery.jobs.getQueryResults
func TestBigQueryGetQueryResultsPages(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ctx := h.Context()

	const sql = "SELECT x FROM UNNEST([1, 2, 3]) AS x ORDER BY x"
	q, err := svc.Jobs.Query(project, &bq.QueryRequest{Query: sql, UseLegacySql: googleapi.Bool(false)}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("jobs.query: %v", err)
	}
	var got []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 3 {
			t.Fatalf("jobs.getQueryResults gave more than 3 pages of one row")
		}
		call := svc.Jobs.GetQueryResults(project, q.JobReference.JobId).MaxResults(1).Context(ctx)
		if token != "" {
			call = call.PageToken(token)
		}
		res, err := call.Do()
		if err != nil {
			t.Fatalf("jobs.getQueryResults: %v", err)
		}
		if !res.JobComplete || res.TotalRows != 3 {
			t.Fatalf("jobs.getQueryResults: jobComplete %v, totalRows %d, want true and 3", res.JobComplete, res.TotalRows)
		}
		if len(res.Schema.Fields) != 1 || res.Schema.Fields[0].Name != "x" {
			t.Fatalf("jobs.getQueryResults schema = %+v, want one field x", res.Schema.Fields)
		}
		for _, row := range res.Rows {
			got = append(got, row.F[0].V.(string))
		}
		if token = res.PageToken; token == "" {
			break
		}
	}
	if strings.Join(got, ",") != "1,2,3" {
		t.Errorf("jobs.getQueryResults rows = %v, want 1,2,3", got)
	}

	// The Go client reads a query job's rows with jobs.getQueryResults.
	job, err := c.Query(sql).Run(ctx)
	if err != nil {
		t.Fatalf("Query.Run: %v", err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatalf("Job.Read: %v", err)
	}
	got = nil
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("Job.Read: %v", err)
		}
		got = append(got, fmt.Sprint(row[0]))
	}
	if strings.Join(got, ",") != "1,2,3" {
		t.Errorf("Job.Read rows = %v, want 1,2,3", got)
	}
}

// TestBigQueryProjectsListNamesTheInstancesProject lists the projects: the
// emulator serves one, the instance's.
//
// covers: bigquery.projects.list
func TestBigQueryProjectsListNamesTheInstancesProject(t *testing.T) {
	h := New(t)
	_, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)

	res, err := svc.Projects.List().Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("projects.list: %v", err)
	}
	if len(res.Projects) != 1 || res.Projects[0].Id != project {
		t.Fatalf("projects.list = %+v, want the one project %s", res.Projects, project)
	}
}

// TestBigQueryModelsOfADatasetWithNone lists, reads, patches and deletes
// models in a dataset that has none, the only kind the emulator has: it
// makes no models (CREATE MODEL is "Statement not supported", and the API
// has no models.insert). The list is empty, and a model it names is 404.
//
// covers: bigquery.models.list, bigquery.models.get, bigquery.models.patch, bigquery.models.delete
func TestBigQueryModelsOfADatasetWithNone(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds, _ := seedOrders(t, h, c)
	ctx := h.Context()

	it := ds.Models(ctx)
	if m, err := it.Next(); err != iterator.Done {
		t.Errorf("Dataset.Models: first = %v, %v; want iterator.Done", m, err)
	}
	m := ds.Model("absent")
	_, err := m.Metadata(ctx)
	wantHTTPStatus(t, "Model.Metadata (models.get)", err, http.StatusNotFound)
	_, err = m.Update(ctx, bigquery.ModelMetadataToUpdate{Description: "x"}, "")
	wantHTTPStatus(t, "Model.Update (models.patch)", err, http.StatusNotFound)
	wantHTTPStatus(t, "Model.Delete (models.delete)", m.Delete(ctx), http.StatusNotFound)
}

// TestBigQueryDatasetUpdatesAreSaved drives datasets.patch through the Go
// client's Dataset.Update (which sends only the fields set, labels to delete
// as null, and the etag as If-Match), and datasets.update through the
// generated client, and reads each change back with datasets.get (#1205).
// The emulator answered both 200 and kept nothing (#1200); bigquery-emulator
// patch 0010 and the front (datasetupdate.go) now save them.
//
// covers: bigquery.datasets.patch, bigquery.datasets.update, bigquery.datasets.undelete (unimplemented)
func TestBigQueryDatasetUpdatesAreSaved(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ctx := h.Context()
	ds := c.Dataset("compat_dsupdate")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Description: "compat", Labels: map[string]string{"a": "1", "b": "2"}}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = ds.Delete(h.Context()) })

	before, err := ds.Metadata(ctx)
	if err != nil {
		t.Fatalf("Dataset.Metadata: %v", err)
	}
	if before.ETag == "" {
		t.Fatalf("Dataset.Metadata gave no etag")
	}

	// A patch of the fields set only (a field mask), under the dataset's etag.
	dm := bigquery.DatasetMetadataToUpdate{
		Name:                       "Friendly",
		DefaultTableExpiration:     2 * time.Hour,
		DefaultPartitionExpiration: 3 * time.Hour,
		DefaultCollation:           "und:ci",
		MaxTimeTravel:              72 * time.Hour,
		Access:                     []*bigquery.AccessEntry{{Role: bigquery.ReaderRole, EntityType: bigquery.SpecialGroupEntity, Entity: "projectReaders"}},
	}
	dm.SetLabel("c", "3")
	dm.DeleteLabel("b")
	got, err := ds.Update(ctx, dm, before.ETag)
	if err != nil {
		t.Fatalf("Dataset.Update (datasets.patch): %v", err)
	}
	md, err := ds.Metadata(ctx)
	if err != nil {
		t.Fatalf("Dataset.Metadata: %v", err)
	}
	if md.Name != "Friendly" || md.Description != "compat" || md.DefaultTableExpiration != 2*time.Hour ||
		md.DefaultPartitionExpiration != 3*time.Hour || md.DefaultCollation != "und:ci" || md.MaxTimeTravel != 72*time.Hour ||
		md.Labels["a"] != "1" || md.Labels["c"] != "3" || len(md.Labels) != 2 || len(md.Access) != 1 {
		t.Errorf("after the patch, Metadata = %+v (labels %v, access %d)", md, md.Labels, len(md.Access))
	}
	if md.ETag == before.ETag || md.ETag != got.ETag {
		t.Errorf("etag after the patch = %q (answered %q, before %q)", md.ETag, got.ETag, before.ETag)
	}

	// The old etag no longer matches: 412, and nothing changed.
	_, err = ds.Update(ctx, bigquery.DatasetMetadataToUpdate{Description: "stale"}, before.ETag)
	wantHTTPStatus(t, "Dataset.Update with a stale etag", err, http.StatusPreconditionFailed)
	if md2, err := ds.Metadata(ctx); err != nil || md2.Description != "compat" {
		t.Errorf("after the refused patch: %v, description %q", err, md2.Description)
	}

	// An update replaces the dataset: what it leaves out is cleared.
	_, err = svc.Datasets.Update(project, ds.DatasetID, &bq.Dataset{
		DatasetReference: &bq.DatasetReference{ProjectId: project, DatasetId: ds.DatasetID}, Description: "updated",
	}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("datasets.update: %v", err)
	}
	md, err = ds.Metadata(ctx)
	if err != nil {
		t.Fatalf("Dataset.Metadata: %v", err)
	}
	if md.Description != "updated" || md.Name != "" || len(md.Labels) != 0 || md.DefaultTableExpiration != 0 {
		t.Errorf("after the update, Metadata = %+v", md)
	}

	_, err = svc.Datasets.Undelete(project, ds.DatasetID+"_gone", &bq.UndeleteDatasetRequest{}).Context(ctx).Do()
	wantNotImplemented(t, "bigquery.datasets.undelete", err)
}

// TestBigQueryServiceAccount reads the project's BigQuery service agent
// through the generated client (#1205): the emulator answered 200 with no
// email (#1200).
//
// covers: bigquery.projects.getServiceAccount
func TestBigQueryServiceAccount(t *testing.T) {
	h := New(t)
	_, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)

	got, err := svc.Projects.GetServiceAccount(project).Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("projects.getServiceAccount: %v", err)
	}
	if got.Kind != "bigquery#getServiceAccountResponse" || !strings.HasPrefix(got.Email, "bq-") ||
		!strings.HasSuffix(got.Email, "@bigquery-encryption.iam.gserviceaccount.com") {
		t.Errorf("projects.getServiceAccount = %+v", got)
	}
}

// TestBigQueryRowAccessPolicies makes, reads and drops row access policies
// through the generated client and DDL, and reads a table with them (#1218).
// CloudBurrow takes every caller to be its credentials' account
// (docs/compatibility.md), so a policy granted to it filters the caller's
// rows, and one granted to someone else alone leaves none. The two IAM
// methods of a policy are TestBigQueryRowAccessPolicyIAMIsItsGrantees's.
//
// covers: bigquery.rowAccessPolicies.insert, bigquery.rowAccessPolicies.list, bigquery.rowAccessPolicies.get, bigquery.rowAccessPolicies.update
// covers: bigquery.rowAccessPolicies.delete, bigquery.rowAccessPolicies.batchDelete
func TestBigQueryRowAccessPolicies(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ds, tbl := seedOrders(t, h, c)
	ctx := h.Context()
	d, tb := ds.DatasetID, tbl.TableID
	me := "serviceAccount:cloudburrow-local@" + project + ".iam.gserviceaccount.com"
	pol := func(id, filter string, grantees ...string) *bq.RowAccessPolicy {
		return &bq.RowAccessPolicy{
			RowAccessPolicyReference: &bq.RowAccessPolicyReference{ProjectId: project, DatasetId: d, TableId: tb, PolicyId: id},
			FilterPredicate:          filter,
			Grantees:                 grantees,
		}
	}
	ids := func() []int64 {
		t.Helper()
		it, err := c.Query("SELECT id FROM `" + project + "." + d + "." + tb + "` ORDER BY id").Read(ctx)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		var got []int64
		for {
			var row struct {
				ID int64 `bigquery:"id"`
			}
			err := it.Next(&row)
			if err == iterator.Done {
				return got
			}
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			got = append(got, row.ID)
		}
	}

	// Someone else's policy alone: the caller reads no rows.
	if _, err := svc.RowAccessPolicies.Insert(project, d, tb, pol("us", "region = 'us'", "user:someone@example.com")).Context(ctx).Do(); err != nil {
		t.Fatalf("rowAccessPolicies.insert: %v", err)
	}
	if got := ids(); len(got) != 0 {
		t.Errorf("rows with no policy for the caller = %v, want none", got)
	}
	// The caller's policy: its rows.
	if _, err := svc.RowAccessPolicies.Insert(project, d, tb, pol("eu", "region = 'nowhere'", me)).Context(ctx).Do(); err != nil {
		t.Fatalf("rowAccessPolicies.insert: %v", err)
	}
	if _, err := svc.RowAccessPolicies.Insert(project, d, tb, pol("eu", "TRUE", me)).Context(ctx).Do(); !isCode(err, http.StatusConflict) {
		t.Errorf("insert of an existing policy = %v, want 409", err)
	}
	if _, err := svc.RowAccessPolicies.Update(project, d, tb, "eu", pol("eu", "region = 'eu'", me)).Context(ctx).Do(); err != nil {
		t.Fatalf("rowAccessPolicies.update: %v", err)
	}
	if got := ids(); fmt.Sprint(got) != "[1 2]" {
		t.Errorf("rows = %v, want [1 2]", got)
	}
	got, err := svc.RowAccessPolicies.Get(project, d, tb, "eu").Context(ctx).Do()
	if err != nil || got.FilterPredicate != "region = 'eu'" || fmt.Sprint(got.Grantees) != "["+me+"]" || got.CreationTime == "" || got.Etag == "" {
		t.Errorf("rowAccessPolicies.get = %+v, %v", got, err)
	}
	list, err := svc.RowAccessPolicies.List(project, d, tb).Context(ctx).Do()
	if err != nil || len(list.RowAccessPolicies) != 2 {
		t.Errorf("rowAccessPolicies.list = %+v, %v", list, err)
	}
	if _, err := svc.RowAccessPolicies.Insert(project, d, tb, pol("bad", "nope = 1", me)).Context(ctx).Do(); !isCode(err, http.StatusBadRequest) {
		t.Errorf("insert with a filter on no column = %v, want 400", err)
	}

	// Reads the front cannot filter are refused, not answered unfiltered.
	if _, err := svc.Tabledata.List(project, d, tb).Context(ctx).Do(); !isCode(err, http.StatusNotImplemented) {
		t.Errorf("tabledata.list of a protected table = %v, want 501", err)
	}
	if _, err := c.Query("DELETE FROM `" + d + "." + tb + "` WHERE TRUE").Read(ctx); err == nil {
		t.Error("DELETE of a protected table ran")
	}
	if _, err := tbl.ExtractorTo(bigquery.NewGCSReference("gs://nowhere-1218/x.csv")).Run(ctx); !isCode(err, http.StatusNotImplemented) {
		t.Errorf("extract job of a protected table = %v, want 501", err)
	}
	if _, err := ds.Table("copy").CopierFrom(tbl).Run(ctx); !isCode(err, http.StatusNotImplemented) {
		t.Errorf("copy job of a protected table = %v, want 501", err)
	}
	sc, _ := bigqueryClient(t, h)
	if err := sc.EnableStorageReadClient(ctx,
		option.WithEndpoint(h.Endpoint(EnvBigQueryStorage)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))); err != nil {
		t.Fatalf("EnableStorageReadClient: %v", err)
	}
	var row []bigquery.Value
	if err := sc.Dataset(d).Table(tb).Read(ctx).Next(&row); status.Code(err) != codes.Unimplemented {
		t.Errorf("Storage Read of a protected table = %v, want UNIMPLEMENTED", err)
	}

	// DDL: a policy granted to everyone, then dropped.
	if _, err := c.Query("CREATE ROW ACCESS POLICY big ON `" + d + "." + tb + "` GRANT TO ('allUsers') FILTER USING (amount > 6)").Read(ctx); err != nil {
		t.Fatalf("CREATE ROW ACCESS POLICY: %v", err)
	}
	if got := ids(); fmt.Sprint(got) != "[1 2 3]" {
		t.Errorf("rows with two policies = %v, want [1 2 3]", got)
	}
	if _, err := c.Query("DROP ROW ACCESS POLICY big ON `" + d + "." + tb + "`").Read(ctx); err != nil {
		t.Fatalf("DROP ROW ACCESS POLICY: %v", err)
	}
	if err := svc.RowAccessPolicies.Delete(project, d, tb, "us").Context(ctx).Do(); err != nil {
		t.Fatalf("rowAccessPolicies.delete: %v", err)
	}
	if err := svc.RowAccessPolicies.BatchDelete(project, d, tb, &bq.BatchDeleteRowAccessPoliciesRequest{PolicyIds: []string{"eu"}}).Context(ctx).Do(); err != nil {
		t.Fatalf("rowAccessPolicies.batchDelete: %v", err)
	}
	if _, err := svc.RowAccessPolicies.Get(project, d, tb, "eu").Context(ctx).Do(); !isCode(err, http.StatusNotFound) {
		t.Errorf("get of a deleted policy = %v, want 404", err)
	}
	if got := ids(); fmt.Sprint(got) != "[1 2 3 4]" {
		t.Errorf("rows with no policies = %v, want all four", got)
	}
	if _, err := c.Query("CREATE ROW ACCESS POLICY x ON `" + d + "." + tb + "` GRANT TO ('allUsers') FILTER USING (TRUE)").Read(ctx); err != nil {
		t.Fatalf("CREATE ROW ACCESS POLICY: %v", err)
	}
	if _, err := c.Query("DROP ALL ROW ACCESS POLICIES ON `" + d + "." + tb + "`").Read(ctx); err != nil {
		t.Fatalf("DROP ALL ROW ACCESS POLICIES: %v", err)
	}
	if list, err := svc.RowAccessPolicies.List(project, d, tb).Context(ctx).Do(); err != nil || len(list.RowAccessPolicies) != 0 {
		t.Errorf("list after DROP ALL = %+v, %v", list, err)
	}

}

// isCode reports whether err is a googleapi error of code.
func isCode(err error, code int) bool {
	var e *googleapi.Error
	return errors.As(err, &e) && e.Code == code
}

// TestBigQueryRoutineUpdateIsRefused records that the front refuses
// routines.update, which the emulator does not have; the routine is
// unchanged.
//
// covers: bigquery.routines.update (unimplemented)
func TestBigQueryRoutineUpdateIsRefused(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds, _ := seedOrders(t, h, c)
	ctx := h.Context()

	if _, err := c.Query("CREATE FUNCTION `" + project + "." + ds.DatasetID + ".plus_one`(x INT64) AS (x + 1)").Read(ctx); err != nil {
		t.Fatalf("CREATE FUNCTION: %v", err)
	}
	r := ds.Routine("plus_one")
	md, err := r.Metadata(ctx)
	if err != nil {
		t.Fatalf("Routine.Metadata: %v", err)
	}
	_, err = r.Update(ctx, &bigquery.RoutineMetadataToUpdate{Body: "x + 2"}, md.ETag)
	wantNotImplemented(t, "routines.update", err)
	if md, err = r.Metadata(ctx); err != nil || md.Body != "x + 1" {
		t.Errorf("after the refusal: body %q, %v; want %q", md.Body, err, "x + 1")
	}
}
