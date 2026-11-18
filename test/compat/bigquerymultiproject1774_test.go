//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// otherProject1774 is another ACTIVE project of Resource Manager, with a
// dataset "mr" in US, for the tests below: the instance's client and
// project, the other's client and ID, and the Projects client.
func otherProject1774(t *testing.T, h *Harness, prefix string) (*bigquery.Client, string, *bigquery.Client, string, *resourcemanager.ProjectsClient) {
	t.Helper()
	control := h.Endpoint(EnvControl)
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	endpoint := h.Endpoint(EnvBigQuery)
	if endpoint == "" {
		t.Skip("no BigQuery endpoint")
	}
	c, home := bigqueryClient(t, h)
	ctx := h.Context()
	pc, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	other := v1ID(h, prefix)
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: other}})
	if err == nil {
		_, err = waitLRO(ctx, op)
	}
	if err != nil {
		t.Fatalf("CreateProject %s: %v", other, err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + other})
		_, _ = adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, other), "", nil)
	})
	oc, err := bigquery.NewClient(ctx, other, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = oc.Close() })
	if err := oc.Dataset("mr").Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("datasets.insert in %s: %v", other, err)
	}
	return c, home, oc, other, pc
}

// waitJob1774 waits for j, which must be the job project's, and returns
// its status.
func waitJob1774(t *testing.T, ctx context.Context, home, what string, j *bigquery.Job, err error) *bigquery.JobStatus {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if j.ProjectID() != home {
		t.Errorf("%s: the job is in %s, want the job's project %s", what, j.ProjectID(), home)
	}
	st, err := j.Wait(ctx)
	if err == nil {
		err = st.Err()
	}
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return st
}

// TestBigQueryParquetAndORCLoadsAcrossProjects (#1774): through the
// official Go client, a job of the instance's project loads a Parquet
// upload, then appends it again, and an ORC upload, into new tables of
// another ACTIVE project of Resource Manager: each table is made in that
// project, with the file's schema and rows, and not in the job's.
//
// covers: bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryParquetAndORCLoadsAcrossProjects(t *testing.T) {
	h := New(t)
	c, home, oc, other, _ := otherProject1774(t, h, "bqpo-")
	ctx := h.Context()
	load := func(what string, tbl *bigquery.Table, src bigquery.LoadSource, write bigquery.TableWriteDisposition) {
		t.Helper()
		l := tbl.LoaderFrom(src)
		l.Location = "US"
		l.WriteDisposition = write
		j, err := l.Run(ctx)
		waitJob1774(t, ctx, home, what, j, err)
	}
	check := func(table, wantSchema, sql, want string) {
		t.Helper()
		md, err := oc.Dataset("mr").Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("tables.get %s.mr.%s: %v", other, table, err)
		}
		var cols []string
		for _, f := range md.Schema {
			cols = append(cols, f.Name+" "+string(f.Type))
		}
		if got := strings.Join(cols, ", "); got != wantSchema {
			t.Errorf("%s.mr.%s schema = %s, want %s", other, table, got, wantSchema)
		}
		if got := fmt.Sprint(rows(t, ctx, oc, sql)); got != want {
			t.Errorf("%s = %s, want %s", sql, got, want)
		}
		if _, err := c.Dataset("mr").Table(table).Metadata(ctx); err == nil {
			t.Errorf("the load made %s.mr.%s in the job's project too", home, table)
		}
	}

	pq := c.DatasetInProject(other, "mr").Table("pq")
	load("Parquet load into "+other+".mr.pq", pq, parquetUpload(parquetFixture(t, "ab.parquet"), nil), bigquery.WriteEmpty)
	load("Parquet WRITE_APPEND into "+other+".mr.pq", pq, parquetUpload(parquetFixture(t, "ab.parquet"), nil), bigquery.WriteAppend)
	check("pq", "a INTEGER, b STRING", "SELECT a, COUNT(*) FROM mr.pq GROUP BY a ORDER BY a", "[[1 2] [2 2] [3 2]]")

	data := orcFile(t, "struct<id:bigint,name:string>", false, []any{int64(1), "x"}, []any{int64(2), "y"})
	src := bigquery.NewReaderSource(strings.NewReader(data))
	src.SourceFormat = bigquery.ORC
	load("ORC load into "+other+".mr.orc", c.DatasetInProject(other, "mr").Table("orc"), src, bigquery.WriteEmpty)
	check("orc", "id INTEGER, name STRING", "SELECT id, name FROM mr.orc ORDER BY id", "[[1 x] [2 y]]")
}

