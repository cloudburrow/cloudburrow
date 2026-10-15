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
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
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

// TestBigQueryRowAccessPoliciesAreRefused records that the front refuses
// every rowAccessPolicies method: the emulator has no row-level security
// and answered each 500, which the Go client retries.
//
// covers: bigquery.rowAccessPolicies.insert (unimplemented), bigquery.rowAccessPolicies.list (unimplemented), bigquery.rowAccessPolicies.get (unimplemented), bigquery.rowAccessPolicies.update (unimplemented)
// covers: bigquery.rowAccessPolicies.delete (unimplemented), bigquery.rowAccessPolicies.batchDelete (unimplemented), bigquery.rowAccessPolicies.getIamPolicy (unimplemented), bigquery.rowAccessPolicies.testIamPermissions (unimplemented)
func TestBigQueryRowAccessPoliciesAreRefused(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ds, tbl := seedOrders(t, h, c)
	ctx := h.Context()
	d, tb := ds.DatasetID, tbl.TableID
	pol := &bq.RowAccessPolicy{
		RowAccessPolicyReference: &bq.RowAccessPolicyReference{ProjectId: project, DatasetId: d, TableId: tb, PolicyId: "eu"},
		FilterPredicate:          "region = 'eu'",
	}
	resource := "projects/" + project + "/datasets/" + d + "/tables/" + tb + "/rowAccessPolicies/eu"

	_, err := svc.RowAccessPolicies.Insert(project, d, tb, pol).Context(ctx).Do()
	wantNotImplemented(t, "bigquery.rowAccessPolicies.insert", err)
	_, err = svc.RowAccessPolicies.List(project, d, tb).Context(ctx).Do()
	wantNotImplemented(t, "bigquery.rowAccessPolicies.list", err)
	_, err = svc.RowAccessPolicies.Get(project, d, tb, "eu").Context(ctx).Do()
	wantNotImplemented(t, "bigquery.rowAccessPolicies.get", err)
	_, err = svc.RowAccessPolicies.Update(project, d, tb, "eu", pol).Context(ctx).Do()
	wantNotImplemented(t, "bigquery.rowAccessPolicies.update", err)
	err = svc.RowAccessPolicies.Delete(project, d, tb, "eu").Context(ctx).Do()
	wantNotImplemented(t, "bigquery.rowAccessPolicies.delete", err)
	err = svc.RowAccessPolicies.BatchDelete(project, d, tb, &bq.BatchDeleteRowAccessPoliciesRequest{PolicyIds: []string{"eu"}}).Context(ctx).Do()
	wantNotImplemented(t, "bigquery.rowAccessPolicies.batchDelete", err)
	_, err = svc.RowAccessPolicies.GetIamPolicy(resource, &bq.GetIamPolicyRequest{}).Context(ctx).Do()
	wantNotImplemented(t, "bigquery.rowAccessPolicies.getIamPolicy", err)
	_, err = svc.RowAccessPolicies.TestIamPermissions(resource, &bq.TestIamPermissionsRequest{
		Permissions: []string{"bigquery.rowAccessPolicies.getFilteredData"},
	}).Context(ctx).Do()
	wantNotImplemented(t, "bigquery.rowAccessPolicies.testIamPermissions", err)
}

