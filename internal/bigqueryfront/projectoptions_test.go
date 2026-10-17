package bigqueryfront

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

func TestParseAlterProject(t *testing.T) {
	for _, c := range []struct {
		sql, scope, key, value string
		null, bad, refused     bool
	}{
		{sql: "ALTER PROJECT SET OPTIONS (`default_location` = 'EU')", scope: "p", key: "default_location", value: "EU"},
		{sql: "ALTER PROJECT `other` SET OPTIONS (default_location = 'us-east1');", scope: "other", key: "default_location", value: "us-east1"},
		{sql: "ALTER PROJECT p SET OPTIONS (`default_location` = NULL)", scope: "p", key: "default_location", null: true},
		{sql: "ALTER PROJECT SET OPTIONS (`region-us.default_time_zone` = 'America/Chicago')", scope: "p", key: "region-us.default_time_zone", value: "America/Chicago"},
		{sql: "ALTER PROJECT SET OPTIONS (`region-US.default_query_job_timeout_ms` = 1800000)", scope: "p", key: "region-us.default_query_job_timeout_ms", value: "1800000"},
		{sql: "ALTER PROJECT SET OPTIONS (`region-us.default_batch_query_queue_timeout_ms` = -1)", scope: "p", key: "region-us.default_batch_query_queue_timeout_ms", value: "-1"},
		{sql: "ALTER PROJECT SET OPTIONS (`region-eu.enable_info_schema_storage` = TRUE)", scope: "p", key: "region-eu.enable_info_schema_storage", value: "true"},
		{sql: "ALTER ORGANIZATION SET OPTIONS (`default_location` = 'EU')", scope: orgScope, key: "default_location", value: "EU"},
		{sql: "ALTER ORGANIZATION SET OPTIONS (`region-us.default_data_policy_projects` = ['a', 'b'])", scope: orgScope, key: "region-us.default_data_policy_projects", value: `["a","b"]`},
		{sql: "ALTER PROJECT SET OPTIONS (`default_location` = 3)", bad: true},
		{sql: "ALTER PROJECT SET OPTIONS (`default_time_zone` = 'UTC')", bad: true},
		{sql: "ALTER PROJECT SET OPTIONS (`region-us.default_location` = 'EU')", bad: true},
		{sql: "ALTER PROJECT SET OPTIONS (`region-us.default_time_zone` = 'Mars/Olympus')", bad: true},
		{sql: "ALTER PROJECT SET OPTIONS (`region-us.default_query_job_timeout_ms` = 1000)", bad: true},
		{sql: "ALTER PROJECT SET OPTIONS (`region-us.default_max_time_travel_hours` = 50)", bad: true},
		{sql: "ALTER PROJECT SET OPTIONS (`region-us.no_such_option` = 'x')", bad: true},
		{sql: "ALTER PROJECT SET OPTIONS (`region-us.default_time_zone` = 'UTC', `region-eu.default_time_zone` = 'UTC')", bad: true},
		{sql: "ALTER PROJECT SET OPTIONS (`default_location` = 'EU', `region-eu.default_time_zone` = 'UTC')", bad: true},
		{sql: "ALTER ORGANIZATION SET OPTIONS (`region-us.enable_reservation_based_fairness` = TRUE)", bad: true},
		{sql: "ALTER PROJECT SET OPTIONS (`region-us.default_data_policy_projects` = ['a'])", bad: true},
		{sql: "IF TRUE THEN ALTER PROJECT SET OPTIONS (`default_location` = 'EU'); END IF", refused: true},
	} {
		all, _, msg, bad, ok := parseAlterProject(c.sql, "p")
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
		if msg != "" || len(all) != 1 {
			t.Errorf("%s: %+v %q", c.sql, all, msg)
			continue
		}
		v, set := all[0].sets[c.key]
		if all[0].scope != c.scope || !set || (v == nil) != c.null || v != nil && *v != c.value {
			t.Errorf("%s: %+v", c.sql, all[0])
		}
	}
	if _, _, _, _, ok := parseAlterProject("ALTER TABLE d.t SET OPTIONS (description = 'x')", "p"); ok {
		t.Error("ALTER TABLE read as ALTER PROJECT")
	}
	sql := "SELECT 1;\nALTER PROJECT SET OPTIONS (`default_location` = 'EU');\nALTER ORGANIZATION SET OPTIONS (`default_location` = 'US');\nSELECT 2"
	all, spans, msg, _, ok := parseAlterProject(sql, "p")
	if !ok || msg != "" || len(all) != 2 || len(spans) != 2 || !strings.HasPrefix(sql[spans[0][0]:spans[0][1]], "ALTER PROJECT") ||
		!strings.HasSuffix(sql[spans[1][0]:spans[1][1]], "'US')") {
		t.Errorf("script: %+v %v %q", all, spans, msg)
	}
	if _, spans, _, _, _ := parseAlterProject("ALTER PROJECT SET OPTIONS (`default_location` = 'EU');", "p"); spans != nil {
		t.Errorf("a lone statement has spans %v", spans)
	}
}

