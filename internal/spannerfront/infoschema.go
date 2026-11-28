package spannerfront

// INFORMATION_SCHEMA views and view columns that Cloud Spanner answers and
// the emulator lacks (#2215). The Google-provided Dataflow templates that
// read a database's schema (DataflowTemplates' InformationSchemaScanner:
// Cloud Spanner to Avro Files, Avro and Text Files on Cloud Storage to
// Cloud Spanner) select them on every run:
//   - COLUMN_OPTIONS.IS_INHERITED and LOCALITY_GROUP_OPTIONS.IS_INHERITED,
//     whether an option's value is inherited rather than set on the object
//     itself. Every option the emulator records is set on its own column or
//     locality group (it has none of the objects one inherits from): FALSE
//     (PostgreSQL 'NO', which the template reads as a string).
//   - TABLE_OPTIONS and INDEX_OPTIONS (table_schema, table_name, [index_name,
//     index_type,] option_name, option_type, option_value), and PLACEMENTS
//     and PLACEMENT_OPTIONS (documented, geo-partitioning's). The emulator
//     records no table, index or placement options: each view is empty.
// A query the emulator refuses for one of these is run again with the view
// in a subquery that adds the column, or with an empty one in the view's
// place; an emulator that has them answers the query itself.

import (
	"context"
	"regexp"
	"strings"

	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// viewRef matches a query's reference to INFORMATION_SCHEMA.<name>.
func viewRef(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\binformation_schema\s*\.\s*` + name + `\b`)
}

// infoSchemaColumn is a view's column the emulator lacks, and its value.
type infoSchemaColumn struct {
	view   *regexp.Regexp
	column string
	// gsql and pg are the column's value in each dialect.
	gsql, pg string
}

var infoSchemaColumns = []infoSchemaColumn{
	{viewRef("column_options"), "is_inherited", "FALSE", "'NO'"},
	{viewRef("locality_group_options"), "is_inherited", "FALSE", "'NO'"},
}

// viewColumn is a column of a view the emulator lacks: its name and its
// type's empty value in each dialect.
type viewColumn struct{ name, gsql, pg string }

var (
	gsqlString = "CAST(NULL AS STRING)"
	pgString   = "CAST(NULL AS character varying)"
)

func stringColumns(names ...string) []viewColumn {
	var cs []viewColumn
	for _, n := range names {
		cs = append(cs, viewColumn{n, gsqlString, pgString})
	}
	return cs
}

// infoSchemaViews are the views the emulator lacks, by lower-case name.
var infoSchemaViews = map[string][]viewColumn{
	"table_options": stringColumns("table_catalog", "table_schema", "table_name", "option_name", "option_type", "option_value"),
	"index_options": stringColumns("table_catalog", "table_schema", "table_name", "index_name", "index_type",
		"option_name", "option_type", "option_value"),
	"placements":        {{"placement_name", gsqlString, pgString}, {"is_default", "CAST(NULL AS BOOL)", pgString}},
	"placement_options": stringColumns("placement_name", "option_name", "option_type", "option_value"),
}

// missingColumn reads the column an emulator's refusal names: GoogleSQL's
// "Name is_inherited not found inside t", PostgreSQL's "column
// t.is_inherited does not exist".
var missingColumn = regexp.MustCompile(`(?i)Name (\w+) not found inside|column (?:\w+\.)?(\w+) does not exist`)

// missingView reads the view: GoogleSQL's "Table not found:
// information_schema.table_options", PostgreSQL's `relation
// "information_schema.table_options" does not exist`.
var missingView = regexp.MustCompile(`(?i)Table not found: information_schema\.(\w+)|relation "information_schema\.(\w+)" does not exist`)

// withInfoSchemaColumns is req rewritten for the view or view column the
// emulator refused it for (err), and whether it was.
func (s *Instances) withInfoSchemaColumns(ctx context.Context, req *spannerpb.ExecuteSqlRequest, err error) (*spannerpb.ExecuteSqlRequest, bool) {
	if status.Code(err) != codes.InvalidArgument {
		return nil, false
	}
	msg := status.Convert(err).Message()
	sql := req.GetSql()
	pg := func() bool {
		if s.emuDB == nil {
			return false
		}
		db, err := s.emuDB.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: databaseOfSession(req.GetSession())})
		return err == nil && db.GetDatabaseDialect() == databasepb.DatabaseDialect_POSTGRESQL
	}
	rewritten := ""
	if m := missingView.FindStringSubmatch(msg); m != nil {
		name := strings.ToLower(m[1] + m[2])
		cols, ok := infoSchemaViews[name]
		if !ok {
			return nil, false
		}
		inPG := pg()
		var sel []string
		for _, c := range cols {
			v := c.gsql
			if inPG {
				v = c.pg
			}
			sel = append(sel, v+" AS "+c.name)
		}
		empty := "(SELECT " + strings.Join(sel, ", ") + " LIMIT 0)"
		rewritten = viewRef(name).ReplaceAllLiteralString(sql, empty)
	} else if m := missingColumn.FindStringSubmatch(msg); m != nil {
		col := strings.ToLower(m[1] + m[2])
		var fix []infoSchemaColumn
		for _, c := range infoSchemaColumns {
			if c.column == col && c.view.MatchString(sql) {
				fix = append(fix, c)
			}
		}
		if len(fix) == 0 {
			return nil, false
		}
		inPG := pg()
		rewritten = sql
		for _, c := range fix {
			v := c.gsql
			if inPG {
				v = c.pg
			}
			rewritten = c.view.ReplaceAllStringFunc(rewritten, func(ref string) string {
				return "(SELECT *, " + v + " AS " + c.column + " FROM " + ref + ")"
			})
		}
	}
	if rewritten == "" || rewritten == sql {
		return nil, false
	}
	r := proto.Clone(req).(*spannerpb.ExecuteSqlRequest)
	r.Sql = rewritten
	return r, true
}

// maxInfoSchemaRewrites bounds the rewrites of one query (each refusal
// names one view or column: a join of PLACEMENTS and PLACEMENT_OPTIONS is
// refused twice).
const maxInfoSchemaRewrites = 6
