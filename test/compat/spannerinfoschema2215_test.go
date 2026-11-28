//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/spanner"
	"github.com/cloudburrow/cloudburrow/internal/localclient"
	"google.golang.org/api/iterator"
)

// covers: google.spanner.v1.Spanner/ExecuteStreamingSql
//
// TestSpannerColumnOptionsIsInherited (#2215): the INFORMATION_SCHEMA
// that Google's Spanner import and export templates read
// (DataflowTemplates' InformationSchemaScanner) and the emulator lacks:
// COLUMN_OPTIONS answers IS_INHERITED (an option set on the column itself
// is not inherited), and TABLE_OPTIONS, INDEX_OPTIONS, PLACEMENTS and
// PLACEMENT_OPTIONS answer, empty for a database with no table, index or
// placement options.
func TestSpannerColumnOptionsIsInherited(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	t.Setenv("SPANNER_DISABLE_BUILTIN_METRICS", "true")
	db := spannerDatabase(t, ctx, addr, h.Project(), fmt.Sprint("inherit-", time.Now().UnixNano()%1e6), "options",
		"CREATE TABLE Events (Id STRING(10) NOT NULL, Happened TIMESTAMP OPTIONS (allow_commit_timestamp = true)) PRIMARY KEY (Id)")
	sp, err := localclient.Spanner(ctx, db, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	it := sp.Single().Query(ctx, spanner.Statement{SQL: "SELECT t.table_schema, t.table_name, t.column_name, t.option_name, t.option_type," +
		" t.option_value, t.is_inherited FROM information_schema.column_options AS t" +
		" WHERE t.table_schema NOT IN ('INFORMATION_SCHEMA', 'SPANNER_SYS') ORDER BY t.table_name, t.column_name"})
	defer it.Stop()
	var got []string
	for {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("the column options: %v", err)
		}
		var schema, table, column, name, typ, value string
		var inherited bool
		if err := r.Columns(&schema, &table, &column, &name, &typ, &value, &inherited); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s.%s %s=%s %s inherited=%v", table, column, name, value, typ, inherited))
	}
	if want := "Events.Happened allow_commit_timestamp=TRUE BOOL inherited=false"; strings.Join(got, ";") != want {
		t.Errorf("the column options are %q; want %q", got, want)
	}

	// The scanner's queries of the other views (listTableOptionsSQL,
	// listIndexOptionsSQL, listPlacements).
	for _, q := range []string{
		"SELECT t.table_schema, t.table_name, t.option_name, t.option_type, t.option_value FROM information_schema.table_options AS t" +
			" WHERE t.table_schema NOT IN ('INFORMATION_SCHEMA', 'SPANNER_SYS') ORDER BY t.table_name, t.option_name",
		"SELECT t.table_schema, t.table_name, t.index_name, t.index_type, t.option_name, t.option_type, t.option_value" +
			" FROM information_schema.index_options AS t WHERE t.table_schema NOT IN ('INFORMATION_SCHEMA', 'SPANNER_SYS')" +
			" ORDER BY t.table_name, t.index_name, t.option_name",
		"SELECT p.placement_name, p.is_default, po.option_name, po.option_type, po.option_value FROM information_schema.placements AS p" +
			" LEFT JOIN information_schema.placement_options AS po ON p.placement_name = po.placement_name ORDER BY po.placement_name, po.option_name",
	} {
		it := sp.Single().Query(ctx, spanner.Statement{SQL: q})
		n := 0
		err := it.Do(func(*spanner.Row) error { n++; return nil })
		if err != nil || n != 0 {
			t.Errorf("%s: %d rows, %v; want none", q, n, err)
		}
	}
}