func TestEffectiveProjectOptions(t *testing.T) {
	p := &projectOptions{}
	chicago, utc, eu := "America/Chicago", "UTC", "EU"
	p.set(orgScope, "region-us.default_time_zone", &chicago)
	p.set(orgScope, "default_location", &eu)
	if got := p.effective("p", "US", "default_time_zone"); got != chicago {
		t.Errorf("organization's: %q", got)
	}
	if got := p.defaultLocation("p"); got != "EU" {
		t.Errorf("organization's default_location: %q", got)
	}
	p.set("p", "region-us.default_time_zone", &utc)
	if got := p.effective("p", "us", "default_time_zone"); got != utc {
		t.Errorf("project's wins: %q", got)
	}
	if got := p.effective("p", "eu", "default_time_zone"); got != "" {
		t.Errorf("another region: %q", got)
	}
	rows := p.effectiveList("p", "us")
	if len(rows) != 2 || rows[0] != (optionRow{"default_location", "EU", "ORGANIZATIONS", orgNumber}) ||
		rows[1] != (optionRow{"default_time_zone", "UTC", "PROJECTS", "p"}) {
		t.Errorf("effectiveList: %+v", rows)
	}
}

func TestOrgNumberIsResourceManagers(t *testing.T) {
	if orgNumber != resourcemanager.OrganizationNumber {
		t.Errorf("orgNumber %s, Resource Manager's %s", orgNumber, resourcemanager.OrganizationNumber)
	}
}

func TestTimeZone(t *testing.T) {
	for in, want := range map[string]string{"America/Chicago": "America/Chicago", "+05:30": "+05:30", "-08": "-08:00",
		"UTC+3": "+03:00", "+0530": "+05:30"} {
		if got := engineZone(in); got != want {
			t.Errorf("engineZone(%s) = %s, want %s", in, got, want)
		}
	}
	for _, z := range []string{"America/Los_Angeles", "UTC", "+05:30", "-08"} {
		if !validTimeZone(z) {
			t.Errorf("%s refused", z)
		}
	}
	if validTimeZone("Nowhere/Never") {
		t.Error("Nowhere/Never accepted")
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

func TestSchemataInTurnAndCollate(t *testing.T) {
	for sql, want := range map[string]bool{
		"CREATE SCHEMA d; SELECT * FROM `region-us`.INFORMATION_SCHEMA.SCHEMATA":                        true,
		"CREATE SCHEMA d; SELECT * FROM d.INFORMATION_SCHEMA.SCHEMATA_OPTIONS":                          true,
		"SELECT * FROM `region-us`.INFORMATION_SCHEMA.SCHEMATA; CREATE SCHEMA d":                        false,
		"SELECT * FROM `region-us`.INFORMATION_SCHEMA.SCHEMATA":                                         false,
		"CREATE TEMP TABLE x AS SELECT 1; CREATE SCHEMA d; SELECT * FROM d.INFORMATION_SCHEMA.SCHEMATA": false,
	} {
		if got := schemataInTurn(sql); got != want {
			t.Errorf("%s: %v", sql, got)
		}
	}
	got := schemaDDL("p", "d", map[string]any{"location": "US", "defaultCollation": "und:ci"})
	want := "CREATE SCHEMA `p.d`\nDEFAULT COLLATE 'und:ci'\nOPTIONS(\n  location=\"us\"\n);"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
