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
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/iterator"
)

// TestBigQueryJobsByFolderAndOrganization (#1392): region
// INFORMATION_SCHEMA.JOBS_BY_FOLDER lists the jobs of the folder that
// holds the project, with folder_numbers the project's folders nearest
// first, and JOBS_BY_ORGANIZATION those of its organization, as Resource
// Manager places the project (the official Resource Manager client moves
// it); a project in no folder is refused 400, another region 404.
// INFORMATION_SCHEMA.SCHEMATA_LINKS with no region qualifier, and
// project-qualified, reads the US region's (no rows), with its columns.
//
// covers: bigquery.jobs.query, google.cloud.resourcemanager.v3.Projects/MoveProject
func TestBigQueryJobsByFolderAndOrganization(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	fc, _, pc := foldersClients(t, h)
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
	move := func(parent string) {
		t.Helper()
		op, err := pc.MoveProject(ctx, &rmpb.MoveProjectRequest{Name: "projects/" + project, DestinationParent: parent})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatalf("MoveProject(%s): %v", parent, err)
		}
	}
	top := folder(localOrg, folderLabel(h, "jbf-"))
	child := folder(top, "child")
	move(child)
	t.Cleanup(func() {
		// A project cannot be moved to no parent: it is left in the
		// organization, its folders deleted.
		if op, err := pc.MoveProject(context.Background(), &rmpb.MoveProjectRequest{Name: "projects/" + project, DestinationParent: localOrg}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})

	q := c.Query("SELECT 1392 AS n")
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	read := func(sql string) (bigquery.Schema, []string, error) {
		qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		it, err := c.Query(sql).Read(qctx)
		if err != nil {
			return nil, nil, err
		}
		var rows []string
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				return it.Schema, rows, nil
			}
			if err != nil {
				return nil, nil, err
			}
			rows = append(rows, fmt.Sprint(row))
		}
	}
	number := func(name string) string { return strings.TrimPrefix(name, "folders/") }
	where := " WHERE job_id = '" + job.ID() + "'"
	_, got, err := read("SELECT job_id, project_id, job_type, state, folder_numbers FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_FOLDER" + where)
	want := fmt.Sprintf("[%s %s QUERY DONE [%s %s]]", job.ID(), project, number(child), number(top))
	if err != nil || len(got) != 1 || got[0] != want {
		t.Errorf("JOBS_BY_FOLDER = %v, %v; want [%s]", got, err, want)
	}
	schema, got, err := read("SELECT * FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_ORGANIZATION" + where)
	if err != nil || len(got) != 1 {
		t.Errorf("JOBS_BY_ORGANIZATION = %v, %v; want the job", got, err)
	}
	for _, col := range []string{"creation_time", "project_id", "job_id", "user_email", "total_bytes_processed"} {
		found := false
		for _, f := range schema {
			found = found || f.Name == col
		}
		if !found {
			t.Errorf("JOBS_BY_ORGANIZATION has no column %s", col)
		}
	}
	_, _, err = read("SELECT job_id FROM `region-eu`.INFORMATION_SCHEMA.JOBS_BY_FOLDER")
	wantCode(t, "JOBS_BY_FOLDER of another region", err, http.StatusNotFound, "region-eu")

	// In the organization, in no folder.
	move(localOrg)
	_, _, err = read("SELECT job_id FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_FOLDER" + where)
	wantCode(t, "JOBS_BY_FOLDER of a project in no folder", err, http.StatusBadRequest, "not in a folder")
	if _, got, err := read("SELECT job_id FROM `region-us`.INFORMATION_SCHEMA.JOBS_BY_ORGANIZATION" + where); err != nil || len(got) != 1 {
		t.Errorf("JOBS_BY_ORGANIZATION in the organization = %v, %v; want the job", got, err)
	}

	cols := "catalog_name, schema_name, linked_schema_catalog_number, linked_schema_catalog_name, linked_schema_name, " +
		"linked_schema_creation_time, linked_schema_org_display_name, shared_asset_id, link_type"
	for _, view := range []string{"INFORMATION_SCHEMA.SCHEMATA_LINKS", "`" + project + "`.INFORMATION_SCHEMA.SCHEMATA_LINKS"} {
		if _, got, err := read("SELECT " + cols + " FROM " + view); err != nil || len(got) != 0 {
			t.Errorf("%s = %v, %v; want no rows", view, got, err)
		}
	}
}
