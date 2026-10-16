package bigqueryfront

import "testing"

func TestParseAlterProject(t *testing.T) {
	for _, c := range []struct {
		sql, project, value string
		null, bad, refused  bool
	}{
		{sql: "ALTER PROJECT SET OPTIONS (`default_location` = 'EU')", project: "p", value: "EU"},
		{sql: "ALTER PROJECT `other` SET OPTIONS (default_location = 'us-east1');", project: "other", value: "us-east1"},
		{sql: "ALTER PROJECT p SET OPTIONS (`default_location` = NULL)", project: "p", null: true},
		{sql: "ALTER PROJECT SET OPTIONS (`region-us.default_time_zone` = 'UTC')", refused: true},
		{sql: "ALTER ORGANIZATION SET OPTIONS (`default_location` = 'EU')", refused: true},
		{sql: "ALTER PROJECT SET OPTIONS (`default_location` = 'EU'); SELECT 1", refused: true},
		{sql: "ALTER PROJECT SET OPTIONS (`default_location` = 3)", bad: true},
	} {
		a, msg, bad, ok := parseAlterProject(c.sql, "p")
		if !ok {
			t.Errorf("%s: not read", c.sql)
			continue
		}
		if c.refused || c.bad {
			if msg == "" || bad != c.bad {
				t.Errorf("%s: msg %q bad %v", c.sql, msg, bad)
			}
			continue
		}
		v, set := a.sets["default_location"]
		if msg != "" || a.project != c.project || !set || (v == nil) != c.null || v != nil && *v != c.value {
			t.Errorf("%s: %+v %q", c.sql, a, msg)
		}
	}
	if _, _, _, ok := parseAlterProject("ALTER TABLE d.t SET OPTIONS (description = 'x')", "p"); ok {
		t.Error("ALTER TABLE read as ALTER PROJECT")
	}
}

func TestSchemaDDL(t *testing.T) {
	got := schemaDDL("myproject", "mydataset2", map[string]any{"location": "US", "defaultPartitionExpirationMs": "259200000"})
	want := "CREATE SCHEMA `myproject.mydataset2`\nOPTIONS(\n  default_partition_expiration_days=3.0,\n  location=\"us\"\n);"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestProjectOptionsDefault(t *testing.T) {
	var none *projectOptions
	if got := none.defaultLocation("p"); got != "US" {
		t.Errorf("nil: %s", got)
	}
	p := &projectOptions{}
	eu := "EU"
	p.set("p", "default_location", &eu)
	if p.defaultLocation("p") != "EU" || p.defaultLocation("q") != "US" {
		t.Error("default_location not per project")
	}
	p.set("p", "default_location", nil)
	if p.defaultLocation("p") != "US" {
		t.Error("NULL did not clear it")
	}
}
