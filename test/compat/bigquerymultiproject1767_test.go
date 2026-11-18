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
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// TestBigQueryAcrossProjectsLoadsAndPartitionCopies (#1767): through the
// official Go client, jobs of the instance's project write the tables of
// another ACTIVE project of Resource Manager, and a project shut down is
// refused:
//
//   - an Avro load (upload) into a new table of the other project, then
//     WRITE_APPEND into it, the job the instance project's;
//   - a copy of the other project's table as it was (table@time);
//   - a copy of one partition (`$20260101`) of the other project's
//     day-partitioned table into the instance's project, and of the whole
//     partitioned table back into the other project, partitioning kept;
//   - a CSV load into the other project's table, and into a partition of
//     it, and a query's result into a partition of it;
//   - CREATE TABLE ... CLONE of the other project's partitioned table, and
//     CREATE SNAPSHOT TABLE in the other project of the instance's table;
//   - the other project, DELETE_REQUESTED in Resource Manager, is 403
//     accessDenied, and served again once undeleted.
//
// covers: bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryAcrossProjectsLoadsAndPartitionCopies(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	c, home := bigqueryClient(t, h)
	ctx := h.Context()
	endpoint := h.Endpoint(EnvBigQuery)
	if endpoint == "" {
		t.Skip("no BigQuery endpoint")
	}
	pc, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	other := v1ID(h, "bqmr-")
	parent := "projects/" + other
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: other}})
	if err != nil {
		t.Fatalf("CreateProject %s: %v", other, err)
	}
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: parent})
		_, _ = adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, other), "", nil)
	})
	oc, err := bigquery.NewClient(ctx, other, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = oc.Close() })
	ods := oc.Dataset("mr")
	if err := ods.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("datasets.insert in %s: %v", other, err)
	}
	homeDS := c.Dataset(strings.ReplaceAll("mr_"+h.Project(), "-", "_"))
	if err := homeDS.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("datasets.insert in %s: %v", home, err)
	}
	t.Cleanup(func() { _ = homeDS.DeleteWithContents(context.Background()) })

	wait := func(what string, j *bigquery.Job, err error) *bigquery.JobStatus {
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
	count := func(cl *bigquery.Client, sql string) int64 {
		t.Helper()
		q := cl.Query(sql)
		q.Location = "US"
		it, err := q.Read(ctx)
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

	// An Avro load into the other project's new table, then appended to.
	schema := `{"type": "record", "name": "Row", "fields": [{"name": "id", "type": "long"}, {"name": "s", "type": "string"}]}`
	data := avroFile(t, schema, "", map[string]any{"id": int64(1), "s": "a"}, map[string]any{"id": int64(2), "s": "b"})
	loaded := c.DatasetInProject(other, "mr").Table("avro")
	l := loaded.LoaderFrom(avroUpload(data))
	l.Location = "US"
	j, err := l.Run(ctx)
	first := wait("Avro load into "+other+".mr.avro", j, err)
	time.Sleep(2 * time.Second)
	l = loaded.LoaderFrom(avroUpload(data))
	l.Location = "US"
	l.WriteDisposition = bigquery.WriteAppend
	j, err = l.Run(ctx)
	second := wait("Avro WRITE_APPEND into "+other+".mr.avro", j, err)
	// The table of two rows, between the loads, by the instance's clock
	// (which another test may have moved).
	before := (first.Statistics.EndTime.UnixMilli() + second.Statistics.StartTime.UnixMilli()) / 2
	if n := count(oc, "SELECT COUNT(*) FROM mr.avro"); n != 4 {
		t.Errorf("%s.mr.avro has %d rows after two loads, want 4", other, n)
	}
	if _, err := homeDS.Table("avro").Metadata(ctx); err == nil {
		t.Errorf("the load made %s.%s.avro in the job's project too", home, homeDS.DatasetID)
	}

	// The other project's table as it was (table@time), copied into the
	// instance's project.
	cp := homeDS.Table("then").CopierFrom(c.DatasetInProject(other, "mr").Table(fmt.Sprintf("avro@%d", before)))
	cp.Location = "US"
	j, err = cp.Run(ctx)
	wait("copy of "+other+".mr.avro@time", j, err)
	if n := count(c, "SELECT COUNT(*) FROM `"+homeDS.DatasetID+".then`"); n != 2 {
		t.Errorf("the copy of %s.mr.avro@%d has %d rows, want 2", other, before, n)
	}

	// Partitioned copies across the projects.
	pt := ods.Table("pt")
	if err := pt.Create(ctx, &bigquery.TableMetadata{
		Schema:           bigquery.Schema{{Name: "d", Type: bigquery.DateFieldType}, {Name: "x", Type: bigquery.IntegerFieldType}},
		TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d"},
	}); err != nil {
		t.Fatalf("tables.insert %s.mr.pt: %v", other, err)
	}
	ins := oc.Query("INSERT INTO mr.pt (d, x) VALUES ('2026-01-01', 1), ('2026-01-01', 2), ('2026-01-02', 3)")
	ins.Location = "US"
	ij, err := ins.Run(ctx)
	if err == nil {
		_, err = ij.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("INSERT INTO %s.mr.pt: %v", other, err)
	}
	cp = homeDS.Table("day").CopierFrom(c.DatasetInProject(other, "mr").Table("pt$20260101"))
	cp.Location = "US"
	// The destination exists, partitioned the same, before a partition is
	// copied into it.
	if err := homeDS.Table("day").Create(ctx, &bigquery.TableMetadata{
		Schema:           bigquery.Schema{{Name: "d", Type: bigquery.DateFieldType}, {Name: "x", Type: bigquery.IntegerFieldType}},
		TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d"},
	}); err != nil {
		t.Fatal(err)
	}
	j, err = cp.Run(ctx)
	wait("copy of "+other+".mr.pt$20260101", j, err)
	if n := count(c, "SELECT COUNT(*) FROM `"+homeDS.DatasetID+".day`"); n != 2 {
		t.Errorf("the copy of partition 20260101 has %d rows, want 2", n)
	}
	cp = c.DatasetInProject(other, "mr").Table("back").CopierFrom(homeDS.Table("day"))
	cp.Location = "US"
	j, err = cp.Run(ctx)
	wait("copy of the partitioned "+homeDS.DatasetID+".day into "+other, j, err)
	md, err := ods.Table("back").Metadata(ctx)
	if err != nil {
		t.Fatalf("tables.get %s.mr.back: %v", other, err)
	}
	if md.TimePartitioning == nil || md.TimePartitioning.Field != "d" || md.NumRows != 2 {
		t.Errorf("%s.mr.back: partitioning %+v, %d rows; want by d, 2 rows", other, md.TimePartitioning, md.NumRows)
	}

	// A CSV load, and a CSV load into a partition, of the other project.
	csv := func(data string) *bigquery.ReaderSource {
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.SourceFormat = bigquery.CSV
		src.Schema = bigquery.Schema{{Name: "d", Type: bigquery.DateFieldType}, {Name: "x", Type: bigquery.IntegerFieldType}}
		return src
	}
	l = c.DatasetInProject(other, "mr").Table("csv").LoaderFrom(csv("2026-01-05,7\n2026-01-06,8\n"))
	l.Location = "US"
	j, err = l.Run(ctx)
	wait("CSV load into "+other+".mr.csv", j, err)
	if n := count(oc, "SELECT SUM(x) FROM mr.csv"); n != 15 {
		t.Errorf("%s.mr.csv sums to %d, want 15", other, n)
	}
	l = c.DatasetInProject(other, "mr").Table("pt$20260103").LoaderFrom(csv("2026-01-03,10\n"))
	l.Location = "US"
	l.WriteDisposition = bigquery.WriteAppend
	j, err = l.Run(ctx)
	wait("CSV load into "+other+".mr.pt$20260103", j, err)
	if n := count(oc, "SELECT SUM(x) FROM mr.pt WHERE d = '2026-01-03'"); n != 10 {
		t.Errorf("partition 20260103 of %s.mr.pt sums to %d, want 10", other, n)
	}

	// A query's result written into a partition of the other project.
	pq := c.Query("SELECT DATE '2026-01-04' AS d, 20 AS x")
	pq.Location = "US"
	pq.Dst = c.DatasetInProject(other, "mr").Table("pt$20260104")
	pq.WriteDisposition = bigquery.WriteAppend
	j, err = pq.Run(ctx)
	wait("query into "+other+".mr.pt$20260104", j, err)
	if n := count(oc, "SELECT SUM(x) FROM mr.pt WHERE d = '2026-01-04'"); n != 20 {
		t.Errorf("partition 20260104 of %s.mr.pt sums to %d, want 20", other, n)
	}

	// CREATE TABLE ... CLONE and CREATE SNAPSHOT TABLE naming the other
	// project's tables, run in the instance's project.
	ddl := func(sql string) {
		t.Helper()
		q := c.Query(sql)
		q.Location = "US"
		j, err := q.Run(ctx)
		wait(sql, j, err)
	}
	ddl("CREATE TABLE `" + home + "." + homeDS.DatasetID + ".cl` CLONE `" + other + ".mr.pt`")
	if n := count(c, "SELECT SUM(x) FROM `"+homeDS.DatasetID+".cl`"); n != 36 {
		t.Errorf("the clone of %s.mr.pt sums to %d, want 36", other, n)
	}
	cmd, err := homeDS.Table("cl").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.TimePartitioning == nil || cmd.CloneDefinition == nil || cmd.CloneDefinition.BaseTableReference.ProjectID != other {
		t.Errorf("the clone: partitioning %+v, clone definition %+v; want by d, of %s", cmd.TimePartitioning, cmd.CloneDefinition, other)
	}
	ddl("CREATE SNAPSHOT TABLE `" + other + ".mr.snap` CLONE `" + home + "." + homeDS.DatasetID + ".day`")
	smd, err := ods.Table("snap").Metadata(ctx)
	if err != nil {
		t.Fatalf("tables.get %s.mr.snap: %v", other, err)
	}
	if smd.Type != bigquery.Snapshot || smd.SnapshotDefinition == nil || smd.SnapshotDefinition.BaseTableReference.ProjectID != home || smd.NumRows != 2 {
		t.Errorf("%s.mr.snap: type %s, %+v, %d rows; want a snapshot of %s with 2 rows", other, smd.Type, smd.SnapshotDefinition, smd.NumRows, home)
	}

	// Shut down: refused, then served again once restored.
	if _, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: parent}); err != nil {
		t.Fatalf("DeleteProject %s: %v", other, err)
	}
	var ge *googleapi.Error
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err = ods.Metadata(ctx)
		if errors.As(err, &ge) && ge.Code == http.StatusForbidden {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("datasets.get in %s, DELETE_REQUESTED = %v, want 403", other, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(ge.Errors) == 0 || ge.Errors[0].Reason != "accessDenied" {
		t.Errorf("the refusal's reason = %+v, want accessDenied", ge.Errors)
	}
	uop, err := pc.UndeleteProject(ctx, &rmpb.UndeleteProjectRequest{Name: parent})
	if err != nil {
		t.Fatalf("UndeleteProject %s: %v", other, err)
	}
	if _, err := waitLRO(ctx, uop); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		if _, err = ods.Metadata(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("datasets.get in %s, restored = %v, want its dataset", other, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestBigQueryJobsByFolderAcrossProjects (#1767): region
// INFORMATION_SCHEMA.JOBS_BY_FOLDER of a project lists the jobs of every
// project under the folder that holds it (in it, or in a folder under
// it), each with its own project_id and folder_numbers, and not those of
// a project outside it; JOBS_BY_ORGANIZATION lists every project's of the
// organization. The projects and folders are made with the official
// Resource Manager client, the jobs and the views read with the official
// BigQuery client.
//
// covers: bigquery.jobs.query
func TestBigQueryJobsByFolderAcrossProjects(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	endpoint := h.Endpoint(EnvBigQuery)
	if endpoint == "" {
		t.Skip("no BigQuery endpoint")
	}
	ctx := h.Context()
	fc, _, pc := foldersClientsAt(t, h, rmOptionsAt(runShardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)))
	folder := func(parent, name string) string {
		t.Helper()
		op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: parent, DisplayName: name}})
		if err != nil {
			t.Fatalf("CreateFolder: %v", err)
		}
		f, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateFolder: %v", err)
		}
		t.Cleanup(func() { _, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: f.GetName()}) })
		return f.GetName()
	}
	top := folder(localOrg, folderLabel(h, "jbx-"))
	child := folder(top, "child")
	project := func(prefix, parent string) (string, *bigquery.Client) {
		t.Helper()
		id := v1ID(h, prefix)
		op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id, Parent: parent}})
		if err == nil {
			_, err = waitLRO(ctx, op)
		}
		if err != nil {
			t.Fatalf("CreateProject %s in %s: %v", id, parent, err)
		}
		t.Cleanup(func() {
			_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id})
			_, _ = adminDo(t, http.MethodPost, fmt.Sprintf("http://%s/admin/purge?project=%s", control, id), "", nil)
		})
		cl, err := bigquery.NewClient(ctx, id, option.WithEndpoint(endpoint), option.WithoutAuthentication())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cl.Close() })
		return id, cl
	}
	inChild, childClient := project("bqfc-", child)
	inTop, topClient := project("bqft-", top)
	outside, outsideClient := project("bqfo-", localOrg)
	runJob := func(cl *bigquery.Client) string {
		t.Helper()
		q := cl.Query("SELECT 1767 AS n")
		q.Location = "US"
		j, err := q.Run(ctx)
		if err == nil {
			_, err = j.Wait(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		return j.ID()
	}
	jobs := map[string]string{inChild: runJob(childClient), inTop: runJob(topClient), outside: runJob(outsideClient)}
	ids := "'" + jobs[inChild] + "', '" + jobs[inTop] + "', '" + jobs[outside] + "'"
	read := func(sql string) []string {
		t.Helper()
		q := topClient.Query(sql)
		q.Location = "US"
		it, err := q.Read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var rows []string
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				return rows
			}
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			rows = append(rows, fmt.Sprint(row))
		}
	}
	number := func(name string) string { return strings.TrimPrefix(name, "folders/") }
	got := read("SELECT project_id, job_id, folder_numbers FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_FOLDER WHERE job_id IN (" +
		ids + ") ORDER BY project_id")
	want := []string{
		fmt.Sprintf("[%s %s [%s %s]]", inChild, jobs[inChild], number(child), number(top)),
		fmt.Sprintf("[%s %s [%s]]", inTop, jobs[inTop], number(top)),
	}
	if inTop < inChild {
		want[0], want[1] = want[1], want[0]
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("JOBS_BY_FOLDER of %s = %v, want %v (not %s's)", inTop, got, want, outside)
	}
	got = read("SELECT project_id FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_ORGANIZATION WHERE job_id IN (" + ids + ") ORDER BY project_id")
	if len(got) != 3 {
		t.Errorf("JOBS_BY_ORGANIZATION of %s = %v, want the jobs of %s, %s and %s", inTop, got, inChild, inTop, outside)
	}
}