// TestBigQueryCloneAsOfAndShutDownAcrossProjects (#1774): through the
// official Go client, in a job of the instance's project,
//
//   - CREATE TABLE ... CLONE of another project's table FOR SYSTEM_TIME AS
//     OF a past time (between two loads) copies the rows it had then, the
//     clone's cloneDefinition naming the other project's table and that
//     time;
//   - once the other project is DELETE_REQUESTED in Resource Manager, a
//     query of the instance's project naming its table, by jobs.insert and
//     by jobs.query, is 403 accessDenied naming that project, as its own
//     requests are; once restored the query runs again.
//
// covers: bigquery.jobs.insert, bigquery.jobs.query, bigquery.tables.get
func TestBigQueryCloneAsOfAndShutDownAcrossProjects(t *testing.T) {
	h := New(t)
	c, home, oc, other, pc := otherProject1774(t, h, "bqca-")
	ctx := h.Context()
	homeDS := c.Dataset(strings.ReplaceAll("ca_"+h.Project(), "-", "_"))
	if err := homeDS.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("datasets.insert in %s: %v", home, err)
	}
	t.Cleanup(func() { _ = homeDS.DeleteWithContents(context.Background()) })
	run := func(sql string) *bigquery.JobStatus {
		t.Helper()
		q := c.Query(sql)
		q.Location = "US"
		j, err := q.Run(ctx)
		return waitJob1774(t, ctx, home, sql, j, err)
	}
	// Two loads of the other project's table, by queries of its own.
	ownRun := func(sql string) *bigquery.JobStatus {
		t.Helper()
		q := oc.Query(sql)
		q.Location = "US"
		j, err := q.Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		st, err := j.Wait(ctx)
		if err == nil {
			err = st.Err()
		}
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return st
	}
	first := ownRun("CREATE TABLE mr.t AS SELECT 1 AS x UNION ALL SELECT 2")
	time.Sleep(2 * time.Second)
	second := ownRun("INSERT INTO mr.t (x) VALUES (30), (40)")
	// Between the two, by the instance's clock.
	before := (first.Statistics.EndTime.UnixMilli() + second.Statistics.StartTime.UnixMilli()) / 2

	run(fmt.Sprintf("CREATE TABLE `%s.%s.then` CLONE `%s.mr.t` FOR SYSTEM_TIME AS OF TIMESTAMP_MILLIS(%d)",
		home, homeDS.DatasetID, other, before))
	if got := fmt.Sprint(rows(t, ctx, c, "SELECT x FROM `"+homeDS.DatasetID+".then` ORDER BY x")); got != "[[1] [2]]" {
		t.Errorf("the clone of %s.mr.t as of %d = %s, want [[1] [2]]", other, before, got)
	}
	md, err := homeDS.Table("then").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cd := md.CloneDefinition; cd == nil || cd.BaseTableReference == nil || cd.BaseTableReference.ProjectID != other ||
		cd.BaseTableReference.TableID != "t" || cd.CloneTime.UnixMilli() != before {
		t.Errorf("the clone's definition = %+v, want of %s.mr.t at %d", cd, other, before)
	}
	if got := fmt.Sprint(rows(t, ctx, oc, "SELECT COUNT(*) FROM mr.t")); got != "[[4]]" {
		t.Errorf("%s.mr.t has %s rows after the clone, want 4", other, got)
	}

	// Shut down: a query naming its table is refused.
	sql := "SELECT SUM(x) FROM `" + other + ".mr.t`"
	if got := fmt.Sprint(rows(t, ctx, c, sql)); got != "[[73]]" {
		t.Fatalf("%s = %s, want [[73]]", sql, got)
	}
	parent := "projects/" + other
	if _, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("DeleteProject %s: %v", other, err)
	}
	refused := func(err error) bool {
		var ge *googleapi.Error
		return errors.As(err, &ge) && ge.Code == http.StatusForbidden && len(ge.Errors) > 0 &&
			ge.Errors[0].Reason == "accessDenied" && strings.Contains(ge.Message, "Project "+other)
	}
	insert := func() error { // jobs.insert
		q := c.Query(sql)
		q.Location = "US"
		_, err := q.Run(ctx)
		return err
	}
	query := func() error { // jobs.query
		q := c.Query(sql)
		q.Location = "US"
		_, err := q.Read(ctx)
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for err := insert(); !refused(err); err = insert() {
		if time.Now().After(deadline) {
			t.Fatalf("jobs.insert of %q, %s DELETE_REQUESTED = %v, want 403 accessDenied naming it", sql, other, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := query(); !refused(err) {
		t.Errorf("jobs.query of %q, %s DELETE_REQUESTED = %v, want 403 accessDenied naming it", sql, other, err)
	}
	uop, err := pc.UndeleteProject(ctx, &rmpb.UndeleteProjectRequest{Name: parent})
	if err == nil {
		_, err = waitLRO(ctx, uop)
	}
	if err != nil {
		t.Fatalf("UndeleteProject %s: %v", other, err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for err := query(); err != nil; err = query() {
		if time.Now().After(deadline) {
			t.Fatalf("jobs.query of %q, %s restored = %v, want it to run", sql, other, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
