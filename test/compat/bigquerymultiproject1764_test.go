//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// TestBigQueryServesEveryResourceManagerProject (#1764): BigQuery serves
// every ACTIVE project of the instance's Resource Manager, not the
// instance's alone, through the official Go client:
//
//   - a project Resource Manager does not have is 404 "project ... is not
//     found", the emulator's answer before;
//   - projects.list, every ACTIVE project of Resource Manager, numbered;
//   - in a project Resource Manager has, datasets, tables and jobs of its
//     own, listed apart from the instance project's;
//   - a query job of the instance's project reading `other.ds.t`, the job's
//     project the job's and the data's the other's;
//   - copy jobs between the projects both ways, and a snapshot of the other
//     project's table in the instance's project;
//   - ALTER TABLE ... ADD FOREIGN KEY REFERENCES other.ds.t;
//   - `region-us`.INFORMATION_SCHEMA.SCHEMATA of a job of the other project
//     lists that project's datasets;
//   - deleted, purged and created again, the project has no datasets.
//
// covers: bigquery.datasets.insert, bigquery.datasets.list, bigquery.tables.insert, bigquery.jobs.insert, bigquery.jobs.list, bigquery.projects.list
func TestBigQueryServesEveryResourceManagerProject(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	c, home := bigqueryClient(t, h)
	ctx := h.Context()
	endpoint := h.Endpoint(EnvBigQuery)
	clientOf := func(project string) *bigquery.Client {
		t.Helper()
		oc, err := bigquery.NewClient(ctx, project, option.WithEndpoint(endpoint), option.WithoutAuthentication())
		if err != nil {
			t.Fatalf("bigquery.NewClient(%s): %v", project, err)
		}
		t.Cleanup(func() { _ = oc.Close() })
		return oc
	}

	// A project Resource Manager does not have: BigQuery's 404.
	nowhere := v1ID(h, "bqnone-")
	err := clientOf(nowhere).Dataset("ds").Create(ctx, &bigquery.DatasetMetadata{})
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusNotFound || !strings.Contains(ge.Message, "project "+nowhere+" is not found") {
		t.Fatalf("datasets.insert in %s, which Resource Manager does not have = %v, want 404 project %s is not found", nowhere, err, nowhere)
	}

	pc, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	other := v1ID(h, "bqmp-")
	parent := "projects/" + other
	create := func() {
		t.Helper()
		op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: other}})
		if err != nil {
			t.Fatalf("CreateProject %s: %v", other, err)
		}
		if _, err := waitLRO(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	create()
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: parent})
		_, _ = adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, other), "", nil)
	})
	oc := clientOf(other)

	// projects.list: every ACTIVE project of Resource Manager, with its
	// number.
	got, err := pc.GetProject(ctx, &rmpb.GetProjectRequest{Name: parent})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := bigqueryService(t, h).Projects.List().MaxResults(1000).Context(ctx).Do()
	if err != nil {
		t.Fatalf("projects.list: %v", err)
	}
	seen := map[string]string{}
	for _, p := range listed.Projects {
		seen[p.Id] = strconv.FormatUint(p.NumericId, 10)
	}
	if want := strings.TrimPrefix(got.GetName(), "projects/"); seen[other] != want || seen[home] == "" || seen[nowhere] != "" {
		t.Errorf("projects.list = %v, want %s numbered %s, and %s, not %s", seen, other, want, home, nowhere)
	}

	// The other project's own dataset, table, rows and job.
	ods := oc.Dataset("mp")
	if err := ods.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("datasets.insert in %s: %v", other, err)
	}
	schema := bigquery.Schema{{Name: "x", Type: bigquery.IntegerFieldType}, {Name: "s", Type: bigquery.StringFieldType}}
	if err := ods.Table("t").Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatalf("tables.insert in %s: %v", other, err)
	}
	run := func(cl *bigquery.Client, sql string) *bigquery.Job {
		t.Helper()
		q := cl.Query(sql)
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
		return j
	}
	one := func(cl *bigquery.Client, sql string) int64 {
		t.Helper()
		it, err := run(cl, sql).Read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		n, _ := row[0].(int64)
		return n
	}
	ownJob := run(oc, "INSERT INTO mp.t (x, s) VALUES (1, 'a'), (2, 'b'), (3, 'c')")
	if ownJob.ProjectID() != other {
		t.Errorf("a job of %s's client is in %s", other, ownJob.ProjectID())
	}

	// Listed apart: the other project's datasets are not the instance's.
	names := func(cl *bigquery.Client) map[string]bool {
		t.Helper()
		out := map[string]bool{}
		it := cl.Datasets(ctx)
		for {
			d, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatalf("datasets.list: %v", err)
			}
			out[d.DatasetID] = true
		}
	}
	homeDS := c.Dataset(strings.ReplaceAll("mp_"+h.Project(), "-", "_"))
	if err := homeDS.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("datasets.insert in %s: %v", home, err)
	}
	t.Cleanup(func() { _ = homeDS.DeleteWithContents(context.Background()) })
	if got := names(oc); !got["mp"] || got[homeDS.DatasetID] {
		t.Errorf("datasets.list of %s = %v, want mp and not %s", other, got, homeDS.DatasetID)
	}
	if got := names(c); got["mp"] && !got[homeDS.DatasetID] {
		t.Errorf("datasets.list of %s = %v", home, got)
	}
	jobs := func(cl *bigquery.Client) map[string]bool {
		t.Helper()
		out := map[string]bool{}
		it := cl.Jobs(ctx)
		it.AllUsers = true
		for {
			j, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatalf("jobs.list: %v", err)
			}
			out[j.ID()] = true
		}
	}

	// A job of the instance's project reads the other project's table.
	q := "SELECT SUM(x) FROM `" + other + ".mp.t`"
	crossJob := run(c, q)
	if crossJob.ProjectID() != home {
		t.Errorf("the cross-project query's job is in %s, want the job's project %s", crossJob.ProjectID(), home)
	}
	if n := one(c, q); n != 6 {
		t.Errorf("%s = %d, want 6", q, n)
	}
	if got := jobs(oc); !got[ownJob.ID()] || got[crossJob.ID()] {
		t.Errorf("jobs.list of %s has its own job %v, the instance project's %v; want only its own",
			other, got[ownJob.ID()], got[crossJob.ID()])
	}

	// Copy jobs, both ways, run in the instance's project.
	copyJob := func(dst, src *bigquery.Table, op bigquery.TableCopyOperationType) {
		t.Helper()
		cp := dst.CopierFrom(src)
		cp.OperationType = op
		cp.Location = "US"
		j, err := cp.Run(ctx)
		if err != nil {
			t.Fatalf("copy %s.%s.%s into %s.%s.%s: %v", src.ProjectID, src.DatasetID, src.TableID, dst.ProjectID, dst.DatasetID, dst.TableID, err)
		}
		if j.ProjectID() != home {
			t.Errorf("the copy job is in %s, want %s", j.ProjectID(), home)
		}
		st, err := j.Wait(ctx)
		if err == nil {
			err = st.Err()
		}
		if err != nil {
			t.Fatalf("copy %s.%s into %s.%s: %v", src.ProjectID, src.TableID, dst.ProjectID, dst.TableID, err)
		}
	}
	copyJob(homeDS.Table("in"), ods.Table("t"), bigquery.CopyOperation)
	if n := one(c, "SELECT COUNT(*) FROM `"+homeDS.DatasetID+".in`"); n != 3 {
		t.Errorf("the copy into %s has %d rows, want 3", home, n)
	}
	// The other project's table, from the instance's: c.DatasetInProject.
	back := c.DatasetInProject(other, "mp").Table("back")
	copyJob(back, homeDS.Table("in"), bigquery.CopyOperation)
	if n := one(oc, "SELECT COUNT(*) FROM mp.back"); n != 3 {
		t.Errorf("the copy into %s has %d rows, want 3", other, n)
	}
	copyJob(homeDS.Table("snap"), c.DatasetInProject(other, "mp").Table("t"), bigquery.SnapshotOperation)
	md, err := homeDS.Table("snap").Metadata(ctx)
	if err != nil {
		t.Fatalf("tables.get of the snapshot: %v", err)
	}
	if md.Type != bigquery.Snapshot || md.SnapshotDefinition == nil || md.SnapshotDefinition.BaseTableReference == nil ||
		md.SnapshotDefinition.BaseTableReference.ProjectID != other {
		t.Errorf("the snapshot of %s.mp.t: type %s, definition %+v", other, md.Type, md.SnapshotDefinition)
	}
	copyJob(homeDS.Table("cl"), c.DatasetInProject(other, "mp").Table("t"), bigquery.CloneOperation)
	if n := one(c, "SELECT COUNT(*) FROM `"+homeDS.DatasetID+".cl`"); n != 3 {
		t.Errorf("the clone of %s.mp.t has %d rows, want 3", other, n)
	}

	// A foreign key to the other project's table.
	run(c, "CREATE TABLE `"+homeDS.DatasetID+".child` (k INT64)")
	run(c, "ALTER TABLE `"+homeDS.DatasetID+".child` ADD CONSTRAINT fk FOREIGN KEY (k) REFERENCES `"+other+".mp.t`(x) NOT ENFORCED")
	md, err = homeDS.Table("child").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tc := md.TableConstraints; tc == nil || len(tc.ForeignKeys) != 1 || tc.ForeignKeys[0].ReferencedTable == nil ||
		tc.ForeignKeys[0].ReferencedTable.ProjectID != other || tc.ForeignKeys[0].ReferencedTable.TableID != "t" {
		t.Errorf("the foreign key to %s.mp.t: %+v", other, md.TableConstraints)
	}
	// A missing table of the other project is 404, as one of the job's.
	q2 := c.Query("ALTER TABLE `" + homeDS.DatasetID + ".child` ADD CONSTRAINT fk2 FOREIGN KEY (k) REFERENCES `" + other + ".mp.nope`(x) NOT ENFORCED")
	q2.Location = "US"
	j2, err := q2.Run(ctx)
	if err == nil {
		var st *bigquery.JobStatus
		if st, err = j2.Wait(ctx); err == nil {
			err = st.Err()
		}
	}
	if err == nil || !strings.Contains(err.Error(), "Not found: Table "+other+":mp.nope") {
		t.Errorf("a foreign key to a missing table of %s = %v, want Not found: Table %s:mp.nope", other, err, other)
	}

	// The other project's region views.
	it, err := run(oc, "SELECT schema_name FROM `region-us`.INFORMATION_SCHEMA.SCHEMATA").Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	schemata := map[string]bool{}
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		s, _ := row[0].(string)
		schemata[s] = true
	}
	if !schemata["mp"] || schemata[homeDS.DatasetID] {
		t.Errorf("%s's region-us SCHEMATA = %v, want mp and not %s", other, schemata, homeDS.DatasetID)
	}

	// The console's BigQuery page browses the project chosen in the
	// toolbar (UI parity): its datasets, and a dataset's tables.
	if strings.TrimSpace(os.Getenv(EnvConsole)) != "" {
		addr := consoleAddr(t, h)
		var list consoleListing
		consoleJSON(t, addr, http.MethodGet, "/api/resources/bigquery?project="+url.QueryEscape(other), "", &list)
		listed := map[string]bool{}
		for _, it := range list.Items {
			listed[it.Name] = true
		}
		if list.Prompt != "" || list.Unavailable != "" || !listed["mp"] || listed[homeDS.DatasetID] {
			t.Errorf("the console's datasets of %s = %+v, want mp", other, list)
		}
		var page consoleDetail
		consoleJSON(t, addr, http.MethodGet, "/api/detail/bigquery?project="+url.QueryEscape(other)+"&name=mp", "", &page)
		tables := map[string]bool{}
		for _, sec := range page.Sections {
			for _, it := range sec.Listing.Items {
				tables[it.Name] = true
			}
		}
		if page.Unavailable != "" || page.Prompt != "" || !tables["t"] || !tables["back"] {
			t.Errorf("the console's page of %s's dataset mp = %+v, want tables t and back", other, page)
		}
	}

	// Deleted, purged and created again: none of the old datasets.
	if _, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if code, body := adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, other), "", nil); code != http.StatusOK {
		t.Fatalf("POST /admin/purge = %d %s", code, body)
	}
	create()
	if got := names(oc); len(got) != 0 {
		t.Errorf("a project created again under a purged ID lists the old project's datasets %v", got)
	}
}
