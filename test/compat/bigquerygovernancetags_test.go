//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"
)

// TestBigQueryDataGovernanceTags (#1456): a column's data_governance_tags,
// ARRAY<STRUCT<STRING, STRING>> ("Applies data governance tags to a column
// in a table. Example: data_governance_tags = [("myproject/tag_key",
// "tag_value")]", the DDL reference's column_option_list), set by CREATE
// TABLE and by ALTER COLUMN SET OPTIONS, NULL clearing them, read back as
// the field's dataGovernanceTagsInfo.dataGovernanceTags through tables.get;
// and set and cleared by tables.patch (an empty dataGovernanceTagsInfo
// clears them, one left out keeps them: TableFieldSchema). Each statement
// through jobs.query and jobs.insert. Refused 400: two tags on a field
// ("Only one data governance tag is currently supported on a field"), a
// key not "parent-id/key", +=. A column renamed keeps its tags, one dropped loses them. Before,
// data_governance_tags was 501 (the engine's schema has no such field).
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get, bigquery.tables.patch
func TestBigQueryDataGovernanceTags(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	svc, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	for _, insert := range []bool{false, true} {
		name := fmt.Sprintf("govtags%v", insert)
		ref := "`" + ds.DatasetID + "." + name + "`"
		run := func(sql string) {
			t.Helper()
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s (insert %v): %v", sql, insert, err)
			}
		}
		tags := func() string {
			t.Helper()
			full, err := svc.Tables.Get(project, ds.DatasetID, name).Context(ctx).Do()
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, f := range full.Schema.Fields {
				if f.DataGovernanceTagsInfo == nil {
					continue
				}
				for k, v := range f.DataGovernanceTagsInfo.DataGovernanceTags {
					out = append(out, f.Name+":"+k+"="+v)
				}
			}
			sort.Strings(out)
			return strings.Join(out, ",")
		}
		run("CREATE TABLE " + ref + " (a STRING OPTIONS (data_governance_tags = [('" + project + "/pii', 'sensitive')]), b INT64, " +
			"s STRUCT<x INT64>)")
		if got, want := tags(), "a:"+project+"/pii=sensitive"; got != want {
			t.Errorf("insert %v: after CREATE TABLE: %q, want %q", insert, got, want)
		}
		run("ALTER TABLE " + ref + " ALTER COLUMN b SET OPTIONS (data_governance_tags = [('" + project + "/cost_center', 'sales')])")
		if got, want := tags(), "a:"+project+"/pii=sensitive,b:"+project+"/cost_center=sales"; got != want {
			t.Errorf("insert %v: after ALTER COLUMN b: %q, want %q", insert, got, want)
		}
		run("ALTER TABLE " + ref + " ALTER COLUMN a SET OPTIONS (data_governance_tags = NULL)")
		if got, want := tags(), "b:"+project+"/cost_center=sales"; got != want {
			t.Errorf("insert %v: after data_governance_tags = NULL: %q, want %q", insert, got, want)
		}
		for _, sql := range []string{
			"ALTER TABLE " + ref + " ALTER COLUMN a SET OPTIONS (data_governance_tags = [('" + project + "/k', 'v'), ('" + project + "/l', 'w')])",
			"ALTER TABLE " + ref + " ALTER COLUMN a SET OPTIONS (data_governance_tags = [('nokey', 'v')])",
			"ALTER TABLE " + ref + " ALTER COLUMN a SET OPTIONS (data_governance_tags += [('" + project + "/k', 'v')])",
		} {
			wantHTTPStatus(t, sql, bqRun(ctx, c, sql, insert), http.StatusBadRequest)
		}
		if got, want := tags(), "b:"+project+"/cost_center=sales"; got != want {
			t.Errorf("insert %v: after the refusals: %q, want %q", insert, got, want)
		}
		run("ALTER TABLE " + ref + " RENAME COLUMN b TO c")
		if got, want := tags(), "c:"+project+"/cost_center=sales"; got != want {
			t.Errorf("insert %v: after RENAME COLUMN b TO c: %q, want %q", insert, got, want)
		}
		run("ALTER TABLE " + ref + " DROP COLUMN c")
		run("ALTER TABLE " + ref + " ADD COLUMN c INT64")
		if got := tags(); got != "" {
			t.Errorf("insert %v: after DROP COLUMN c and ADD COLUMN c: %q, want none", insert, got)
		}
	}

	// tables.patch: dataGovernanceTagsInfo set on a field, kept by a patch
	// that leaves it out, cleared by an empty one.
	name := "govtagspatch"
	run := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	run("CREATE TABLE `" + ds.DatasetID + "." + name + "` (a STRING, b INT64)")
	patch := func(info *bq.TableFieldSchemaDataGovernanceTagsInfo, desc string) *bq.Table {
		t.Helper()
		a := &bq.TableFieldSchema{Name: "a", Type: "STRING", Description: desc, DataGovernanceTagsInfo: info}
		got, err := svc.Tables.Patch(project, ds.DatasetID, name, &bq.Table{Schema: &bq.TableSchema{Fields: []*bq.TableFieldSchema{
			a, {Name: "b", Type: "INTEGER"},
		}}}).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	tagOf := func(tb *bq.Table) map[string]string {
		if f := tb.Schema.Fields[0]; f.DataGovernanceTagsInfo != nil {
			return f.DataGovernanceTagsInfo.DataGovernanceTags
		}
		return nil
	}
	want := map[string]string{project + "/pii": "sensitive"}
	if got := tagOf(patch(&bq.TableFieldSchemaDataGovernanceTagsInfo{DataGovernanceTags: want}, "")); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("tables.patch with tags answered %v, want %v", got, want)
	}
	if got := tagOf(patch(nil, "d")); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("tables.patch without dataGovernanceTagsInfo answered %v, want %v kept", got, want)
	}
	full, err := svc.Tables.Get(project, ds.DatasetID, name).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := tagOf(full); fmt.Sprint(got) != fmt.Sprint(want) || full.Schema.Fields[0].Description != "d" {
		t.Errorf("tables.get after the patches: tags %v, description %q, want %v and d", got, full.Schema.Fields[0].Description, want)
	}
	if got := tagOf(patch(&bq.TableFieldSchemaDataGovernanceTagsInfo{}, "d")); len(got) != 0 {
		t.Errorf("tables.patch with an empty dataGovernanceTagsInfo answered %v, want none", got)
	}
	_, err = svc.Tables.Patch(project, ds.DatasetID, name, &bq.Table{Schema: &bq.TableSchema{Fields: []*bq.TableFieldSchema{
		{Name: "a", Type: "STRING", DataGovernanceTagsInfo: &bq.TableFieldSchemaDataGovernanceTagsInfo{
			DataGovernanceTags: map[string]string{project + "/k": "v", project + "/l": "w"}}},
		{Name: "b", Type: "INTEGER"},
	}}}).Context(ctx).Do()
	if httpStatus(err) != http.StatusBadRequest {
		t.Errorf("tables.patch with two tags on a field: %v, want 400", err)
	}
}
