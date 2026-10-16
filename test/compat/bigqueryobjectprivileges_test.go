//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryObjectPrivilegesAndSchemataLinks (#1355): region
// INFORMATION_SCHEMA.OBJECT_PRIVILEGES has a SCHEMA row per role the
// dataset's access list grants and a row per member of the table's IAM
// policy, and refuses a query that does not filter on object_name;
// SCHEMATA_LINKS has BigQuery's columns and no rows. Measured first: the
// front refused both 501. A dataset-qualified OBJECT_PRIVILEGES is 400, as
// BigQuery documents the view region-qualified only (#1458).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.datasets.update, bigquery.tables.setIamPolicy
func TestBigQueryObjectPrivilegesAndSchemataLinks(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	md, err := ds.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A new dataset has BigQuery's default access list, which grants
	// projectWriters WRITER already (#1304); the entry is added only when it
	// is missing, as BigQuery keeps one entry per role and entity.
	access := append(md.Access,
		&bigquery.AccessEntry{Role: bigquery.ReaderRole, EntityType: bigquery.UserEmailEntity, Entity: "ada@example.com"})
	writers := false
	for _, a := range md.Access {
		writers = writers || (a.Role == bigquery.WriterRole && a.EntityType == bigquery.SpecialGroupEntity && a.Entity == "projectWriters")
	}
	if !writers {
		access = append(access, &bigquery.AccessEntry{Role: bigquery.WriterRole, EntityType: bigquery.SpecialGroupEntity, Entity: "projectWriters"})
	}
	if _, err := ds.Update(ctx, bigquery.DatasetMetadataToUpdate{Access: access}, md.ETag); err != nil {
		t.Fatal(err)
	}
	tbl := ds.Table("priv")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatal(err)
	}
	pol, err := tbl.IAM().Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pol.Add("user:bob@example.com", "roles/bigquery.admin")
	if err := tbl.IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatal(err)
	}

	read := func(sql string, insert bool) ([]string, error) {
		q := c.Query(sql)
		var it *bigquery.RowIterator
		var err error
		if insert {
			job, err := q.Run(ctx)
			if err != nil {
				return nil, err
			}
			if it, err = job.Read(ctx); err != nil {
				return nil, err
			}
		} else if it, err = q.Read(ctx); err != nil {
			return nil, err
		}
		var out []string
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				return nil, err
			}
			out = append(out, fmt.Sprint(row))
		}
		sort.Strings(out)
		return out, nil
	}
	view := "`" + project + "`.`region-us`.INFORMATION_SCHEMA.OBJECT_PRIVILEGES"
	cols := "SELECT object_catalog, object_schema, object_name, object_type, privilege_type, grantee FROM "
	for _, insert := range []bool{false, true} {
		got, err := read(cols+view+" WHERE object_name = '"+ds.DatasetID+"' AND grantee IN ('user:ada@example.com', 'projectEditor')", insert)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			fmt.Sprintf("[%s <nil> %s SCHEMA roles/bigquery.dataEditor projectEditor]", project, ds.DatasetID),
			fmt.Sprintf("[%s <nil> %s SCHEMA roles/bigquery.dataViewer user:ada@example.com]", project, ds.DatasetID),
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("insert=%v: dataset rows\n%s\nwant\n%s", insert, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		got, err = read(cols+view+" WHERE object_schema = '"+ds.DatasetID+"' AND object_name = 'priv'", insert)
		if err != nil {
			t.Fatal(err)
		}
		if w := fmt.Sprintf("[%s %s priv TABLE roles/bigquery.admin user:bob@example.com]", project, ds.DatasetID); len(got) != 1 || got[0] != w {
			t.Errorf("insert=%v: table rows %v, want [%s]", insert, got, w)
		}
	}
	_, err = read(cols+view, false)
	wantCode(t, "OBJECT_PRIVILEGES with no object_name filter", err, http.StatusBadRequest, "object_name")
	// BigQuery documents both views region-qualified only (#1458).
	_, err = read(cols+"`"+project+"."+ds.DatasetID+".INFORMATION_SCHEMA.OBJECT_PRIVILEGES` WHERE object_name = 'priv'", false)
	wantCode(t, "dataset-qualified OBJECT_PRIVILEGES", err, http.StatusBadRequest, "must include a region qualifier")

	got, err := read("SELECT catalog_name, schema_name, linked_schema_catalog_number, linked_schema_catalog_name, linked_schema_name, "+
		"linked_schema_creation_time, linked_schema_org_display_name, shared_asset_id, link_type FROM `region-us`.INFORMATION_SCHEMA.SCHEMATA_LINKS", false)
	if err != nil || len(got) != 0 {
		t.Errorf("SCHEMATA_LINKS = %v, %v; want no rows", got, err)
	}
}