// TestBigQueryTableAndRoutineIAMIsRefused records that the front refuses
// the IAM methods of tables and routines: the emulator answered a table's
// 500 and a routine's 405.
//
// covers: bigquery.tables.getIamPolicy (unimplemented), bigquery.tables.setIamPolicy (unimplemented), bigquery.tables.testIamPermissions (unimplemented)
// covers: bigquery.routines.getIamPolicy (unimplemented), bigquery.routines.setIamPolicy (unimplemented), bigquery.routines.testIamPermissions (unimplemented)
func TestBigQueryTableAndRoutineIAMIsRefused(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ds, tbl := seedOrders(t, h, c)
	ctx := h.Context()

	// Tables through the Go client's IAM handle.
	handle := tbl.IAM()
	_, err := handle.Policy(ctx)
	wantNotImplemented(t, "bigquery.tables.getIamPolicy", err)
	err = handle.SetPolicy(ctx, &iam.Policy{InternalProto: &iampb.Policy{}})
	wantNotImplemented(t, "bigquery.tables.setIamPolicy", err)
	_, err = handle.TestPermissions(ctx, []string{"bigquery.tables.get"})
	wantNotImplemented(t, "bigquery.tables.testIamPermissions", err)

	if _, err := c.Query("CREATE FUNCTION `" + project + "." + ds.DatasetID + ".plus_one`(x INT64) AS (x + 1)").Read(ctx); err != nil {
		t.Fatalf("CREATE FUNCTION: %v", err)
	}
	resource := "projects/" + project + "/datasets/" + ds.DatasetID + "/routines/plus_one"
	_, err = svc.Routines.GetIamPolicy(resource, &bq.GetIamPolicyRequest{}).Context(ctx).Do()
	wantNotImplemented(t, "bigquery.routines.getIamPolicy", err)
	_, err = svc.Routines.SetIamPolicy(resource, &bq.SetIamPolicyRequest{Policy: &bq.Policy{}}).Context(ctx).Do()
	wantNotImplemented(t, "bigquery.routines.setIamPolicy", err)
	_, err = svc.Routines.TestIamPermissions(resource, &bq.TestIamPermissionsRequest{Permissions: []string{"bigquery.routines.get"}}).Context(ctx).Do()
	wantNotImplemented(t, "bigquery.routines.testIamPermissions", err)
}

// TestBigQueryRoutineUpdate (#1238): Routine.Update (routines.update)
// replaces a function CREATE FUNCTION made. The new body, description and
// return type read back, a query calls the new body, and the creation time is
// kept. A stale etag is 412 and leaves the routine as it was; an update that
// gives only a body, which the client's comment says routines.update does
// not take as partial, is 400 and leaves it as it was too.
//
// covers: bigquery.routines.update
func TestBigQueryRoutineUpdate(t *testing.T) {
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
	full := &bigquery.RoutineMetadataToUpdate{
		Type: "SCALAR_FUNCTION", Language: "SQL", Body: "x + 2", Description: "plus two",
		Arguments:  []*bigquery.RoutineArgument{{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}}},
		ReturnType: &bigquery.StandardSQLDataType{TypeKind: "INT64"},
	}
	got, err := r.Update(ctx, full, md.ETag)
	if err != nil {
		t.Fatalf("Routine.Update: %v", err)
	}
	if got.Body != "x + 2" || got.ETag == md.ETag {
		t.Errorf("Routine.Update = %+v", got)
	}
	after, err := r.Metadata(ctx)
	if err != nil || after.Body != "x + 2" || after.Description != "plus two" || after.ReturnType == nil ||
		after.ReturnType.TypeKind != "INT64" || !after.CreationTime.Equal(md.CreationTime) {
		t.Errorf("after the update: %+v, %v (created %v)", after, err, md.CreationTime)
	}
	var row []bigquery.Value
	it, err := c.Query("SELECT `" + project + "." + ds.DatasetID + ".plus_one`(1)").Read(ctx)
	if err != nil || it.Next(&row) != nil || len(row) != 1 || row[0] != int64(3) {
		t.Errorf("calling the updated function = %v, %v; want 3", row, err)
	}

	stale := *full
	stale.Body = "x + 3"
	var e *googleapi.Error
	if _, err := r.Update(ctx, &stale, md.ETag); !errors.As(err, &e) || e.Code != http.StatusPreconditionFailed {
		t.Errorf("Routine.Update with a stale etag = %v; want 412", err)
	}
	if _, err := r.Update(ctx, &bigquery.RoutineMetadataToUpdate{Body: "x + 4"}, ""); !errors.As(err, &e) || e.Code != http.StatusBadRequest {
		t.Errorf("Routine.Update with only a body = %v; want 400", err)
	}
	if md, err := r.Metadata(ctx); err != nil || md.Body != "x + 2" {
		t.Errorf("after the refusals: %+v, %v; want body %q", md, err, "x + 2")
	}
}
