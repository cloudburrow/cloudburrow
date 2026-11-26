package spannerfront

import (
	"context"
	"errors"
	"testing"

	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestWithInfoSchemaColumns(t *testing.T) {
	s := &Instances{}
	ctx := context.Background()
	// DataflowTemplates' InformationSchemaScanner.listColumnOptionsSQL.
	q := "SELECT t.table_schema, t.table_name, t.column_name, t.option_name, t.option_type, t.option_value, t.is_inherited" +
		" FROM information_schema.column_options AS t WHERE t.table_schema NOT IN ('INFORMATION_SCHEMA', 'SPANNER_SYS')"
	req := &spannerpb.ExecuteSqlRequest{Session: "projects/p/instances/i/databases/d/sessions/s", Sql: q}
	refused := status.Error(codes.InvalidArgument, "Name is_inherited not found inside t [at 1:101]")
	r, ok := s.withInfoSchemaColumns(ctx, req, refused)
	want := "SELECT t.table_schema, t.table_name, t.column_name, t.option_name, t.option_type, t.option_value, t.is_inherited" +
		" FROM (SELECT *, FALSE AS is_inherited FROM information_schema.column_options) AS t WHERE t.table_schema NOT IN ('INFORMATION_SCHEMA', 'SPANNER_SYS')"
	if !ok || r.GetSql() != want {
		t.Fatalf("rewritten = %v %q; want %q", ok, r.GetSql(), want)
	}
	if req.GetSql() != q {
		t.Error("the request was changed in place")
	}
	lg := &spannerpb.ExecuteSqlRequest{Sql: "SELECT t.locality_group_name, t.is_inherited FROM INFORMATION_SCHEMA.LOCALITY_GROUP_OPTIONS AS t"}
	if r, ok := s.withInfoSchemaColumns(ctx, lg, status.Error(codes.InvalidArgument, "column t.is_inherited does not exist")); !ok ||
		r.GetSql() != "SELECT t.locality_group_name, t.is_inherited FROM (SELECT *, FALSE AS is_inherited FROM INFORMATION_SCHEMA.LOCALITY_GROUP_OPTIONS) AS t" {
		t.Errorf("locality groups rewritten = %v %q", ok, r.GetSql())
	}
	// listTableOptionsSQL: the emulator has no TABLE_OPTIONS.
	to := &spannerpb.ExecuteSqlRequest{Sql: "SELECT t.table_schema, t.table_name, t.option_name, t.option_type, t.option_value" +
		" FROM information_schema.table_options AS t WHERE t.table_schema NOT IN ('INFORMATION_SCHEMA', 'SPANNER_SYS')"}
	if r, ok := s.withInfoSchemaColumns(ctx, to, status.Error(codes.InvalidArgument, "Table not found: information_schema.table_options [at 1:88]")); !ok ||
		r.GetSql() != "SELECT t.table_schema, t.table_name, t.option_name, t.option_type, t.option_value FROM (SELECT CAST(NULL AS STRING) AS table_catalog,"+
			" CAST(NULL AS STRING) AS table_schema, CAST(NULL AS STRING) AS table_name, CAST(NULL AS STRING) AS option_name,"+
			" CAST(NULL AS STRING) AS option_type, CAST(NULL AS STRING) AS option_value LIMIT 0) AS t WHERE t.table_schema NOT IN ('INFORMATION_SCHEMA', 'SPANNER_SYS')" {
		t.Errorf("table options rewritten = %v %q", ok, r.GetSql())
	}
	// listPlacements joins two views the emulator lacks: refused for each.
	pl := &spannerpb.ExecuteSqlRequest{Sql: "SELECT p.placement_name, p.is_default, po.option_name FROM information_schema.placements AS p" +
		" LEFT JOIN information_schema.placement_options AS po ON p.placement_name = po.placement_name"}
	r1, ok := s.withInfoSchemaColumns(ctx, pl, status.Error(codes.InvalidArgument, "Table not found: information_schema.placements [at 1:70]"))
	if !ok {
		t.Fatal("placements not rewritten")
	}
	r2, ok := s.withInfoSchemaColumns(ctx, r1, status.Error(codes.InvalidArgument, "Table not found: information_schema.placement_options [at 1:200]"))
	if want := "SELECT p.placement_name, p.is_default, po.option_name FROM (SELECT CAST(NULL AS STRING) AS placement_name, CAST(NULL AS BOOL) AS is_default LIMIT 0) AS p" +
		" LEFT JOIN (SELECT CAST(NULL AS STRING) AS placement_name, CAST(NULL AS STRING) AS option_name, CAST(NULL AS STRING) AS option_type," +
		" CAST(NULL AS STRING) AS option_value LIMIT 0) AS po ON p.placement_name = po.placement_name"; !ok || r2.GetSql() != want {
		t.Errorf("placements rewritten = %v %q; want %q", ok, r2.GetSql(), want)
	}
	if _, ok := s.withInfoSchemaColumns(ctx, to, status.Error(codes.InvalidArgument, "Table not found: information_schema.nope")); ok {
		t.Error("an unknown view was rewritten")
	}
	for name, c := range map[string]struct {
		sql string
		err error
	}{
		"no error":           {q, nil},
		"another code":       {q, status.Error(codes.NotFound, "Name is_inherited not found inside t")},
		"another column":     {q, status.Error(codes.InvalidArgument, "Name nope not found inside t")},
		"another view":       {"SELECT is_inherited FROM information_schema.columns", refused},
		"not a status error": {q, errors.New("Name is_inherited not found inside t")},
	} {
		if _, ok := s.withInfoSchemaColumns(ctx, &spannerpb.ExecuteSqlRequest{Sql: c.sql}, c.err); ok {
			t.Errorf("%s: rewritten", name)
		}
	}
}
