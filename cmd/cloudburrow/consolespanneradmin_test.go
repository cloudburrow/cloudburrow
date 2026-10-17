package main

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestSpannerDDLSplitter: Update schema's text becomes UpdateDatabaseDdl's
// statements, split at semicolons outside quotes and comments, each without
// its terminator; DML, queries and anything not DDL are refused with where
// they belong, and nothing at all is refused too (#1207).
func TestSpannerDDLSplitter(t *testing.T) {
	got, err := splitSpannerDDL(`CREATE TABLE A (Id INT64 NOT NULL, Note STRING(MAX) DEFAULT ("x;y")) PRIMARY KEY (Id);
		-- a comment; with a semicolon
		CREATE INDEX AByNote ON A (Note) ;
		/* another; */ ALTER TABLE A ADD COLUMN ` + "`semi;colon`" + ` INT64;;`)
	if err != nil {
		t.Fatalf("splitSpannerDDL: %v", err)
	}
	if len(got) != 3 || !strings.HasPrefix(got[0], "CREATE TABLE A") || !strings.HasSuffix(got[0], "PRIMARY KEY (Id)") ||
		!strings.HasSuffix(got[1], "CREATE INDEX AByNote ON A (Note)") || !strings.HasSuffix(got[2], "INT64") {
		t.Fatalf("splitSpannerDDL = %q, want the three statements without their semicolons", got)
	}
	if got, err := splitSpannerDDL("DROP TABLE A"); err != nil || !slices.Equal(got, []string{"DROP TABLE A"}) {
		t.Errorf("one statement without a semicolon = %q, %v", got, err)
	}
	for sql, want := range map[string]string{
		"":                              "at least one DDL statement",
		" ; ;":                          "at least one DDL statement",
		"INSERT INTO A (Id) VALUES (1)": "query editor's Read-write mode",
		"CREATE TABLE B (Id INT64) PRIMARY KEY (Id); DELETE FROM A WHERE TRUE": "changes data",
		"SELECT 1":                                "reads data",
		"EXPLAIN SELECT 1":                        "not EXPLAIN",
		"CREATE TABLE 'unterminated":              "unterminated",
		strings.Repeat("x", maxSpannerDDLBytes+1): "at most",
	} {
		if _, err := splitSpannerDDL(sql); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("splitSpannerDDL(%.40q) = %v, want an error saying %q", sql, err, want)
		}
	}
}

// TestSpannerAdminActionsAreOfferedWhereTheyBelong: Update schema on a
// database's page, confirmed with the database named; Create instance
// partition on an instance's; Delete instance partition on a partition's,
// leaving it; nothing on a table's page or the partitions page (#1207). A
// partition's path cannot be read as a database's: no database ID has a
// capital letter. Since #1498 an instance's page also has Create backup,
// Create custom configuration and Move instance, a database's Create backup
// schedule, and a partition's Edit instance partition.
func TestSpannerAdminActionsAreOfferedWhereTheyBelong(t *testing.T) {
	_, p := startFakeSpannerAdmin(t)
	ids := func(path ...string) []string {
		var out []string
		for _, a := range p.DetailActions(context.Background(), "proj", path) {
			out = append(out, a.ID)
		}
		return out
	}
	if got := ids("inst"); !slices.Equal(got, []string{"editinstance", "createpartition", "createbackup", "createconfig", "moveinstance", "dropinstance"}) {
		t.Errorf("an instance's actions = %v", got)
	}
	if got := ids("inst", "db"); !slices.Equal(got, []string{"updateddl", "createschedule", "dropdatabase", "dropprotection"}) {
		t.Errorf("a database's actions = %v", got)
	}
	if got := ids("inst", spannerPartitions); len(got) != 0 {
		t.Errorf("the partitions page offers %v, want nothing", got)
	}
	if got := ids("inst", spannerPartitions, "p1"); !slices.Equal(got, []string{"editpartition", "deletepartition"}) {
		t.Errorf("a partition's actions = %v", got)
	}
	if got := ids("inst", "db", "Table"); len(got) != 0 {
		t.Errorf("a table's page offers %v, want nothing", got)
	}

	ddl := spannerDDLAction("inst", "db")
	if !strings.Contains(ddl.Confirm, "database db in instance inst") || ddl.Destructive || len(ddl.Fields) != 1 ||
		ddl.Fields[0].Type != "textarea" || !ddl.Fields[0].Required {
		t.Errorf("Update schema is %+v; want a confirmed, non-destructive action with one required textarea naming the database", ddl)
	}
	if del := spannerDeletePartitionAction(true); !del.Destructive || !del.Leaves {
		t.Errorf("Delete instance partition on its page is %+v, want destructive and leaving", del)
	}
	if isSpannerPartitionPath([]string{"inst", "instancepartitions"}) {
		t.Error("a database named instancepartitions is read as the partitions page")
	}

	// The actions refuse a path they do not belong to before any call.
	for _, c := range []struct {
		action string
		path   []string
	}{
		{"updateddl", []string{"inst"}},
		{"updateddl", []string{"inst", spannerPartitions}},
		{"createpartition", []string{"inst", "db"}},
		{"deletepartition", []string{"inst", "db", "Table"}},
	} {
		handled, err := p.spannerAdminAct(context.Background(), "proj", c.path, c.action, nil)
		if !handled || err == nil {
			t.Errorf("%s at %v = (%v, %v), want it refused", c.action, c.path, handled, err)
		}
	}
	if handled, _ := p.spannerAdminAct(context.Background(), "proj", []string{"inst"}, "dropinstance", nil); handled {
		t.Error("spannerAdminAct claims dropinstance, which ActAt performs")
	}
}
